// mcp.ts — Model Context Protocol over Streamable HTTP (JSON-RPC 2.0).
// Modern era 2026-07-28: server/discover + tools/list + tools/call.
// The tool list is a constant so two tools/list calls answer byte-identical.
import { VIEWER_LOGIN, VIEWER_ID } from './git.ts';
import { issueJson, prJson, repoJson, type Ctx } from './rest.ts';
import { trialNowIso, type Issue, type PullRequest, type Repo } from './store.ts';

export interface McpRequest {
  jsonrpc?: string;
  id?: string | number | null;
  method?: string;
  params?: Record<string, unknown>;
}

const TOOLS = [
  {
    name: 'create_repo',
    description: 'Create a new repository owned by the authenticated user.',
    inputSchema: {
      type: 'object',
      properties: {
        name: { type: 'string', description: 'Repository name' },
        description: { type: 'string', description: 'Repository description' },
        private: { type: 'boolean', description: 'Whether the repository is private' },
      },
      required: ['name'],
    },
  },
  {
    name: 'get_repo',
    description: 'Get a repository by owner and name.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        full_name: { type: 'string', description: 'owner/repo' },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_issue',
    description: 'Create an issue in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        full_name: { type: 'string', description: 'owner/repo' },
        title: { type: 'string', description: 'Issue title' },
        body: { type: 'string', description: 'Issue body' },
      },
      required: ['owner', 'repo', 'title'],
    },
  },
  {
    name: 'list_issues',
    description: 'List issues in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        full_name: { type: 'string', description: 'owner/repo' },
        state: { type: 'string', description: 'open, closed or all', enum: ['open', 'closed', 'all'] },
      },
      required: ['owner', 'repo'],
    },
  },
  {
    name: 'create_pull_request',
    description: 'Create a pull request in a repository.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        full_name: { type: 'string', description: 'owner/repo' },
        title: { type: 'string', description: 'Pull request title' },
        body: { type: 'string', description: 'Pull request body' },
        head: { type: 'string', description: 'Head branch name' },
        base: { type: 'string', description: 'Base branch name' },
      },
      required: ['owner', 'repo', 'title', 'head', 'base'],
    },
  },
  {
    name: 'merge_pull_request',
    description: 'Merge a pull request with a squash merge.',
    inputSchema: {
      type: 'object',
      properties: {
        owner: { type: 'string', description: 'Repository owner login' },
        repo: { type: 'string', description: 'Repository name' },
        full_name: { type: 'string', description: 'owner/repo' },
        pull_number: { type: 'integer', description: 'Pull request number' },
      },
      required: ['owner', 'repo', 'pull_number'],
    },
  },
];

// Pre-serialized constant: two tools/list calls answer byte-identical.
const TOOLS_LIST_RESULT = JSON.stringify({ tools: TOOLS });

const SERVER_INFO = { name: 'forge', version: '1.0.0' };

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function str(v: unknown, fallback = ''): string {
  return typeof v === 'string' ? v : fallback;
}

function resolveRepoArg(ctx: Ctx, args: Record<string, unknown>): Repo | null {
  let owner = str(args.owner);
  let name = str(args.repo);
  const fullName = str(args.full_name);
  if ((!owner || !name) && fullName.includes('/')) {
    const [o, r] = fullName.split('/');
    owner = owner || o;
    name = name || r;
  }
  if (!owner || !name) return null;
  if (owner !== VIEWER_LOGIN) return null;
  return ctx.store.getRepo(owner, name) ?? null;
}

function toolResult(result: unknown): Record<string, unknown> {
  const text = JSON.stringify(result);
  return { content: [{ type: 'text', text }], structuredContent: result };
}

function toolError(message: string): Record<string, unknown> {
  return { content: [{ type: 'text', text: JSON.stringify({ error: message }) }], isError: true };
}

function callTool(ctx: Ctx, name: string, args: Record<string, unknown>): Record<string, unknown> {
  const normalized = name.toLowerCase().replace(/[^a-z0-9]/g, '_');
  if (normalized === 'create_repo') {
    const repoName = str(args.name).trim();
    if (!repoName) return toolError('name is required');
    const existing = ctx.store.getRepo(VIEWER_LOGIN, repoName);
    if (existing) return toolError(`A repository named ${repoName} already exists`);
    const repo = ctx.store.createRepo(repoName, VIEWER_LOGIN, str(args.description), args.private === true);
    return toolResult(repoJson(repo, ctx.baseUrl));
  }
  if (normalized === 'get_repo') {
    const repo = resolveRepoArg(ctx, args);
    if (!repo) return toolError('Repository not found');
    return toolResult(repoJson(repo, ctx.baseUrl));
  }
  if (normalized === 'create_issue') {
    const repo = resolveRepoArg(ctx, args);
    if (!repo) return toolError('Repository not found');
    const title = str(args.title).trim();
    if (!title) return toolError('title is required');
    const number = repo.nextNumber++;
    const issue: Issue = {
      id: ctx.store.nextId(),
      number,
      title,
      body: str(args.body),
      state: 'open',
      labels: [],
      createdSeq: ++repo.seq,
      comments: [],
    };
    repo.issues.set(number, issue);
    return toolResult(issueJson(repo, issue));
  }
  if (normalized === 'list_issues') {
    const repo = resolveRepoArg(ctx, args);
    if (!repo) return toolError('Repository not found');
    const state = str(args.state, 'open');
    let issues = [...repo.issues.values()];
    if (state !== 'all') issues = issues.filter((i) => i.state === state);
    issues.sort((a, b) => b.createdSeq - a.createdSeq);
    return toolResult({ issues: issues.map((i) => issueJson(repo, i)) });
  }
  if (normalized === 'create_pull_request') {
    const repo = resolveRepoArg(ctx, args);
    if (!repo) return toolError('Repository not found');
    const title = str(args.title).trim();
    const head = str(args.head).trim();
    const base = str(args.base).trim();
    if (!title || !head || !base) return toolError('title, head and base are required');
    const headSha = repo.branchTip(head);
    const baseSha = repo.branchTip(base);
    if (!headSha) return toolError(`head branch ${head} does not exist`);
    if (!baseSha) return toolError(`base branch ${base} does not exist`);
    const number = repo.nextNumber++;
    const pr: PullRequest = {
      id: ctx.store.nextId(),
      number,
      title,
      body: str(args.body),
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
    return toolResult(prJson(repo, pr));
  }
  if (normalized === 'merge_pull_request') {
    const repo = resolveRepoArg(ctx, args);
    if (!repo) return toolError('Repository not found');
    const num = typeof args.pull_number === 'number' ? args.pull_number : typeof args.pullNumber === 'number' ? args.pullNumber : NaN;
    const pr = repo.prs.get(num);
    if (!pr) return toolError('Pull request not found');
    if (pr.state === 'closed') return toolError('Pull request is closed');
    const result = repo.squashMerge(pr);
    if ('conflict' in result) return toolError('Merge conflict');
    pr.merged = true;
    pr.state = 'closed';
    pr.mergedAt = trialNowIso();
    pr.mergeCommitSha = result.sha;
    pr.baseSha = result.sha;
    return toolResult({ merged: true, sha: result.sha, message: `Pull request #${pr.number} successfully merged` });
  }
  return toolError(`Unknown tool: ${name}`);
}

export function handleMcp(ctx: Ctx): { status: number; body: unknown; isNotification: boolean } {
  const req = ctx.body as McpRequest | null;
  if (!isRecord(req)) {
    return { status: 400, body: { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } }, isNotification: false };
  }
  const id = req.id ?? null;
  const method = str(req.method);
  const isNotification = id === null || id === undefined;

  // Notifications (no id): acknowledge, no JSON-RPC body.
  if (isNotification) {
    return { status: 202, body: null, isNotification: true };
  }

  const result = (r: unknown) => ({ status: 200, body: { jsonrpc: '2.0', id, result: r }, isNotification: false });
  const error = (code: number, message: string, data?: unknown) => ({
    status: 200,
    body: { jsonrpc: '2.0', id, error: { code, message, ...(data !== undefined ? { data } : {}) } },
    isNotification: false,
  });

  if (method === 'server/discover') {
    return result({
      serverInfo: SERVER_INFO,
      capabilities: { tools: {} },
      supportedVersions: ['2026-07-28', '2025-06-18'],
      protocolVersion: '2026-07-28',
      _meta: { 'io.modelcontextprotocol/serverInfo': SERVER_INFO },
    });
  }
  if (method === 'initialize') {
    return result({
      protocolVersion: '2026-07-28',
      serverInfo: SERVER_INFO,
      capabilities: { tools: {} },
    });
  }
  if (method === 'ping') return result({});
  if (method === 'tools/list') {
    // Byte-identical every time: the result payload is a pre-serialized constant.
    return { status: 200, body: { jsonrpc: '2.0', id, result: JSON.parse(TOOLS_LIST_RESULT) }, isNotification: false };
  }
  if (method === 'tools/call') {
    const params = isRecord(req.params) ? req.params : {};
    const toolName = str(params.name);
    const args = isRecord(params.arguments) ? (params.arguments as Record<string, unknown>) : {};
    const known = TOOLS.some((t) => t.name === toolName.toLowerCase().replace(/[^a-z0-9]/g, '_'));
    if (!known) return error(-32602, `Unknown tool: ${toolName}`);
    const res = callTool(ctx, toolName, args);
    if (res.isError) return error(-32602, String((res.content as { text: string }[])[0]?.text ?? 'tool error'));
    return result(res);
  }
  return error(-32601, `Method not found: ${method}`);
}
