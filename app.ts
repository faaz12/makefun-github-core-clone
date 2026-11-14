// app.ts — HTTP server: routing, auth, request ids, JSON body parsing.
// The entry's whole surface: REST v3, GraphQL v4, MCP, git smart HTTP.
import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { executeGraphQL } from './graphql.ts';
import { handleInfoRefs, handleReceivePack, handleUploadPack } from './githttp.ts';
import { handleMcp } from './mcp.ts';
import {
  apiRoot,
  createIssue,
  createIssueComment,
  createLabel,
  createPullRequest,
  createRef,
  createUserRepo,
  deleteContents,
  deleteRepo,
  getBranch,
  getContents,
  getIssue,
  getPullRequest,
  getRef,
  getRepo,
  getUser,
  listBranches,
  listCommits,
  listIssueComments,
  listIssues,
  listLabels,
  listPullRequestFiles,
  listPullRequests,
  listUserRepos,
  mergePullRequest,
  meta,
  patchIssue,
  patchRepo,
  putContents,
  type Ctx,
  type Resp,
} from './rest.ts';
import { Store } from './store.ts';

const store = new Store();
let requestCounter = 0;

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function readBody(req: IncomingMessage): Promise<Buffer> {
  return new Promise((resolve, reject) => {
    const chunks: Buffer[] = [];
    req.on('data', (c: Buffer) => chunks.push(c));
    req.on('end', () => resolve(Buffer.concat(chunks)));
    req.on('error', reject);
  });
}

function sendJson(res: ServerResponse, status: number, body: unknown, headers: Record<string, string> = {}): void {
  const payload = body === null ? '' : JSON.stringify(body);
  res.writeHead(status, { 'Content-Type': 'application/json', ...headers });
  res.end(payload);
}

function sendBuffer(res: ServerResponse, status: number, body: Buffer, headers: Record<string, string> = {}): void {
  res.writeHead(status, headers);
  res.end(body);
}

interface Route {
  method: string;
  pattern: string[];
  handler: (ctx: Ctx) => Resp | Promise<Resp>;
}

// Match a path against a pattern of segments; '*' captures the rest.
function matchPattern(pattern: string[], segments: string[]): Record<string, string> | null {
  const params: Record<string, string> = {};
  for (let i = 0; i < pattern.length; i += 1) {
    const p = pattern[i];
    if (p === '*') {
      params.path = segments.slice(i).map(decodeURIComponent).join('/');
      return params;
    }
    if (i >= segments.length) return null;
    const seg = segments[i];
    if (p.startsWith(':')) params[p.slice(1)] = decodeURIComponent(seg);
    else if (p !== seg) return null;
  }
  if (segments.length !== pattern.length) return null;
  return params;
}

const routes: Route[] = [
  { method: 'GET', pattern: ['api', 'v3'], handler: apiRoot },
  { method: 'GET', pattern: ['api', 'v3', 'meta'], handler: meta },
  { method: 'GET', pattern: ['api', 'v3', 'user'], handler: getUser },
  { method: 'POST', pattern: ['api', 'v3', 'user', 'repos'], handler: createUserRepo },
  { method: 'GET', pattern: ['api', 'v3', 'user', 'repos'], handler: listUserRepos },
  { method: 'GET', pattern: ['api', 'v3', 'repos', ':owner', ':repo'], handler: getRepo },
  { method: 'PATCH', pattern: ['api', 'v3', 'repos', ':owner', ':repo'], handler: patchRepo },
  { method: 'DELETE', pattern: ['api', 'v3', 'repos', ':owner', ':repo'], handler: deleteRepo },
  { method: 'POST', pattern: ['api', 'v3', 'repos', ':owner', ':repo', 'git', 'refs'], handler: createRef },
  { method: 'GET', pattern: ['api', 'v3', 'repos', ':owner', ':repo', 'git', 'refs', '*'], handler: getRef },
  { method: 'GET', pattern: ['api', 'v3', 'repos', ':owner', ':repo', 'branches'], handler: listBranches },
  { method: 'GET', pattern: ['api', 'v3', 'repos', ':owner', ':repo', 'branches', ':branch'], handler: getBranch },
  { method: 'GET', pattern: ['api', 'v3', 'repos', ':owner', ':repo', 'commits'], handler: listCommits },
  { method: 'GET', pattern: ['api', 'v3', 'repos', ':owner', ':repo', 'contents', '*'], handler: getContents },
  { method: 'PUT', pattern: ['api', 'v3', 'repos', ':owner', ':repo', 'contents', '*'], handler: putContents },
  { method: 'DELETE', pattern: ['api', 'v3', 'repos', ':owner', ':repo', 'contents', '*'], handler: deleteContents },
  { method: 'POST', pattern: ['api', 'v3', 'repos', ':owner', ':repo', 'labels'], handler: createLabel },
  { method: 'GET', pattern: ['api', 'v3', 'repos', ':owner', ':repo', 'labels'], handler: listLabels },
  { method: 'POST', pattern: ['api', 'v3', 'repos', ':owner', ':repo', 'issues'], handler: createIssue },
  { method: 'GET', pattern: ['api', 'v3', 'repos', ':owner', ':repo', 'issues'], handler: listIssues },
  { method: 'GET', pattern: ['api', 'v3', 'repos', ':owner', ':repo', 'issues', ':number'], handler: getIssue },
  { method: 'PATCH', pattern: ['api', 'v3', 'repos', ':owner', ':repo', 'issues', ':number'], handler: patchIssue },
  { method: 'POST', pattern: ['api', 'v3', 'repos', ':owner', ':repo', 'issues', ':number', 'comments'], handler: createIssueComment },
  { method: 'GET', pattern: ['api', 'v3', 'repos', ':owner', ':repo', 'issues', ':number', 'comments'], handler: listIssueComments },
  { method: 'POST', pattern: ['api', 'v3', 'repos', ':owner', ':repo', 'pulls'], handler: createPullRequest },
  { method: 'GET', pattern: ['api', 'v3', 'repos', ':owner', ':repo', 'pulls'], handler: listPullRequests },
  { method: 'GET', pattern: ['api', 'v3', 'repos', ':owner', ':repo', 'pulls', ':number'], handler: getPullRequest },
  { method: 'GET', pattern: ['api', 'v3', 'repos', ':owner', ':repo', 'pulls', ':number', 'files'], handler: listPullRequestFiles },
  { method: 'PUT', pattern: ['api', 'v3', 'repos', ':owner', ':repo', 'pulls', ':number', 'merge'], handler: mergePullRequest },
];

async function handleRequest(req: IncomingMessage, res: ServerResponse): Promise<void> {
  const requestId = `req-${++requestCounter}`;
  const baseHeaders: Record<string, string> = { 'X-GitHub-Request-Id': requestId };
  try {
    const url = new URL(req.url ?? '/', `http://${req.headers.host ?? 'localhost'}`);
    const segments = url.pathname.split('/').filter((s) => s.length > 0);
    const method = (req.method ?? 'GET').toUpperCase();

    // Health probe: no auth.
    if (method === 'GET' && segments.length === 1 && segments[0] === 'healthz') {
      sendJson(res, 200, { ok: true }, baseHeaders);
      return;
    }

    // Auth: Bearer token must match MAKEFUN_SHOP_TOKEN (when set).
    const auth = req.headers.authorization ?? '';
    const expected = process.env.MAKEFUN_SHOP_TOKEN ?? '';
    const presented = auth.startsWith('Bearer ') ? auth.slice(7) : '';
    if (!presented || (expected !== '' && presented !== expected)) {
      sendJson(res, 401, { message: 'Bad credentials' }, baseHeaders);
      return;
    }

    const rawBody = method === 'GET' || method === 'HEAD' ? Buffer.alloc(0) : await readBody(req);
    const baseUrl = `http://${req.headers.host ?? 'localhost'}`;

    // GraphQL.
    if (method === 'POST' && segments.length === 2 && segments[0] === 'api' && segments[1] === 'graphql') {
      let parsed: unknown = null;
      try {
        parsed = rawBody.length > 0 ? JSON.parse(rawBody.toString('utf8')) : null;
      } catch {
        sendJson(res, 400, { message: 'Problems parsing JSON' }, baseHeaders);
        return;
      }
      if (Array.isArray(parsed)) {
        const results = parsed.map((doc) => {
          const query = isRecord(doc) ? String(doc.query ?? '') : '';
          const variables = isRecord(doc) && isRecord(doc.variables) ? (doc.variables as Record<string, unknown>) : {};
          return executeGraphQL(store, query, variables);
        });
        sendJson(res, 200, results, baseHeaders);
        return;
      }
      const query = isRecord(parsed) ? String(parsed.query ?? '') : '';
      const variables = isRecord(parsed) && isRecord(parsed.variables) ? (parsed.variables as Record<string, unknown>) : {};
      sendJson(res, 200, executeGraphQL(store, query, variables), baseHeaders);
      return;
    }

    // MCP.
    if ((method === 'POST' || method === 'DELETE') && segments.length === 1 && segments[0] === 'mcp') {
      if (method === 'DELETE') {
        sendJson(res, 200, {}, baseHeaders);
        return;
      }
      let parsed: unknown = null;
      try {
        parsed = rawBody.length > 0 ? JSON.parse(rawBody.toString('utf8')) : null;
      } catch {
        sendJson(
          res,
          400,
          { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'Parse error' } },
          { ...baseHeaders, 'Mcp-Protocol-Version': '2026-07-28' },
        );
        return;
      }
      const ctx: Ctx = { store, method, url, params: {}, body: parsed, baseUrl };
      const out = handleMcp(ctx);
      if (out.isNotification) {
        res.writeHead(out.status, { ...baseHeaders, 'Mcp-Protocol-Version': '2026-07-28' });
        res.end();
        return;
      }
      sendJson(res, out.status, out.body, { ...baseHeaders, 'Mcp-Protocol-Version': '2026-07-28' });
      return;
    }

    // Git smart HTTP.
    //   /{owner}/{repo}.git/info/refs?service=...        (4 segments)
    //   /{owner}/{repo}.git/git-upload-pack              (3 segments)
    //   /{owner}/{repo}.git/git-receive-pack             (3 segments)
    const isGitPath =
      segments.length >= 3 &&
      segments[1].endsWith('.git') &&
      ((segments.length === 4 && segments[2] === 'info' && segments[3] === 'refs') ||
        (segments.length === 3 && (segments[2] === 'git-upload-pack' || segments[2] === 'git-receive-pack')));
    if (isGitPath) {
      const owner = decodeURIComponent(segments[0]);
      const repoName = decodeURIComponent(segments[1].slice(0, -4));
      const repo = store.getRepo(owner, repoName);
      if (segments[2] === 'info') {
        // /info/refs?service=...
        const service = url.searchParams.get('service');
        if (service !== 'git-upload-pack' && service !== 'git-receive-pack') {
          sendJson(res, 400, { message: 'service is required' }, baseHeaders);
          return;
        }
        const out = handleInfoRefs(repo, service, req.headers['git-protocol'] as string | undefined);
        if (Buffer.isBuffer(out.body)) {
          sendBuffer(res, out.status, out.body, { ...baseHeaders, ...(out.headers ?? {}) });
        } else {
          sendJson(res, out.status, out.body, baseHeaders);
        }
        return;
      }
      if (segments[2] === 'git-upload-pack') {
        const out = handleUploadPack(repo, rawBody);
        if (Buffer.isBuffer(out.body)) sendBuffer(res, out.status, out.body, { ...baseHeaders, ...(out.headers ?? {}) });
        else sendJson(res, out.status, out.body, baseHeaders);
        return;
      }
      const out = handleReceivePack(repo, rawBody);
      if (Buffer.isBuffer(out.body)) sendBuffer(res, out.status, out.body, { ...baseHeaders, ...(out.headers ?? {}) });
      else sendJson(res, out.status, out.body, baseHeaders);
      return;
    }

    // REST routes.
    let body: unknown = null;
    if (rawBody.length > 0) {
      try {
        body = JSON.parse(rawBody.toString('utf8'));
      } catch {
        sendJson(res, 400, { message: 'Problems parsing JSON' }, baseHeaders);
        return;
      }
    }
    for (const route of routes) {
      if (route.method !== method) continue;
      const params = matchPattern(route.pattern, segments);
      if (!params) continue;
      const ctx: Ctx = { store, method, url, params, body, baseUrl };
      const out = await route.handler(ctx);
      const headers = { ...baseHeaders, ...(out.headers ?? {}) };
      if (route.handler === apiRoot) headers['X-OAuth-Scopes'] = 'repo, read:org';
      if (out.status === 204 || out.body === null) {
        res.writeHead(out.status, headers);
        res.end();
        return;
      }
      sendJson(res, out.status, out.body, headers);
      return;
    }

    sendJson(res, 404, { message: 'Not Found' }, baseHeaders);
  } catch (e) {
    // Never crash on a bad request: answer 500 and stay up.
    try {
      sendJson(res, 500, { message: 'Internal Server Error' }, baseHeaders);
    } catch {
      // response already gone
    }
    console.error('handler error:', e instanceof Error ? e.message : String(e));
  }
}

const server = createServer((req, res) => {
  void handleRequest(req, res);
});

export function createApp(): ReturnType<typeof createServer> {
  return server;
}

// When run directly (node app.ts), bind an ephemeral port and print it.
if (process.argv[1] && process.argv[1].endsWith('app.ts')) {
  server.listen(0, () => {
    const addr = server.address();
    console.log(`forge listening on port ${typeof addr === 'object' && addr ? addr.port : '?'}`);
  });
}
