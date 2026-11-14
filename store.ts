// store.ts — in-memory data model: repositories, issues, PRs, labels,
// comments, and the per-repo git object store + refs.
import {
  VIEWER_EMAIL,
  VIEWER_NAME,
  buildTreeFromFiles,
  flattenTree,
  hashObject,
  mergeBase,
  parseCommit,
  type GitObject,
} from './git.ts';

export function trialNow(): number {
  const raw = process.env.MAKEFUN_TRIAL_NOW;
  if (raw) {
    const n = Number(raw);
    if (Number.isFinite(n) && n > 0) return n > 1e12 ? Math.floor(n / 1000) : Math.floor(n);
    const d = Date.parse(raw);
    if (Number.isFinite(d)) return Math.floor(d / 1000);
  }
  return 1700000000;
}

export function trialNowIso(): string {
  return new Date(trialNow() * 1000).toISOString();
}

export interface Issue {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  labels: string[];
  createdSeq: number;
  comments: Comment[];
}

export interface Comment {
  id: number;
  body: string;
  createdSeq: number;
}

export interface PullRequest {
  id: number;
  number: number;
  title: string;
  body: string;
  state: 'open' | 'closed';
  merged: boolean;
  mergedAt: string | null;
  mergeCommitSha: string | null;
  headRef: string;
  headSha: string;
  baseRef: string;
  baseSha: string;
  createdSeq: number;
}

export interface Label {
  id: number;
  name: string;
  color: string;
}

export class Repo {
  id: number;
  name: string;
  owner: string;
  description: string;
  isPrivate: boolean;
  defaultBranch = 'main';
  createdSeq: number;
  refs = new Map<string, string>(); // 'refs/heads/main' -> commit oid
  objects = new Map<string, GitObject>();
  issues = new Map<number, Issue>();
  prs = new Map<number, PullRequest>();
  labels = new Map<string, Label>();
  nextNumber = 1; // shared issue + PR numbers (GitHub behaviour)
  seq = 0;

  constructor(id: number, name: string, owner: string, description: string, isPrivate: boolean, createdSeq: number) {
    this.id = id;
    this.name = name;
    this.owner = owner;
    this.description = description;
    this.isPrivate = isPrivate;
    this.createdSeq = createdSeq;
  }

  getObject(oid: string): GitObject | undefined {
    return this.objects.get(oid);
  }

  putObject(obj: GitObject): string {
    const oid = hashObject(obj.type, obj.content);
    if (!this.objects.has(oid)) this.objects.set(oid, obj);
    return oid;
  }

  getRef(name: string): string | undefined {
    return this.refs.get(name);
  }

  setRef(name: string, oid: string): void {
    this.refs.set(name, oid);
  }

  deleteRef(name: string): void {
    this.refs.delete(name);
  }

  defaultTip(): string | undefined {
    return this.refs.get(`refs/heads/${this.defaultBranch}`);
  }

  branchTip(branch: string): string | undefined {
    return this.refs.get(`refs/heads/${branch}`);
  }

  branches(): { name: string; sha: string }[] {
    const out: { name: string; sha: string }[] = [];
    for (const [ref, sha] of this.refs) {
      if (ref.startsWith('refs/heads/')) out.push({ name: ref.slice('refs/heads/'.length), sha });
    }
    out.sort((a, b) => (a.name < b.name ? -1 : 1));
    return out;
  }

  // Commit a file change on a branch (creating the branch from the default
  // tip when it does not exist yet, or from nothing when the repo is unborn).
  commitFile(opts: {
    branch: string;
    path: string;
    content: Buffer | null; // null = delete
    message: string;
  }): { commitOid: string; blobOid: string | null; existed: boolean } {
    const branchRef = `refs/heads/${opts.branch}`;
    let tip = this.refs.get(branchRef);
    let baseFiles = new Map<string, { oid: string; mode: string }>();
    if (!tip) {
      // Branch does not exist: start from the default branch tip (or empty).
      const defTip = this.defaultTip();
      if (defTip && opts.branch !== this.defaultBranch) tip = defTip;
    }
    let existed = false;
    if (tip) {
      const commit = parseCommit(this.objects.get(tip)!.content);
      baseFiles = flattenTree(this, commit.tree);
      existed = baseFiles.has(opts.path);
    }
    const files = new Map(baseFiles);
    let blobOid: string | null = null;
    if (opts.content === null) {
      files.delete(opts.path);
    } else {
      blobOid = this.putObject({ type: 'blob', content: opts.content });
      files.set(opts.path, { oid: blobOid, mode: '100644' });
    }
    const treeOid = buildTreeFromFiles(this, files);
    const parents = tip ? [tip] : [];
    const commitContent = buildCommitContent(treeOid, parents, opts.message);
    const commitOid = hashObject('commit', commitContent);
    this.objects.set(commitOid, { type: 'commit', content: commitContent });
    this.refs.set(branchRef, commitOid);
    return { commitOid, blobOid, existed };
  }

  // List commits reachable from `tip`, newest first.
  listCommits(tip: string): { sha: string; commit: ReturnType<typeof parseCommit> }[] {
    const out: { sha: string; commit: ReturnType<typeof parseCommit> }[] = [];
    const seen = new Set<string>();
    const stack = [tip];
    while (stack.length > 0) {
      const oid = stack.pop()!;
      if (seen.has(oid)) continue;
      seen.add(oid);
      const obj = this.objects.get(oid);
      if (!obj || obj.type !== 'commit') continue;
      const parts = parseCommit(obj.content);
      out.push({ sha: oid, commit: parts });
      for (const p of parts.parents) stack.push(p);
    }
    return out;
  }

  // Squash-merge PR: result tree = base tree + head's changes vs merge-base.
  // Returns { sha } on success, or { conflict: true }.
  squashMerge(pr: PullRequest): { sha: string } | { conflict: true } {
    const baseTip = this.branchTip(pr.baseRef);
    const headTip = this.branchTip(pr.headRef);
    if (!baseTip || !headTip) return { conflict: true };
    const base = mergeBase(this, baseTip, headTip);
    const baseTree = base ? flattenTree(this, parseCommit(this.objects.get(base)!.content).tree) : new Map();
    const baseSide = flattenTree(this, parseCommit(this.objects.get(baseTip)!.content).tree);
    const headSide = flattenTree(this, parseCommit(this.objects.get(headTip)!.content).tree);
    // Conflict: a path changed on both sides (vs merge-base) with different results.
    const allPaths = new Set([...baseTree.keys(), ...baseSide.keys(), ...headSide.keys()]);
    for (const path of allPaths) {
      const mb = baseTree.get(path)?.oid;
      const bs = baseSide.get(path)?.oid;
      const hs = headSide.get(path)?.oid;
      const baseChanged = bs !== mb;
      const headChanged = hs !== mb;
      if (baseChanged && headChanged && bs !== hs) return { conflict: true };
    }
    // Apply head's changes onto base.
    const result = new Map(baseSide);
    for (const [path, file] of headSide) result.set(path, file);
    for (const path of baseTree.keys()) {
      if (!headSide.has(path) && baseSide.has(path)) {
        // deleted on head side (and not re-added on base side beyond merge-base)
        if (baseSide.get(path)?.oid === baseTree.get(path)?.oid) result.delete(path);
      }
    }
    const treeOid = buildTreeFromFiles(this, result);
    const message = `Merge pull request #${pr.number} from ${pr.headRef}`;
    const commitContent = buildCommitContent(treeOid, [baseTip], message);
    const commitOid = hashObject('commit', commitContent);
    this.objects.set(commitOid, { type: 'commit', content: commitContent });
    this.refs.set(`refs/heads/${pr.baseRef}`, commitOid);
    return { sha: commitOid };
  }

  mergeConflict(pr: PullRequest): boolean {
    const baseTip = this.branchTip(pr.baseRef);
    const headTip = this.branchTip(pr.headRef);
    if (!baseTip || !headTip) return true;
    const base = mergeBase(this, baseTip, headTip);
    const baseTree = base ? flattenTree(this, parseCommit(this.objects.get(base)!.content).tree) : new Map();
    const baseSide = flattenTree(this, parseCommit(this.objects.get(baseTip)!.content).tree);
    const headSide = flattenTree(this, parseCommit(this.objects.get(headTip)!.content).tree);
    const allPaths = new Set([...baseTree.keys(), ...baseSide.keys(), ...headSide.keys()]);
    for (const path of allPaths) {
      const mb = baseTree.get(path)?.oid;
      const bs = baseSide.get(path)?.oid;
      const hs = headSide.get(path)?.oid;
      if (bs !== mb && hs !== mb && bs !== hs) return true;
    }
    return false;
  }

  // Diff base tree vs head tree: per-file status + line counts.
  diffFiles(baseSha: string, headSha: string): {
    filename: string;
    status: 'added' | 'removed' | 'modified';
    additions: number;
    deletions: number;
    changes: number;
  }[] {
    const baseTree = flattenTree(this, parseCommit(this.objects.get(baseSha)!.content).tree);
    const headTree = flattenTree(this, parseCommit(this.objects.get(headSha)!.content).tree);
    const out: {
      filename: string;
      status: 'added' | 'removed' | 'modified';
      additions: number;
      deletions: number;
      changes: number;
    }[] = [];
    const paths = new Set([...baseTree.keys(), ...headTree.keys()]);
    for (const path of paths) {
      const b = baseTree.get(path);
      const h = headTree.get(path);
      if (!b && h) {
        const lines = this.objects.get(h.oid)?.content.toString('utf8').split('\n').length ?? 0;
        out.push({ filename: path, status: 'added', additions: lines, deletions: 0, changes: lines });
      } else if (b && !h) {
        const lines = this.objects.get(b.oid)?.content.toString('utf8').split('\n').length ?? 0;
        out.push({ filename: path, status: 'removed', additions: 0, deletions: lines, changes: lines });
      } else if (b && h && b.oid !== h.oid) {
        const oldLines = this.objects.get(b.oid)?.content.toString('utf8').split('\n') ?? [];
        const newLines = this.objects.get(h.oid)?.content.toString('utf8').split('\n') ?? [];
        const { additions, deletions } = lineDiff(oldLines, newLines);
        out.push({ filename: path, status: 'modified', additions, deletions, changes: additions + deletions });
      }
    }
    out.sort((a, b) => (a.filename < b.filename ? -1 : 1));
    return out;
  }
}

function buildCommitContent(tree: string, parents: string[], message: string): Buffer {
  const lines: string[] = [`tree ${tree}`];
  for (const p of parents) lines.push(`parent ${p}`);
  lines.push(`author ${VIEWER_NAME} <${VIEWER_EMAIL}> ${trialNow()} +0000`);
  lines.push(`committer ${VIEWER_NAME} <${VIEWER_EMAIL}> ${trialNow()} +0000`);
  lines.push('');
  lines.push(message.endsWith('\n') ? message : message + '\n');
  return Buffer.from(lines.join('\n'), 'utf8');
}

// Small LCS line diff (files are small in this contest).
export function lineDiff(oldLines: string[], newLines: string[]): { additions: number; deletions: number } {
  const m = oldLines.length;
  const n = newLines.length;
  if (m === 0) return { additions: n, deletions: 0 };
  if (n === 0) return { additions: 0, deletions: m };
  // LCS via DP (bounded by file sizes; contest files are small).
  const dp: number[][] = Array.from({ length: m + 1 }, () => new Array<number>(n + 1).fill(0));
  for (let i = m - 1; i >= 0; i -= 1) {
    for (let j = n - 1; j >= 0; j -= 1) {
      dp[i][j] = oldLines[i] === newLines[j] ? dp[i + 1][j + 1] + 1 : Math.max(dp[i + 1][j], dp[i][j + 1]);
    }
  }
  let additions = 0;
  let deletions = 0;
  let i = 0;
  let j = 0;
  while (i < m && j < n) {
    if (oldLines[i] === newLines[j]) {
      i += 1;
      j += 1;
    } else if (dp[i + 1][j] >= dp[i][j + 1]) {
      deletions += 1;
      i += 1;
    } else {
      additions += 1;
      j += 1;
    }
  }
  deletions += m - i;
  additions += n - j;
  return { additions, deletions };
}

export class Store {
  repos = new Map<string, Repo>(); // key: `${owner}/${name}`
  nextRepoId = 1;
  nextObjectId = 1; // global id counter for issues/PRs/comments
  nextLabelId = 1;
  seq = 0;

  getRepo(owner: string, name: string): Repo | undefined {
    return this.repos.get(`${owner}/${name}`);
  }

  createRepo(name: string, owner: string, description: string, isPrivate: boolean): Repo {
    const repo = new Repo(this.nextRepoId++, name, owner, description, isPrivate, ++this.seq);
    this.repos.set(`${owner}/${name}`, repo);
    return repo;
  }

  deleteRepo(owner: string, name: string): boolean {
    return this.repos.delete(`${owner}/${name}`);
  }

  listRepos(owner: string): Repo[] {
    const out: Repo[] = [];
    for (const repo of this.repos.values()) {
      if (repo.owner === owner) out.push(repo);
    }
    out.sort((a, b) => b.createdSeq - a.createdSeq);
    return out;
  }

  nextId(): number {
    return this.nextObjectId++;
  }

  // Opaque, stable GraphQL global ids.
  globalId(kind: string, ...parts: (string | number)[]): string {
    return Buffer.from(`${kind}:${parts.join(':')}`, 'utf8').toString('base64url');
  }

  parseGlobalId(id: string): { kind: string; parts: string[] } | null {
    try {
      const text = Buffer.from(id, 'base64url').toString('utf8');
      const [kind, ...parts] = text.split(':');
      if (!kind || parts.length === 0) return null;
      return { kind, parts };
    } catch {
      return null;
    }
  }

  resolveNode(id: string): { repo: Repo; issue?: Issue; pr?: PullRequest } | null {
    const parsed = this.parseGlobalId(id);
    if (!parsed) return null;
    if (parsed.kind === 'Repository') {
      const dbId = Number(parsed.parts[0]);
      for (const repo of this.repos.values()) if (repo.id === dbId) return { repo };
      return null;
    }
    if (parsed.kind === 'Issue' || parsed.kind === 'PullRequest') {
      const dbId = Number(parsed.parts[0]);
      const num = Number(parsed.parts[1]);
      for (const repo of this.repos.values()) {
        if (repo.id !== dbId) continue;
        if (parsed.kind === 'Issue') {
          const issue = repo.issues.get(num);
          if (issue) return { repo, issue };
        } else {
          const pr = repo.prs.get(num);
          if (pr) return { repo, pr };
        }
      }
      return null;
    }
    return null;
  }
}
