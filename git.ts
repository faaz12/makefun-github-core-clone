// git.ts — pure git object model, pack codec, pkt-line and smart-HTTP
// protocol (v0 + v2), all in Node stdlib. No git binary, no network.
import { createHash } from 'node:crypto';
import { deflateSync, inflateSync } from 'node:zlib';

export type ObjectType = 'commit' | 'tree' | 'blob' | 'tag';
export interface GitObject {
  type: ObjectType;
  content: Buffer;
}

export const ZERO_OID = '0'.repeat(40);
export const VIEWER_LOGIN = 'adaiversen'; // fixed scenario viewer (SCENARIO_SEED 0x6a09e667f3bcc909)
export const VIEWER_ID = 1000;
export const VIEWER_NAME = 'ada iversen';
export const VIEWER_EMAIL = 'adaiversen@example.invalid';

const TYPE_CODES: Record<ObjectType, number> = { commit: 1, tree: 2, blob: 3, tag: 4 };
const CODE_TYPES: Record<number, ObjectType> = { 1: 'commit', 2: 'tree', 3: 'blob', 4: 'tag' };

export function sha1Hex(data: Buffer): string {
  return createHash('sha1').update(data).digest('hex');
}

export function hashObject(type: ObjectType, content: Buffer): string {
  const header = Buffer.from(`${type} ${content.length}\0`, 'utf8');
  return sha1Hex(Buffer.concat([header, content]));
}

export function oidBuffer(oid: string): Buffer {
  return Buffer.from(oid, 'hex');
}

export interface CommitParts {
  tree: string;
  parents: string[];
  authorName: string;
  authorEmail: string;
  authorTs: number;
  committerName: string;
  committerEmail: string;
  committerTs: number;
  message: string;
}

export function buildCommit(opts: {
  tree: string;
  parents: string[];
  message: string;
  authorName: string;
  authorEmail: string;
  timestamp: number;
}): { oid: string; content: Buffer } {
  const lines: string[] = [`tree ${opts.tree}`];
  for (const p of opts.parents) lines.push(`parent ${p}`);
  lines.push(`author ${opts.authorName} <${opts.authorEmail}> ${opts.timestamp} +0000`);
  lines.push(`committer ${opts.authorName} <${opts.authorEmail}> ${opts.timestamp} +0000`);
  lines.push('');
  lines.push(opts.message.endsWith('\n') ? opts.message : opts.message + '\n');
  const content = Buffer.from(lines.join('\n'), 'utf8');
  return { oid: hashObject('commit', content), content };
}

export function parseCommit(content: Buffer): CommitParts {
  const text = content.toString('utf8');
  const headerEnd = text.indexOf('\n\n');
  const header = headerEnd >= 0 ? text.slice(0, headerEnd) : text;
  const message = headerEnd >= 0 ? text.slice(headerEnd + 2) : '';
  const parts: CommitParts = {
    tree: '',
    parents: [],
    authorName: '',
    authorEmail: '',
    authorTs: 0,
    committerName: '',
    committerEmail: '',
    committerTs: 0,
    message: message.replace(/\n$/, ''),
  };
  for (const line of header.split('\n')) {
    if (line.startsWith('tree ')) parts.tree = line.slice(5).trim();
    else if (line.startsWith('parent ')) parts.parents.push(line.slice(7).trim());
    else if (line.startsWith('author ') || line.startsWith('committer ')) {
      const m = line.match(/^(author|committer) (.*) <(.*)> (\d+) ([+-]\d{4})$/);
      if (m) {
        const target = m[1] === 'author' ? 'author' : 'committer';
        parts[`${target}Name` as 'authorName' | 'committerName'] = m[2];
        parts[`${target}Email` as 'authorEmail' | 'committerEmail'] = m[3];
        parts[`${target}Ts` as 'authorTs' | 'committerTs'] = Number(m[4]);
      }
    }
  }
  return parts;
}

export interface TreeEntry {
  mode: string;
  name: string;
  oid: string;
}

export function buildTree(entries: TreeEntry[]): Buffer {
  const sorted = [...entries].sort((a, b) => {
    const ka = a.mode === '40000' ? a.name + '/' : a.name;
    const kb = b.mode === '40000' ? b.name + '/' : b.name;
    return ka < kb ? -1 : ka > kb ? 1 : 0;
  });
  const chunks: Buffer[] = [];
  for (const e of sorted) {
    chunks.push(Buffer.from(`${e.mode} ${e.name}\0`, 'utf8'));
    chunks.push(oidBuffer(e.oid));
  }
  return Buffer.concat(chunks);
}

export function parseTree(content: Buffer): TreeEntry[] {
  const entries: TreeEntry[] = [];
  let pos = 0;
  while (pos < content.length) {
    const sp = content.indexOf(0x20, pos);
    const nul = content.indexOf(0x00, sp);
    const mode = content.subarray(pos, sp).toString('utf8');
    const name = content.subarray(sp + 1, nul).toString('utf8');
    const oid = content.subarray(nul + 1, nul + 21).toString('hex');
    entries.push({ mode, name, oid });
    pos = nul + 21;
  }
  return entries;
}

export interface TagParts {
  object: string;
  type: ObjectType;
  tag: string;
  tagger: string;
}

export function parseTag(content: Buffer): TagParts {
  const text = content.toString('utf8');
  const headerEnd = text.indexOf('\n\n');
  const header = headerEnd >= 0 ? text.slice(0, headerEnd) : text;
  const parts: TagParts = { object: '', type: 'commit', tag: '', tagger: '' };
  for (const line of header.split('\n')) {
    if (line.startsWith('object ')) parts.object = line.slice(7).trim();
    else if (line.startsWith('type ')) parts.type = line.slice(5).trim() as ObjectType;
    else if (line.startsWith('tag ')) parts.tag = line.slice(4).trim();
    else if (line.startsWith('tagger ')) parts.tagger = line.slice(7).trim();
  }
  return parts;
}

// ---------------------------------------------------------------------------
// pack codec
// ---------------------------------------------------------------------------

function encodeObjectHeader(type: ObjectType, size: number): Buffer {
  const code = TYPE_CODES[type];
  const bytes: number[] = [];
  let b = (code << 4) | (size & 0x0f);
  size >>= 4;
  if (size > 0) b |= 0x80;
  bytes.push(b);
  while (size > 0) {
    b = size & 0x7f;
    size >>= 7;
    if (size > 0) b |= 0x80;
    bytes.push(b);
  }
  return Buffer.from(bytes);
}

export function encodePack(objects: { oid: string; type: ObjectType; content: Buffer }[]): Buffer {
  const chunks: Buffer[] = [];
  const head = Buffer.alloc(12);
  head.write('PACK', 0, 'ascii');
  head.writeUInt32BE(2, 4);
  head.writeUInt32BE(objects.length, 8);
  chunks.push(head);
  for (const obj of objects) {
    chunks.push(encodeObjectHeader(obj.type, obj.content.length));
    chunks.push(deflateSync(obj.content, { level: 6 }));
  }
  const body = Buffer.concat(chunks);
  const trailer = Buffer.from(sha1Hex(body), 'hex');
  return Buffer.concat([body, trailer]);
}

// Inflate exactly one zlib stream starting at pos; returns data + bytes consumed.
// inflateSync ignores trailing bytes, and a truncated stream throws, so the
// minimal successful length is the exact end of the stream (binary search).
function inflateOne(buf: Buffer, pos: number): { data: Buffer; consumed: number } {
  const rest = buf.subarray(pos);
  let full: Buffer;
  try {
    full = inflateSync(rest);
  } catch {
    throw new Error('pack: bad zlib stream');
  }
  let lo = 1;
  let hi = rest.length;
  while (lo < hi) {
    const mid = (lo + hi) >> 1;
    try {
      inflateSync(rest.subarray(0, mid));
      hi = mid;
    } catch {
      lo = mid + 1;
    }
  }
  return { data: full, consumed: lo };
}

interface PackEntry {
  type: ObjectType | 'ofs-delta' | 'ref-delta';
  size: number;
  data: Buffer;
  baseOid?: string;
  baseOffset?: number;
}

function applyDelta(base: Buffer, delta: Buffer): Buffer {
  let pos = 0;
  const readDeltaVarint = (): number => {
    let value = 0;
    let shift = 0;
    while (true) {
      const b = delta[pos++];
      value |= (b & 0x7f) << shift;
      shift += 7;
      if (!(b & 0x80)) break;
    }
    return value;
  };
  const srcSize = readDeltaVarint();
  const tgtSize = readDeltaVarint();
  if (srcSize !== base.length) throw new Error('pack: delta base size mismatch');
  const out: Buffer[] = [];
  let outLen = 0;
  while (pos < delta.length) {
    const op = delta[pos++];
    if (op & 0x80) {
      let offset = 0;
      let size = 0;
      if (op & 0x01) offset = delta[pos++];
      if (op & 0x02) offset |= delta[pos++] << 8;
      if (op & 0x04) offset |= delta[pos++] << 16;
      if (op & 0x08) offset |= delta[pos++] << 24;
      if (op & 0x10) size = delta[pos++];
      if (op & 0x20) size |= delta[pos++] << 8;
      if (op & 0x40) size |= delta[pos++] << 16;
      if (size === 0) size = 0x10000;
      out.push(base.subarray(offset, offset + size));
      outLen += size;
    } else if (op !== 0) {
      out.push(delta.subarray(pos, pos + op));
      outLen += op;
      pos += op;
    } else {
      throw new Error('pack: delta opcode 0');
    }
  }
  if (outLen !== tgtSize) throw new Error('pack: delta target size mismatch');
  return Buffer.concat(out, outLen);
}

export function decodePack(
  buf: Buffer,
  resolveBase: (oid: string) => GitObject | undefined,
): Map<string, GitObject> {
  if (buf.length < 12 || buf.subarray(0, 4).toString('ascii') !== 'PACK') {
    throw new Error('pack: bad magic');
  }
  const version = buf.readUInt32BE(4);
  if (version !== 2) throw new Error(`pack: unsupported version ${version}`);
  const count = buf.readUInt32BE(8);
  let pos = 12;
  const entries: PackEntry[] = [];
  const offsets: number[] = [];
  for (let i = 0; i < count; i += 1) {
    const start = pos;
    offsets.push(start);
    const first = buf[pos++];
    const typeCode = (first >> 4) & 0x07;
    let size = first & 0x0f;
    let shift = 4;
    while (first & 0x80) {
      const b = buf[pos++];
      size |= (b & 0x7f) << shift;
      shift += 7;
      if (!(b & 0x80)) break;
    }
    if (typeCode === 6) {
      // OFS_DELTA: negative offset varint
      let b = buf[pos++];
      let off = b & 0x7f;
      while (b & 0x80) {
        b = buf[pos++];
        off = ((off + 1) << 7) | (b & 0x7f);
      }
      const { data, consumed } = inflateOne(buf, pos);
      pos += consumed;
      entries.push({ type: 'ofs-delta', size, data, baseOffset: start - off });
    } else if (typeCode === 7) {
      const baseOid = buf.subarray(pos, pos + 20).toString('hex');
      pos += 20;
      const { data, consumed } = inflateOne(buf, pos);
      pos += consumed;
      entries.push({ type: 'ref-delta', size, data, baseOid });
    } else {
      const type = CODE_TYPES[typeCode];
      if (!type) throw new Error(`pack: unknown object type ${typeCode}`);
      const { data, consumed } = inflateOne(buf, pos);
      pos += consumed;
      entries.push({ type, size, data });
    }
  }
  // Resolve entries (deltas may reference earlier/later pack entries or the store).
  const byOffset = new Map<number, GitObject>();
  const resolved = new Map<string, GitObject>();
  const resolveEntry = (idx: number, depth: number): GitObject => {
    if (depth > 64) throw new Error('pack: delta chain too deep');
    const entry = entries[idx];
    if (entry.type !== 'ofs-delta' && entry.type !== 'ref-delta') {
      const obj: GitObject = { type: entry.type as ObjectType, content: entry.data };
      const oid = hashObject(obj.type, obj.content);
      byOffset.set(offsets[idx], obj);
      resolved.set(oid, obj);
      return obj;
    }
    let base: GitObject | undefined;
    if (entry.type === 'ofs-delta') {
      base = byOffset.get(entry.baseOffset!);
      if (!base) {
        const baseIdx = offsets.indexOf(entry.baseOffset!);
        if (baseIdx < 0) throw new Error('pack: ofs-delta base not in pack');
        base = resolveEntry(baseIdx, depth + 1);
      }
    } else {
      base = resolveBase(entry.baseOid!);
      if (!base) {
        // base may be an earlier pack entry with that oid
        for (let j = 0; j < idx; j += 1) {
          if (entries[j].type !== 'ofs-delta' && entries[j].type !== 'ref-delta') {
            const cand = resolveEntry(j, depth + 1);
            if (hashObject(cand.type, cand.content) === entry.baseOid) {
              base = cand;
              break;
            }
          }
        }
      }
      if (!base) throw new Error(`pack: ref-delta base ${entry.baseOid} not found`);
    }
    const content = applyDelta(base.content, entry.data);
    const obj: GitObject = { type: base.type, content };
    const oid = hashObject(obj.type, obj.content);
    byOffset.set(offsets[idx], obj);
    resolved.set(oid, obj);
    return obj;
  };
  for (let i = 0; i < entries.length; i += 1) resolveEntry(i, 0);
  return resolved;
}

// ---------------------------------------------------------------------------
// pkt-line
// ---------------------------------------------------------------------------

export interface PktLine {
  kind: 'data' | 'flush' | 'delim' | 'end';
  data: Buffer;
}

export function pktLine(data: string | Buffer): Buffer {
  const payload = typeof data === 'string' ? Buffer.from(data, 'utf8') : data;
  const len = payload.length + 4;
  const head = Buffer.from(len.toString(16).padStart(4, '0'), 'ascii');
  return Buffer.concat([head, payload]);
}

export const FLUSH_PKT = Buffer.from('0000', 'ascii');
export const DELIM_PKT = Buffer.from('0001', 'ascii');

export function parsePktLines(buf: Buffer): PktLine[] {
  const out: PktLine[] = [];
  let pos = 0;
  while (pos + 4 <= buf.length) {
    const lenHex = buf.subarray(pos, pos + 4).toString('ascii');
    const len = parseInt(lenHex, 16);
    if (Number.isNaN(len)) break;
    if (len === 0) {
      out.push({ kind: 'flush', data: Buffer.alloc(0) });
      pos += 4;
    } else if (len === 1) {
      out.push({ kind: 'delim', data: Buffer.alloc(0) });
      pos += 4;
    } else if (len === 2) {
      out.push({ kind: 'end', data: Buffer.alloc(0) });
      pos += 4;
    } else {
      out.push({ kind: 'data', data: buf.subarray(pos + 4, pos + len) });
      pos += len;
    }
  }
  return out;
}

export function sidebandPacket(band: 1 | 2 | 3, data: Buffer): Buffer {
  const chunks: Buffer[] = [];
  let off = 0;
  const maxChunk = 65516 - 1;
  while (off < data.length) {
    const slice = data.subarray(off, Math.min(data.length, off + maxChunk));
    // Band is the raw byte value 1/2/3 (not the ASCII digit).
    const payload = Buffer.concat([Buffer.from([band]), slice]);
    chunks.push(pktLine(payload));
    off += slice.length;
  }
  return Buffer.concat(chunks);
}

// ---------------------------------------------------------------------------
// reachability / pack selection
// ---------------------------------------------------------------------------

export interface RepoObjects {
  getObject(oid: string): GitObject | undefined;
  getRef(name: string): string | undefined;
}

// Full object closure reachable from tips (commits, trees, blobs, tags).
// `stopAt` names shallow boundary commits: the client holds them and their
// trees/blobs, but NOT their parents, so the closure stops there.
export function reachableObjects(repo: RepoObjects, tips: string[], stopAt?: Set<string>): Set<string> {
  const out = new Set<string>();
  const commitQueue = [...tips];
  const seenCommits = new Set<string>();
  while (commitQueue.length > 0) {
    const oid = commitQueue.pop()!;
    if (seenCommits.has(oid)) continue;
    seenCommits.add(oid);
    const obj = repo.getObject(oid);
    if (!obj) continue;
    out.add(oid);
    if (obj.type === 'commit') {
      const parts = parseCommit(obj.content);
      // walkTree adds the root tree itself; pre-adding it here would make
      // walkTree return immediately (it skips trees already in `out`) and
      // the exclusion set would contain no blobs or subtrees at all.
      const walkTree = (treeOid: string) => {
        if (out.has(treeOid)) return;
        out.add(treeOid);
        const tree = repo.getObject(treeOid);
        if (!tree || tree.type !== 'tree') return;
        for (const e of parseTree(tree.content)) {
          if (e.mode === '40000') walkTree(e.oid);
          else out.add(e.oid);
        }
      };
      walkTree(parts.tree);
      if (stopAt && stopAt.has(oid)) continue;
      for (const p of parts.parents) commitQueue.push(p);
    } else if (obj.type === 'tag') {
      const tag = parseTag(obj.content);
      if (!seenCommits.has(tag.object)) commitQueue.push(tag.object);
    }
  }
  return out;
}

export function reachableFrom(
  repo: RepoObjects,
  tips: string[],
  exclude: Set<string>,
  depth?: number,
): {
  commits: string[];
  boundary: string[];
  objects: { oid: string; type: ObjectType; content: Buffer }[];
} {
  const seen = new Set<string>(exclude);
  const commits: string[] = [];
  const tags: string[] = [];
  const queue: { oid: string; dist: number }[] = tips.map((oid) => ({ oid, dist: 0 }));
  const visited = new Set<string>();
  // Traverse from the wants regardless of the exclude set (a have is a valid
  // anchor for finding ancestors within depth); only non-excluded commits
  // are included in the pack. A wanted TAG object is itself sent, and the
  // traversal continues into the commit it points at (tags may nest).
  while (queue.length > 0) {
    const { oid, dist } = queue.shift()!;
    if (visited.has(oid)) continue;
    visited.add(oid);
    const obj = repo.getObject(oid);
    if (!obj) continue;
    if (obj.type === 'tag') {
      if (!seen.has(oid)) tags.push(oid);
      const target = parseTag(obj.content).object;
      if (!visited.has(target)) queue.push({ oid: target, dist });
      continue;
    }
    if (obj.type !== 'commit') continue;
    if (!seen.has(oid)) commits.push(oid);
    const parts = parseCommit(obj.content);
    if (depth === undefined || dist + 1 < depth) {
      for (const p of parts.parents) queue.push({ oid: p, dist: dist + 1 });
    }
  }
  const included = new Set(commits);
  // Boundary = included commits whose parents are not all in the sent set
  // (depth cut or excluded). These are the new shallow boundary.
  const boundary: string[] = [];
  for (const oid of commits) {
    const obj = repo.getObject(oid);
    if (!obj) continue;
    const parents = parseCommit(obj.content).parents;
    if (parents.length === 0 || !parents.every((p) => included.has(p))) boundary.push(oid);
  }
  // Collect trees + blobs reachable from the included commits.
  const objects: { oid: string; type: ObjectType; content: Buffer }[] = [];
  const objSeen = new Set<string>();
  const addObj = (oid: string) => {
    if (objSeen.has(oid) || seen.has(oid)) return;
    const obj = repo.getObject(oid);
    if (!obj) return;
    objSeen.add(oid);
    objects.push({ oid, type: obj.type, content: obj.content });
  };
  const walkTree = (treeOid: string) => {
    if (seen.has(treeOid) || objSeen.has(treeOid)) return;
    addObj(treeOid);
    const obj = repo.getObject(treeOid);
    if (!obj || obj.type !== 'tree') return;
    for (const e of parseTree(obj.content)) {
      if (e.mode === '40000') walkTree(e.oid);
      else addObj(e.oid);
    }
  };
  const commitObjs: { oid: string; type: ObjectType; content: Buffer }[] = [];
  for (const oid of commits) {
    const obj = repo.getObject(oid);
    if (obj) commitObjs.push({ oid, type: obj.type, content: obj.content });
  }
  // Tag objects the client asked for (a clone wants every advertised ref,
  // tags included); their target commits are already in `commits` above.
  const tagObjs: { oid: string; type: ObjectType; content: Buffer }[] = [];
  for (const oid of tags) {
    const obj = repo.getObject(oid);
    if (obj) tagObjs.push({ oid, type: obj.type, content: obj.content });
  }
  for (const oid of commits) {
    const obj = repo.getObject(oid);
    if (!obj) continue;
    const parts = parseCommit(obj.content);
    walkTree(parts.tree);
  }
  return { commits, boundary, objects: [...commitObjs, ...tagObjs, ...objects] };
}

export function isAncestor(repo: RepoObjects, ancestor: string, tip: string): boolean {
  if (ancestor === tip) return true;
  const seen = new Set<string>();
  const stack = [tip];
  while (stack.length > 0) {
    const oid = stack.pop()!;
    if (oid === ancestor) return true;
    if (seen.has(oid)) continue;
    seen.add(oid);
    const obj = repo.getObject(oid);
    if (!obj || obj.type !== 'commit') continue;
    for (const p of parseCommit(obj.content).parents) stack.push(p);
  }
  return false;
}

export function mergeBase(repo: RepoObjects, a: string, b: string): string | undefined {
  const ancestorsA = new Set<string>();
  const stackA = [a];
  while (stackA.length > 0) {
    const oid = stackA.pop()!;
    if (ancestorsA.has(oid)) continue;
    ancestorsA.add(oid);
    const obj = repo.getObject(oid);
    if (!obj || obj.type !== 'commit') continue;
    for (const p of parseCommit(obj.content).parents) stackA.push(p);
  }
  const stackB = [b];
  const seenB = new Set<string>();
  while (stackB.length > 0) {
    const oid = stackB.pop()!;
    if (ancestorsA.has(oid)) return oid;
    if (seenB.has(oid)) continue;
    seenB.add(oid);
    const obj = repo.getObject(oid);
    if (!obj || obj.type !== 'commit') continue;
    for (const p of parseCommit(obj.content).parents) stackB.push(p);
  }
  return undefined;
}

// Flatten a tree to path -> {oid, mode}.
export function flattenTree(repo: RepoObjects, treeOid: string): Map<string, { oid: string; mode: string }> {
  const out = new Map<string, { oid: string; mode: string }>();
  const walk = (oid: string, prefix: string) => {
    const obj = repo.getObject(oid);
    if (!obj || obj.type !== 'tree') return;
    for (const e of parseTree(obj.content)) {
      const path = prefix ? `${prefix}/${e.name}` : e.name;
      if (e.mode === '40000') walk(e.oid, path);
      else out.set(path, { oid: e.oid, mode: e.mode });
    }
  };
  walk(treeOid, '');
  return out;
}

// Build nested trees bottom-up from a flat file map; stores intermediate trees
// via putObject and returns the root tree oid.
export function buildTreeFromFiles(
  repo: { getObject(oid: string): GitObject | undefined; putObject(obj: GitObject): string },
  files: Map<string, { oid: string; mode: string }>,
): string {
  const buildDirFromMap = (prefix: string, map: Map<string, { oid: string; mode: string }>): string => {
    // Build a tree for one directory from the flat file map scoped to it.
    const entries: TreeEntry[] = [];
    const subdirs = new Map<string, Map<string, { oid: string; mode: string }>>();
    for (const [path, file] of map) {
      const rest = path.slice(prefix.length);
      const slash = rest.indexOf('/');
      if (slash < 0) entries.push({ mode: file.mode, name: rest, oid: file.oid });
      else {
        const dir = rest.slice(0, slash);
        if (!subdirs.has(dir)) subdirs.set(dir, new Map());
        subdirs.get(dir)!.set(path, file);
      }
    }
    for (const [dir, sub] of subdirs) {
      const treeOid = buildDirFromMap(prefix + dir + '/', sub);
      entries.push({ mode: '40000', name: dir, oid: treeOid });
    }
    const content = buildTree(entries);
    return repo.putObject({ type: 'tree', content });
  };
  return buildDirFromMap('', files);
}
