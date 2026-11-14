// rest.ts — GitHub REST v3 surface: repos, refs, branches, commits,
// contents, labels, issues, comments, pull requests.
import { VIEWER_EMAIL, VIEWER_ID, VIEWER_LOGIN, VIEWER_NAME, flattenTree, parseCommit } from './git.ts';
import { trialNowIso, type Comment, type Issue, type Label, type PullRequest, type Repo, type Store } from './store.ts';

export interface Ctx {
  store: Store;
  method: string;
  url: URL;
  params: Record<string, string>;
  body: unknown;
  baseUrl: string;
}

export interface Resp {
  status: number;
  body: unknown;
  headers?: Record<string, string>;
}

const json = (status: number, body: unknown, headers?: Record<string, string>): Resp => ({ status, body, headers });
const notFound = (msg = 'Not Found'): Resp => json(404, { message: msg });
const unprocessable = (msg: string): Resp => json(422, { message: msg });
const badRequest = (msg: string): Resp => json(400, { message: msg });

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function bodyOf(ctx: Ctx): Record<string, unknown> {
  return isRecord(ctx.body) ? ctx.body : {};
}

function str(v: unknown, fallback = ''): string {
  return typeof v === 'string' ? v : fallback;
}

function bool(v: unknown, fallback = false): boolean {
  return typeof v === 'boolean' ? v : fallback;
}

// ---------------------------------------------------------------------------
// JSON shapes
// ---------------------------------------------------------------------------

export function userJson(): Record<string, unknown> {
  return {
    login: VIEWER_LOGIN,
    id: VIEWER_ID,
    node_id: 'U_kgDOAAAD6A',
    type: 'User',
    name: VIEWER_NAME,
    email: VIEWER_EMAIL,
    html_url: `https://example.invalid/${VIEWER_LOGIN}`,
    url: `/api/v3/users/${VIEWER_LOGIN}`,
    site_admin: false,
  };
}

export function repoJson(repo: Repo, baseUrl: string): Record<string, unknown> {
  const now = trialNowIso();
  return {
    id: repo.id,
    node_id: `R_kgDO${repo.id}`,
    name: repo.name,
    full_name: `${repo.owner}/${repo.name}`,
    owner: { login: repo.owner, id: VIEWER_ID, type: 'User' },
    private: repo.isPrivate,
    description: repo.description,
    fork: false,
    html_url: `https://example.invalid/${repo.owner}/${repo.name}`,
    url: `${baseUrl}/api/v3/repos/${repo.owner}/${repo.name}`,
    created_at: now,
    updated_at: now,
    pushed_at: now,
    default_branch: repo.defaultBranch,
    has_issues: true,
    has_projects: false,
    has_wiki: false,
    has_pages: false,
    open_issues_count: [...repo.issues.values()].filter((i) => i.state === 'open').length,
    watchers_count: 0,
    forks_count: 0,
    stargazers_count: 0,
    size: 0,
    language: null,
    topics: [],
    visibility: repo.isPrivate ? 'private' : 'public',
    archived: false,
    disabled: false,
  };
}

export function labelJson(label: Label): Record<string, unknown> {
  return { id: label.id, name: label.name, color: label.color, description: '', default: false };
}

export function issueJson(repo: Repo, issue: Issue): Record<string, unknown> {
  const now = trialNowIso();
  return {
    id: issue.id,
    number: issue.number,
    title: issue.title,
    body: issue.body,
    state: issue.state,
    labels: issue.labels.map((name) => {
      const l = repo.labels.get(name);
      return { name, color: l ? l.color : 'ededed', description: '' };
    }),
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    assignees: [],
    created_at: now,
    updated_at: now,
    closed_at: issue.state === 'closed' ? now : null,
    comments: issue.comments.length,
    html_url: `https://example.invalid/${repo.owner}/${repo.name}/issues/${issue.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/issues/${issue.number}`,
  };
}

export function commentJson(repo: Repo, issue: Issue, comment: Comment): Record<string, unknown> {
  const now = trialNowIso();
  return {
    id: comment.id,
    body: comment.body,
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    created_at: now,
    updated_at: now,
    html_url: `https://example.invalid/${repo.owner}/${repo.name}/issues/${issue.number}#issuecomment-${comment.id}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/issues/comments/${comment.id}`,
  };
}

export function prJson(repo: Repo, pr: PullRequest): Record<string, unknown> {
  const now = trialNowIso();
  return {
    id: pr.id,
    number: pr.number,
    title: pr.title,
    body: pr.body,
    state: pr.state,
    locked: false,
    title_draft: false,
    user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
    head: {
      ref: pr.headRef,
      sha: pr.headSha,
      label: `${VIEWER_LOGIN}:${pr.headRef}`,
      user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
      repo: repoJson(repo, ''),
    },
    base: {
      ref: pr.baseRef,
      sha: pr.baseSha,
      label: `${VIEWER_LOGIN}:${pr.baseRef}`,
      user: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
      repo: repoJson(repo, ''),
    },
    merged: pr.merged,
    mergeable: true,
    merged_at: pr.mergedAt,
    merge_commit_sha: pr.mergeCommitSha,
    created_at: now,
    updated_at: now,
    closed_at: pr.state === 'closed' ? now : null,
    html_url: `https://example.invalid/${repo.owner}/${repo.name}/pull/${pr.number}`,
    url: `/api/v3/repos/${repo.owner}/${repo.name}/pulls/${pr.number}`,
  };
}

function refJson(repo: Repo, ref: string, sha: string): Record<string, unknown> {
  return {
    ref,
    object: { sha, type: 'commit', url: `/api/v3/repos/${repo.owner}/${repo.name}/git/commits/${sha}` },
    url: `/api/v3/repos/${repo.owner}/${repo.name}/git/refs/${ref}`,
  };
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

function resolveRepo(ctx: Ctx): Repo | null {
  const owner = ctx.params.owner;
  const name = ctx.params.repo;
  if (!owner || !name) return null;
  if (owner !== VIEWER_LOGIN) return null;
  return ctx.store.getRepo(owner, name) ?? null;
}

function pagination(ctx: Ctx): { page: number; perPage: number } {
  const page = Math.max(1, Number(ctx.url.searchParams.get('page') ?? '1') || 1);
  const perPage = Math.min(100, Math.max(1, Number(ctx.url.searchParams.get('per_page') ?? '30') || 30));
  return { page, perPage };
}

function linkHeader(ctx: Ctx, page: number, perPage: number, total: number): string | null {
  const totalPages = Math.max(1, Math.ceil(total / perPage));
  const parts: string[] = [];
  const make = (p: number, rel: string) => {
    const u = new URL(ctx.url.toString());
    u.searchParams.set('page', String(p));
    u.searchParams.set('per_page', String(perPage));
    parts.push(`<${u.toString()}>; rel="${rel}"`);
  };
  if (page < totalPages) make(page + 1, 'next');
  if (page > 1) make(page - 1, 'prev');
  make(1, 'first');
  make(totalPages, 'last');
  return parts.length > 0 ? parts.join(', ') : null;
}

function paginate<T>(items: T[], page: number, perPage: number): T[] {
  return items.slice((page - 1) * perPage, page * perPage);
}

// ---------------------------------------------------------------------------
// root / meta / user
// ---------------------------------------------------------------------------

export function apiRoot(ctx: Ctx): Resp {
  return json(200, {
    current_user_url: `${ctx.baseUrl}/api/v3/user`,
    repository_url: `${ctx.baseUrl}/api/v3/repos/{owner}/{repo}`,
    user_url: `${ctx.baseUrl}/api/v3/users/{user}`,
    meta: `${ctx.baseUrl}/api/v3/meta`,
  });
}

export function meta(ctx: Ctx): Resp {
  void ctx;
  return json(200, { installed_version: '1.0.0' });
}

export function getUser(ctx: Ctx): Resp {
  void ctx;
  return json(200, userJson());
}

// ---------------------------------------------------------------------------
// repos
// ---------------------------------------------------------------------------

export function createUserRepo(ctx: Ctx): Resp {
  const b = bodyOf(ctx);
  const name = str(b.name).trim();
  if (!name) return badRequest('name is required');
  const existing = ctx.store.getRepo(VIEWER_LOGIN, name);
  if (existing) return unprocessable(`A repository named ${name} already exists`);
  const repo = ctx.store.createRepo(name, VIEWER_LOGIN, str(b.description), bool(b.private));
  return json(201, repoJson(repo, ctx.baseUrl));
}

export function listUserRepos(ctx: Ctx): Resp {
  const { page, perPage } = pagination(ctx);
  const repos = ctx.store.listRepos(VIEWER_LOGIN);
  const link = linkHeader(ctx, page, perPage, repos.length);
  const headers: Record<string, string> = {};
  if (link) headers.Link = link;
  return json(200, paginate(repos, page, perPage).map((r) => repoJson(r, ctx.baseUrl)), headers);
}

export function getRepo(ctx: Ctx): Resp {
  const repo = resolveRepo(ctx);
  if (!repo) return notFound('Repository not found');
  return json(200, repoJson(repo, ctx.baseUrl));
}

export function patchRepo(ctx: Ctx): Resp {
  const repo = resolveRepo(ctx);
  if (!repo) return notFound('Repository not found');
  const b = bodyOf(ctx);
  if (typeof b.description === 'string') repo.description = b.description;
  if (typeof b.private === 'boolean') repo.isPrivate = b.private;
  if (typeof b.default_branch === 'string' && b.default_branch) repo.defaultBranch = b.default_branch;
  return json(200, repoJson(repo, ctx.baseUrl));
}

export function deleteRepo(ctx: Ctx): Resp {
  const repo = resolveRepo(ctx);
  if (!repo) return notFound('Repository not found');
  ctx.store.deleteRepo(repo.owner, repo.name);
  return { status: 204, body: null };
}

// ---------------------------------------------------------------------------
// git refs / branches / commits
// ---------------------------------------------------------------------------

export function createRef(ctx: Ctx): Resp {
  const repo = resolveRepo(ctx);
  if (!repo) return notFound('Repository not found');
  const b = bodyOf(ctx);
  const ref = str(b.ref);
  const sha = str(b.sha);
  if (!ref || !sha) return badRequest('ref and sha are required');
  if (!/^refs\/(heads|tags)\/.+/.test(ref)) return unprocessable('ref must be refs/heads/<name> or refs/tags/<name>');
  if (!/^[0-9a-f]{40}$/.test(sha)) return unprocessable('sha is not a valid object');
  if (repo.refs.has(ref)) return unprocessable(`Reference ${ref} already exists`);
  const obj = repo.getObject(sha);
  if (!obj || obj.type !== 'commit') return unprocessable('sha does not reference a commit');
  repo.refs.set(ref, sha);
  return json(201, refJson(repo, ref, sha));
}

export function getRef(ctx: Ctx): Resp {
  const repo = resolveRepo(ctx);
  if (!repo) return notFound('Repository not found');
  const ref = `refs/${ctx.params.path}`;
  const sha = repo.refs.get(ref);
  if (!sha) return notFound('Reference not found');
  return json(200, refJson(repo, ref, sha));
}

export function listBranches(ctx: Ctx): Resp {
  const repo = resolveRepo(ctx);
  if (!repo) return notFound('Repository not found');
  return json(200, repo.branches().map((b) => ({ name: b.name, commit: { sha: b.sha }, protected: false })));
}

export function getBranch(ctx: Ctx): Resp {
  const repo = resolveRepo(ctx);
  if (!repo) return notFound('Repository not found');
  const sha = repo.branchTip(ctx.params.branch);
  if (!sha) return notFound('Branch not found');
  return json(200, { name: ctx.params.branch, commit: { sha }, protected: false });
}

export function listCommits(ctx: Ctx): Resp {
  const repo = resolveRepo(ctx);
  if (!repo) return notFound('Repository not found');
  const shaParam = ctx.url.searchParams.get('sha');
  let tip: string | undefined;
  if (shaParam) {
    if (/^[0-9a-f]{40}$/.test(shaParam)) tip = shaParam;
    else tip = repo.branchTip(shaParam);
    if (!tip) return notFound('Reference not found');
  } else {
    tip = repo.defaultTip();
    if (!tip) return json(200, []);
  }
  const commits = repo.listCommits(tip);
  const { page, perPage } = pagination(ctx);
  return json(
    200,
    paginate(commits, page, perPage).map((c) => ({
      sha: c.sha,
      commit: {
        message: c.commit.message,
        author: { name: c.commit.authorName, email: c.commit.authorEmail, date: new Date(c.commit.authorTs * 1000).toISOString() },
        committer: { name: c.commit.committerName, email: c.commit.committerEmail, date: new Date(c.commit.committerTs * 1000).toISOString() },
        tree: { sha: c.commit.tree },
      },
      author: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
      committer: { login: VIEWER_LOGIN, id: VIEWER_ID, type: 'User' },
      parents: c.commit.parents.map((p) => ({ sha: p })),
      url: `/api/v3/repos/${repo.owner}/${repo.name}/commits/${c.sha}`,
    })),
  );
}

// ---------------------------------------------------------------------------
// contents
// ---------------------------------------------------------------------------

function readTreeAt(repo: Repo, refParam: string | null): Map<string, { oid: string; mode: string }> | null {
  let tip: string | undefined;
  if (refParam) {
    if (/^[0-9a-f]{40}$/.test(refParam)) tip = refParam;
    else tip = repo.branchTip(refParam);
    if (!tip) return null;
  } else {
    tip = repo.defaultTip();
    if (!tip) return new Map();
  }
  const obj = repo.getObject(tip);
  if (!obj || obj.type !== 'commit') return null;
  const parts = parseCommit(obj.content);
  return flattenTree(repo, parts.tree);
}

export function getContents(ctx: Ctx): Resp {
  const repo = resolveRepo(ctx);
  if (!repo) return notFound('Repository not found');
  const path = ctx.params.path ?? '';
  const refParam = ctx.url.searchParams.get('ref');
  const files = readTreeAt(repo, refParam);
  if (files === null) return notFound('Reference not found');
  if (path === '') {
    // Root listing.
    const entries: Record<string, unknown>[] = [];
    const dirs = new Set<string>();
    for (const [p, f] of files) {
      const slash = p.indexOf('/');
      if (slash < 0) {
        entries.push({ type: 'file', name: p, path: p, sha: f.oid, size: repo.getObject(f.oid)?.content.length ?? 0, url: `/api/v3/repos/${repo.owner}/${repo.name}/contents/${p}` });
      } else {
        dirs.add(p.slice(0, slash));
      }
    }
    for (const d of dirs) {
      entries.push({ type: 'dir', name: d, path: d, sha: '', size: 0, url: `/api/v3/repos/${repo.owner}/${repo.name}/contents/${d}` });
    }
    entries.sort((a, b) => String(a.name).localeCompare(String(b.name)));
    return json(200, entries);
  }
  const file = files.get(path);
  if (file) {
    const content = repo.getObject(file.oid)?.content ?? Buffer.alloc(0);
    return json(200, {
      type: 'file',
      encoding: 'base64',
      size: content.length,
      name: path.split('/').pop(),
      path,
      content: content.toString('base64'),
      sha: file.oid,
      url: `/api/v3/repos/${repo.owner}/${repo.name}/contents/${path}`,
    });
  }
  // Directory?
  const prefix = path + '/';
  const children: Record<string, unknown>[] = [];
  const dirs = new Set<string>();
  for (const [p, f] of files) {
    if (!p.startsWith(prefix)) continue;
    const rest = p.slice(prefix.length);
    const slash = rest.indexOf('/');
    if (slash < 0) {
      children.push({ type: 'file', name: rest, path: p, sha: f.oid, size: repo.getObject(f.oid)?.content.length ?? 0, url: `/api/v3/repos/${repo.owner}/${repo.name}/contents/${p}` });
    } else {
      dirs.add(rest.slice(0, slash));
    }
  }
  if (children.length > 0 || dirs.size > 0) {
    for (const d of dirs) {
      children.push({ type: 'dir', name: d, path: `${path}/${d}`, sha: '', size: 0, url: `/api/v3/repos/${repo.owner}/${repo.name}/contents/${path}/${d}` });
    }
    children.sort((a, b) => String(a.name).localeCompare(String(b.name)));
    return json(200, children);
  }
  return notFound('Not Found');
}

export function putContents(ctx: Ctx): Resp {
  const repo = resolveRepo(ctx);
  if (!repo) return notFound('Repository not found');
  const path = ctx.params.path ?? '';
  if (!path) return badRequest('path is required');
  const b = bodyOf(ctx);
  const message = str(b.message);
  const contentB64 = str(b.content);
  if (!message) return badRequest('message is required');
  if (!contentB64) return badRequest('content is required');
  let content: Buffer;
  try {
    content = Buffer.from(contentB64, 'base64');
  } catch {
    return badRequest('content is not valid base64');
  }
  const branch = str(b.branch, repo.defaultBranch) || repo.defaultBranch;
  const sha = str(b.sha);
  // Stale-sha check.
  const tip = repo.branchTip(branch) ?? repo.defaultTip();
  if (sha && tip) {
    const obj = repo.getObject(tip);
    if (obj && obj.type === 'commit') {
      const files = flattenTree(repo, parseCommit(obj.content).tree);
      const cur = files.get(path);
      if (!cur) return unprocessable('sha does not match the current file');
      if (cur.oid !== sha) return json(409, { message: 'sha does not match the current file' });
    }
  }
  const result = repo.commitFile({ branch, path, content, message });
  const status = result.existed ? 200 : 201;
  return json(status, {
    content: {
      name: path.split('/').pop(),
      path,
      sha: result.blobOid,
      size: content.length,
      type: 'file',
      encoding: 'base64',
    },
    commit: {
      sha: result.commitOid,
      message,
      author: { name: VIEWER_NAME, email: VIEWER_EMAIL, date: trialNowIso() },
      committer: { name: VIEWER_NAME, email: VIEWER_EMAIL, date: trialNowIso() },
    },
  });
}

export function deleteContents(ctx: Ctx): Resp {
  const repo = resolveRepo(ctx);
  if (!repo) return notFound('Repository not found');
  const path = ctx.params.path ?? '';
  if (!path) return badRequest('path is required');
  const b = bodyOf(ctx);
  const message = str(b.message);
  if (!message) return badRequest('message is required');
  const branch = str(b.branch, repo.defaultBranch) || repo.defaultBranch;
  const sha = str(b.sha);
  const tip = repo.branchTip(branch) ?? repo.defaultTip();
  if (!tip) return notFound('Not Found');
  const obj = repo.getObject(tip);
  if (!obj || obj.type !== 'commit') return notFound('Not Found');
  const files = flattenTree(repo, parseCommit(obj.content).tree);
  const cur = files.get(path);
  if (!cur) return notFound('Not Found');
  if (sha && cur.oid !== sha) return json(409, { message: 'sha does not match the current file' });
  const result = repo.commitFile({ branch, path, content: null, message });
  return json(200, {
    commit: {
      sha: result.commitOid,
      message,
      author: { name: VIEWER_NAME, email: VIEWER_EMAIL, date: trialNowIso() },
      committer: { name: VIEWER_NAME, email: VIEWER_EMAIL, date: trialNowIso() },
    },
  });
}

// ---------------------------------------------------------------------------
// labels
// ---------------------------------------------------------------------------

export function createLabel(ctx: Ctx): Resp {
  const repo = resolveRepo(ctx);
  if (!repo) return notFound('Repository not found');
  const b = bodyOf(ctx);
  const name = str(b.name).trim();
  if (!name) return badRequest('name is required');
  const color = str(b.color, 'ededed').replace(/^#/, '');
  const existing = repo.labels.get(name);
  if (existing) return json(201, labelJson(existing));
  const label: Label = { id: ctx.store.nextLabelId++, name, color };
  repo.labels.set(name, label);
  return json(201, labelJson(label));
}

export function listLabels(ctx: Ctx): Resp {
  const repo = resolveRepo(ctx);
  if (!repo) return notFound('Repository not found');
  return json(200, [...repo.labels.values()].map(labelJson));
}

// ---------------------------------------------------------------------------
// issues
// ---------------------------------------------------------------------------

function normalizeLabels(v: unknown): string[] {
  if (!Array.isArray(v)) return [];
  const out: string[] = [];
  for (const item of v) {
    if (typeof item === 'string') out.push(item);
    else if (isRecord(item) && typeof item.name === 'string') out.push(item.name);
  }
  return out;
}

export function createIssue(ctx: Ctx): Resp {
  const repo = resolveRepo(ctx);
  if (!repo) return notFound('Repository not found');
  const b = bodyOf(ctx);
  const title = str(b.title).trim();
  if (!title) return badRequest('title is required');
  const labels = normalizeLabels(b.labels);
  for (const name of labels) {
    if (!repo.labels.has(name)) return unprocessable(`Label '${name}' does not exist`);
  }
  const number = repo.nextNumber++;
  const issue: Issue = {
    id: ctx.store.nextId(),
    number,
    title,
    body: str(b.body),
    state: 'open',
    labels,
    createdSeq: ++repo.seq,
    comments: [],
  };
  repo.issues.set(number, issue);
  return json(201, issueJson(repo, issue));
}

export function listIssues(ctx: Ctx): Resp {
  const repo = resolveRepo(ctx);
  if (!repo) return notFound('Repository not found');
  const state = ctx.url.searchParams.get('state') ?? 'open';
  const sort = ctx.url.searchParams.get('sort') ?? 'created';
  const direction = ctx.url.searchParams.get('direction') ?? 'desc';
  let issues = [...repo.issues.values()];
  if (state !== 'all') issues = issues.filter((i) => i.state === state);
  if (sort === 'created') {
    issues.sort((a, b) => (direction === 'asc' ? a.createdSeq - b.createdSeq : b.createdSeq - a.createdSeq));
  }
  const { page, perPage } = pagination(ctx);
  const link = linkHeader(ctx, page, perPage, issues.length);
  const headers: Record<string, string> = {};
  if (link) headers.Link = link;
  return json(200, paginate(issues, page, perPage).map((i) => issueJson(repo, i)), headers);
}

export function getIssue(ctx: Ctx): Resp {
  const repo = resolveRepo(ctx);
  if (!repo) return notFound('Repository not found');
  const issue = repo.issues.get(Number(ctx.params.number));
  if (!issue) return notFound('Issue not found');
  return json(200, issueJson(repo, issue));
}

export function patchIssue(ctx: Ctx): Resp {
  const repo = resolveRepo(ctx);
  if (!repo) return notFound('Repository not found');
  const issue = repo.issues.get(Number(ctx.params.number));
  if (!issue) return notFound('Issue not found');
  const b = bodyOf(ctx);
  if (typeof b.title === 'string') issue.title = b.title;
  if (typeof b.body === 'string') issue.body = b.body;
  if (b.state === 'open' || b.state === 'closed') issue.state = b.state;
  if (b.labels !== undefined) {
    const labels = normalizeLabels(b.labels);
    for (const name of labels) {
      if (!repo.labels.has(name)) return unprocessable(`Label '${name}' does not exist`);
    }
    issue.labels = labels;
  }
  return json(200, issueJson(repo, issue));
}

export function createIssueComment(ctx: Ctx): Resp {
  const repo = resolveRepo(ctx);
  if (!repo) return notFound('Repository not found');
  const issue = repo.issues.get(Number(ctx.params.number));
  if (!issue) return notFound('Issue not found');
  const b = bodyOf(ctx);
  const body = str(b.body).trim();
  if (!body) return badRequest('body is required');
  const comment: Comment = { id: ctx.store.nextId(), body, createdSeq: ++repo.seq };
  issue.comments.push(comment);
  return json(201, commentJson(repo, issue, comment));
}

export function listIssueComments(ctx: Ctx): Resp {
  const repo = resolveRepo(ctx);
  if (!repo) return notFound('Repository not found');
  const issue = repo.issues.get(Number(ctx.params.number));
  if (!issue) return notFound('Issue not found');
  return json(200, issue.comments.map((c) => commentJson(repo, issue, c)));
}

// ---------------------------------------------------------------------------
// pull requests
// ---------------------------------------------------------------------------

export function createPullRequest(ctx: Ctx): Resp {
  const repo = resolveRepo(ctx);
  if (!repo) return notFound('Repository not found');
  const b = bodyOf(ctx);
  const title = str(b.title).trim();
  const head = str(b.head).trim();
  const base = str(b.base).trim();
  if (!title) return badRequest('title is required');
  if (!head || !base) return badRequest('head and base are required');
  const headSha = repo.branchTip(head);
  const baseSha = repo.branchTip(base);
  if (!headSha) return unprocessable(`head branch ${head} does not exist`);
  if (!baseSha) return unprocessable(`base branch ${base} does not exist`);
  const number = repo.nextNumber++;
  const pr: PullRequest = {
    id: ctx.store.nextId(),
    number,
    title,
    body: str(b.body),
    state: 'open',
    merged: false,
    mergedAt: null,
    mergeCommitSha: null,
    headRef: head,
    headSha,
    baseRef: base,
    baseSha,
    createdSeq: ++repo.seq,
  };
  repo.prs.set(number, pr);
  return json(201, prJson(repo, pr));
}

export function listPullRequests(ctx: Ctx): Resp {
  const repo = resolveRepo(ctx);
  if (!repo) return notFound('Repository not found');
  const state = ctx.url.searchParams.get('state') ?? 'open';
  let prs = [...repo.prs.values()];
  if (state !== 'all') prs = prs.filter((p) => p.state === state);
  prs.sort((a, b) => b.createdSeq - a.createdSeq);
  const { page, perPage } = pagination(ctx);
  return json(200, paginate(prs, page, perPage).map((p) => prJson(repo, p)));
}

export function getPullRequest(ctx: Ctx): Resp {
  const repo = resolveRepo(ctx);
  if (!repo) return notFound('Repository not found');
  const pr = repo.prs.get(Number(ctx.params.number));
  if (!pr) return notFound('Pull request not found');
  return json(200, prJson(repo, pr));
}

export function listPullRequestFiles(ctx: Ctx): Resp {
  const repo = resolveRepo(ctx);
  if (!repo) return notFound('Repository not found');
  const pr = repo.prs.get(Number(ctx.params.number));
  if (!pr) return notFound('Pull request not found');
  const files = repo.diffFiles(pr.baseSha, pr.headSha);
  return json(
    200,
    files.map((f) => ({
      sha: '',
      filename: f.filename,
      status: f.status,
      additions: f.additions,
      deletions: f.deletions,
      changes: f.changes,
      blob_url: '',
      raw_url: '',
      contents_url: '',
      patch: '',
    })),
  );
}

export function mergePullRequest(ctx: Ctx): Resp {
  const repo = resolveRepo(ctx);
  if (!repo) return notFound('Repository not found');
  const pr = repo.prs.get(Number(ctx.params.number));
  if (!pr) return notFound('Pull request not found');
  if (pr.state === 'closed') return json(405, { message: pr.merged ? 'Pull request is already merged' : 'Pull request is closed' });
  const result = repo.squashMerge(pr);
  if ('conflict' in result) return json(405, { message: 'Merge conflict' });
  pr.merged = true;
  pr.state = 'closed';
  pr.mergedAt = trialNowIso();
  pr.mergeCommitSha = result.sha;
  pr.baseSha = result.sha;
  return json(200, { sha: result.sha, merged: true, message: `Pull request #${pr.number} successfully merged` });
}
