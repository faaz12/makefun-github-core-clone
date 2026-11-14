// githttp.ts — git smart-HTTP handlers: info/refs (v0 + v2), upload-pack
// (v2 ls-refs / fetch, v0 fetch), receive-pack (v0).
import {
  DELIM_PKT,
  FLUSH_PKT,
  ZERO_OID,
  decodePack,
  encodePack,
  isAncestor,
  parseCommit,
  parsePktLines,
  parseTag,
  pktLine,
  reachableFrom,
  reachableObjects,
  sidebandPacket,
  type GitObject,
} from './git.ts';
import type { Resp } from './rest.ts';
import type { Repo } from './store.ts';

const V2_CAPS = ['fetch=shallow filter', 'ls-refs=unborn'];
const V0_UPLOAD_CAPS =
  'multi_ack thin-pack side-band-64k side-band ofs-delta shallow symref=HEAD:refs/heads/main agent=forge/1.0';
const V0_RECEIVE_CAPS = 'report-status side-band-64k delete-refs ofs-delta agent=forge/1.0';

function notFound(): Resp {
  return { status: 404, body: { message: 'Not Found' } };
}

function refLines(repo: Repo, caps: string): Buffer[] {
  const lines: Buffer[] = [];
  const refs: { name: string; oid: string }[] = [];
  for (const [ref, oid] of repo.refs) refs.push({ name: ref, oid });
  refs.sort((a, b) => (a.name < b.name ? -1 : 1));
  const headTip = repo.defaultTip();
  if (refs.length === 0) {
    lines.push(pktLine(`${ZERO_OID} capabilities^{}\0${caps}\n`));
    return lines;
  }
  let first = true;
  if (headTip) {
    const suffix = first ? `\0${caps}` : '';
    lines.push(pktLine(`${headTip} HEAD${suffix}\n`));
    first = false;
  }
  for (const { name, oid } of refs) {
    const suffix = first ? `\0${caps}` : '';
    lines.push(pktLine(`${oid} ${name}${suffix}\n`));
    first = false;
    // Annotated tag: also emit the peeled commit line.
    const obj = repo.getObject(oid);
    if (obj && obj.type === 'tag' && name.startsWith('refs/tags/')) {
      const tag = parseTag(obj.content);
      lines.push(pktLine(`${tag.object} ${name}^{}\n`));
    }
  }
  return lines;
}

export function handleInfoRefs(
  repo: Repo | undefined,
  service: 'git-upload-pack' | 'git-receive-pack',
  gitProtocolHeader: string | undefined,
): Resp {
  if (!repo) return notFound();
  const contentType =
    service === 'git-upload-pack'
      ? 'application/x-git-upload-pack-advertisement'
      : 'application/x-git-receive-pack-advertisement';
  const wantsV2 = service === 'git-upload-pack' && gitProtocolHeader?.includes('version=2');
  if (wantsV2) {
    const chunks: Buffer[] = [pktLine('version 2\n')];
    for (const cap of V2_CAPS) chunks.push(pktLine(`${cap}\n`));
    chunks.push(FLUSH_PKT);
    return { status: 200, body: Buffer.concat(chunks), headers: { 'Content-Type': contentType } };
  }
  const serviceName = service === 'git-upload-pack' ? 'git-upload-pack' : 'git-receive-pack';
  const caps = service === 'git-upload-pack' ? V0_UPLOAD_CAPS : V0_RECEIVE_CAPS;
  const chunks: Buffer[] = [pktLine(`# service=${serviceName}\n`), FLUSH_PKT, ...refLines(repo, caps), FLUSH_PKT];
  return { status: 200, body: Buffer.concat(chunks), headers: { 'Content-Type': contentType } };
}

// ---------------------------------------------------------------------------
// upload-pack
// ---------------------------------------------------------------------------

interface FetchArgs {
  wants: string[];
  haves: string[];
  shallow: string[];
  deepen?: number;
}

function parseFetchArgs(lines: { kind: string; data: Buffer }[]): FetchArgs {
  const args: FetchArgs = { wants: [], haves: [], shallow: [] };
  let pastDelim = false;
  for (const line of lines) {
    if (line.kind === 'delim') {
      pastDelim = true;
      continue;
    }
    if (line.kind !== 'data') continue;
    const text = line.data.toString('utf8').replace(/\n$/, '');
    if (!pastDelim) {
      if (text.startsWith('command=')) continue;
      continue;
    }
    if (text.startsWith('want ')) args.wants.push(text.slice(5).trim());
    else if (text.startsWith('have ')) args.haves.push(text.slice(5).trim());
    else if (text.startsWith('shallow ')) args.shallow.push(text.slice(8).trim());
    else if (text.startsWith('deepen ')) args.deepen = Number(text.slice(7).trim());
  }
  return args;
}

function lsRefsResponse(repo: Repo, lines: { kind: string; data: Buffer }[]): Buffer {
  let peel = false;
  const prefixes: string[] = [];
  let pastDelim = false;
  for (const line of lines) {
    if (line.kind === 'delim') {
      pastDelim = true;
      continue;
    }
    if (line.kind !== 'data' || !pastDelim) continue;
    const text = line.data.toString('utf8').replace(/\n$/, '');
    if (text === 'peel') peel = true;
    else if (text.startsWith('ref-prefix ')) prefixes.push(text.slice('ref-prefix '.length));
  }
  const matches = (name: string): boolean => prefixes.length === 0 || prefixes.some((p) => name.startsWith(p));
  const chunks: Buffer[] = [];
  const tip = repo.defaultTip();
  if (matches('HEAD')) {
    if (tip) chunks.push(pktLine(`${tip} HEAD symref-target:refs/heads/${repo.defaultBranch}\n`));
    else chunks.push(pktLine(`unborn HEAD symref-target:refs/heads/${repo.defaultBranch}\n`));
  }
  const refs: { name: string; oid: string }[] = [];
  for (const [ref, oid] of repo.refs) refs.push({ name: ref, oid });
  refs.sort((a, b) => (a.name < b.name ? -1 : 1));
  for (const { name, oid } of refs) {
    if (!matches(name)) continue;
    const obj = repo.getObject(oid);
    if (peel && obj && obj.type === 'tag' && name.startsWith('refs/tags/')) {
      const tag = parseTag(obj.content);
      chunks.push(pktLine(`${oid} ${name} peeled:${tag.object}\n`));
    } else {
      chunks.push(pktLine(`${oid} ${name}\n`));
    }
  }
  chunks.push(FLUSH_PKT);
  return Buffer.concat(chunks);
}

function fetchResponse(repo: Repo, args: FetchArgs, v2: boolean): Buffer {
  const chunks: Buffer[] = [];
  const exclude = reachableObjects(repo, args.haves, new Set(args.shallow));
  const { boundary, objects } = reachableFrom(repo, args.wants, exclude, args.deepen);
  const sentOids = new Set(objects.map((o) => o.oid));

  const ackLines = (): Buffer[] => {
    const out: Buffer[] = [];
    if (args.haves.length === 0) {
      out.push(pktLine('NAK\n'));
    } else {
      let acked = false;
      for (const h of args.haves) {
        if (repo.getObject(h)) {
          out.push(pktLine(`ACK ${h}\n`));
          acked = true;
        }
      }
      if (!acked) out.push(pktLine('NAK\n'));
      out.push(pktLine('ready\n'));
    }
    return out;
  };

  if (v2) {
    // Sections are separated by delim packets (0001); the packfile section
    // ends with a flush (0000). This is the real git v2 wire format, and the
    // suite's client accepts delim as a section separator.
    // The acknowledgments section is ONLY sent when the client sent `have`
    // lines: a fresh clone sends none, and git then expects the response to
    // start with shallow-info / packfile (verified against real git).
    if (args.haves.length > 0) {
      chunks.push(pktLine('acknowledgments\n'), ...ackLines(), DELIM_PKT);
    }
    if (args.deepen !== undefined) {
      const unshallow: string[] = [];
      for (const s of args.shallow) {
        const obj = repo.getObject(s);
        if (!obj || obj.type !== 'commit') continue;
        const parents = parseCommit(obj.content).parents;
        if (parents.length > 0 && parents.every((p) => sentOids.has(p))) unshallow.push(s);
      }
      if (boundary.length > 0 || unshallow.length > 0) {
        chunks.push(pktLine('shallow-info\n'));
        for (const b of boundary) chunks.push(pktLine(`shallow ${b}\n`));
        for (const u of unshallow) chunks.push(pktLine(`unshallow ${u}\n`));
        chunks.push(DELIM_PKT);
      }
    }
    if (objects.length > 0) {
      chunks.push(pktLine('packfile\n'));
      chunks.push(sidebandPacket(1, encodePack(objects)));
      chunks.push(FLUSH_PKT);
    }
  } else {
    // v0: bare ack / shallow lines, then the sideband pack, then flush.
    chunks.push(...ackLines());
    if (args.deepen !== undefined) {
      for (const b of boundary) chunks.push(pktLine(`shallow ${b}\n`));
    }
    if (objects.length > 0) {
      chunks.push(sidebandPacket(1, encodePack(objects)));
    }
    chunks.push(FLUSH_PKT);
  }
  return Buffer.concat(chunks);
}

export function handleUploadPack(repo: Repo | undefined, body: Buffer): Resp {
  if (!repo) return notFound();
  const lines = parsePktLines(body);
  const first = lines.find((l) => l.kind === 'data');
  const firstText = first ? first.data.toString('utf8').replace(/\n$/, '') : '';
  if (firstText.startsWith('command=ls-refs')) {
    const out = lsRefsResponse(repo, lines);
    return { status: 200, body: out, headers: { 'Content-Type': 'application/x-git-upload-pack-result' } };
  }
  if (firstText.startsWith('command=fetch')) {
    const args = parseFetchArgs(lines);
    const out = fetchResponse(repo, args, true);
    return { status: 200, body: out, headers: { 'Content-Type': 'application/x-git-upload-pack-result' } };
  }
  // v0 fetch: want/have lines.
  const args: FetchArgs = { wants: [], haves: [], shallow: [] };
  for (const line of lines) {
    if (line.kind !== 'data') continue;
    const text = line.data.toString('utf8').replace(/\n$/, '');
    const nul = text.indexOf('\0');
    const clean = nul >= 0 ? text.slice(0, nul) : text;
    if (clean.startsWith('want ')) args.wants.push(clean.slice(5).trim());
    else if (clean.startsWith('have ')) args.haves.push(clean.slice(5).trim());
    else if (clean.startsWith('shallow ')) args.shallow.push(clean.slice(8).trim());
    else if (clean.startsWith('deepen ')) args.deepen = Number(clean.slice(7).trim());
  }
  const out = fetchResponse(repo, args, false);
  return { status: 200, body: out, headers: { 'Content-Type': 'application/x-git-upload-pack-result' } };
}

// ---------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------

interface PushCommand {
  oldOid: string;
  newOid: string;
  ref: string;
}

export function handleReceivePack(repo: Repo | undefined, body: Buffer): Resp {
  if (!repo) return notFound();
  const lines = parsePktLines(body);
  const commands: PushCommand[] = [];
  const clientCaps = new Set<string>();
  let packStart = body.length;
  // Find the flush that ends the command section; the pack follows it.
  let pos = 0;
  for (const line of lines) {
    if (line.kind === 'flush') {
      // pack starts right after this flush packet (4 bytes).
      packStart = pos + 4;
      break;
    }
    if (line.kind === 'data') {
      const text = line.data.toString('utf8').replace(/\n$/, '');
      const nul = text.indexOf('\0');
      if (nul >= 0 && commands.length === 0) {
        // Capabilities ride on the first command line, after the NUL.
        for (const cap of text.slice(nul + 1).split(' ')) {
          if (cap) clientCaps.add(cap.split('=')[0]);
        }
      }
      const clean = nul >= 0 ? text.slice(0, nul) : text;
      const m = clean.match(/^([0-9a-f]{40}) ([0-9a-f]{40}) (.+)$/);
      if (m) commands.push({ oldOid: m[1], newOid: m[2], ref: m[3] });
    }
    pos += line.kind === 'data' ? line.data.length + 4 : 4;
  }
  // Unpack the pack (if any) and store objects.
  const packBytes = body.subarray(packStart);
  let unpackOk = true;
  if (packBytes.length > 0) {
    try {
      const objects = decodePack(packBytes, (oid) => repo.getObject(oid));
      for (const [oid, obj] of objects) {
        if (!repo.objects.has(oid)) repo.objects.set(oid, obj as GitObject);
      }
    } catch {
      unpackOk = false;
    }
  }
  // Apply commands.
  const results: { ref: string; ok: boolean; reason?: string }[] = [];
  for (const cmd of commands) {
    if (cmd.newOid === ZERO_OID) {
      // Delete.
      if (cmd.oldOid !== ZERO_OID) {
        const cur = repo.refs.get(cmd.ref);
        if (cur !== cmd.oldOid) {
          results.push({ ref: cmd.ref, ok: false, reason: 'failed to delete' });
          continue;
        }
      }
      repo.refs.delete(cmd.ref);
      results.push({ ref: cmd.ref, ok: true });
      continue;
    }
    const newObj = repo.getObject(cmd.newOid);
    const isTagRef = cmd.ref.startsWith('refs/tags/');
    // Branch refs point at commits; tag refs point at tag objects (or,
    // for lightweight tags, commits). A tag object is not a commit, so the
    // commit-only check below used to refuse every annotated-tag push.
    if (!newObj || (isTagRef ? (newObj.type !== 'commit' && newObj.type !== 'tag') : newObj.type !== 'commit')) {
      results.push({ ref: cmd.ref, ok: false, reason: 'object not found' });
      continue;
    }
    if (cmd.oldOid === ZERO_OID) {
      // Create.
      repo.refs.set(cmd.ref, cmd.newOid);
      results.push({ ref: cmd.ref, ok: true });
      continue;
    }
    if (isTagRef) {
      // Tags don't fast-forward; an update only needs the old value to match.
      const cur = repo.refs.get(cmd.ref);
      if (cur !== cmd.oldOid) {
        results.push({ ref: cmd.ref, ok: false, reason: 'non-fast-forward' });
        continue;
      }
      repo.refs.set(cmd.ref, cmd.newOid);
      results.push({ ref: cmd.ref, ok: true });
      continue;
    }
    const cur = repo.refs.get(cmd.ref);
    if (cur !== cmd.oldOid) {
      results.push({ ref: cmd.ref, ok: false, reason: 'non-fast-forward' });
      continue;
    }
    if (!isAncestor(repo, cmd.oldOid, cmd.newOid)) {
      results.push({ ref: cmd.ref, ok: false, reason: 'non-fast-forward' });
      continue;
    }
    repo.refs.set(cmd.ref, cmd.newOid);
    results.push({ ref: cmd.ref, ok: true });
  }
  // Report: each report line is itself a pkt-line, and the report stream
  // ends with a flush. With side-band-64k the pkt-lined report (including
  // its inner flush) rides inside band-1 sideband packets, followed by the
  // outer flush; without it the pkt-lines go directly. (Matches real git's
  // receive-pack byte for byte; the inner flush is what the client's report
  // reader waits for.)
  const reportLines: Buffer[] = [pktLine(unpackOk ? 'unpack ok\n' : 'unpack failed\n')];
  for (const r of results) {
    reportLines.push(pktLine(r.ok ? `ok ${r.ref}\n` : `ng ${r.ref} ${r.reason ?? 'failed'}\n`));
  }
  const inner = Buffer.concat([...reportLines, FLUSH_PKT]);
  const out = clientCaps.has('side-band-64k')
    ? Buffer.concat([sidebandPacket(1, inner), FLUSH_PKT])
    : inner;
  return { status: 200, body: out, headers: { 'Content-Type': 'application/x-git-receive-pack-result' } };
}
