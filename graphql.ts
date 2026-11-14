// graphql.ts — a small GraphQL executor for the fixed set of documents the
// gh CLI / github-mcp-server shapes send (see checks-graphql.ts). It parses
// queries and mutations, resolves them against the store, and reports unknown
// fields as GraphQL errors (HTTP 200).
import { VIEWER_EMAIL, VIEWER_ID, VIEWER_LOGIN, VIEWER_NAME } from './git.ts';
import { trialNowIso, type Issue, type PullRequest, type Repo, type Store } from './store.ts';

// ---------------------------------------------------------------------------
// tokenizer + parser
// ---------------------------------------------------------------------------

type Token =
  | { kind: 'punct'; value: string }
  | { kind: 'name'; value: string }
  | { kind: 'string'; value: string }
  | { kind: 'int'; value: string }
  | { kind: 'float'; value: string }
  | { kind: 'spread'; value: string };

function tokenize(src: string): Token[] {
  const tokens: Token[] = [];
  let i = 0;
  while (i < src.length) {
    const ch = src[i];
    if (ch === ' ' || ch === '\t' || ch === '\n' || ch === '\r' || ch === ',') {
      i += 1;
      continue;
    }
    if (ch === '#') {
      while (i < src.length && src[i] !== '\n') i += 1;
      continue;
    }
    if (ch === '.' && src[i + 1] === '.' && src[i + 2] === '.') {
      tokens.push({ kind: 'spread', value: '...' });
      i += 3;
      continue;
    }
    if ('{}()[]:!=$'.includes(ch)) {
      tokens.push({ kind: 'punct', value: ch });
      i += 1;
      continue;
    }
    if (ch === '"') {
      let j = i + 1;
      let out = '';
      while (j < src.length && src[j] !== '"') {
        if (src[j] === '\\' && j + 1 < src.length) {
          const esc = src[j + 1];
          if (esc === 'n') out += '\n';
          else if (esc === 't') out += '\t';
          else if (esc === 'r') out += '\r';
          else out += esc;
          j += 2;
        } else {
          out += src[j];
          j += 1;
        }
      }
      tokens.push({ kind: 'string', value: out });
      i = j + 1;
      continue;
    }
    if (/[0-9-]/.test(ch) && (ch !== '-' || /[0-9]/.test(src[i + 1] ?? ''))) {
      let j = i;
      let isFloat = false;
      if (src[j] === '-') j += 1;
      while (j < src.length && /[0-9]/.test(src[j])) j += 1;
      if (src[j] === '.') {
        isFloat = true;
        j += 1;
        while (j < src.length && /[0-9]/.test(src[j])) j += 1;
      }
      if (src[j] === 'e' || src[j] === 'E') {
        isFloat = true;
        j += 1;
        if (src[j] === '+' || src[j] === '-') j += 1;
        while (j < src.length && /[0-9]/.test(src[j])) j += 1;
      }
      tokens.push({ kind: isFloat ? 'float' : 'int', value: src.slice(i, j) });
      i = j;
      continue;
    }
    if (/[A-Za-z_]/.test(ch)) {
      let j = i;
      while (j < src.length && /[A-Za-z0-9_]/.test(src[j])) j += 1;
      tokens.push({ kind: 'name', value: src.slice(i, j) });
      i = j;
      continue;
    }
    throw new Error(`graphql: unexpected character ${JSON.stringify(ch)}`);
  }
  return tokens;
}

export interface FieldNode {
  kind: 'field';
  alias?: string;
  name: string;
  args: Record<string, unknown>;
  selections?: SelectionNode[];
}

export interface FragmentNode {
  kind: 'fragment';
  typeCondition?: string;
  selections: SelectionNode[];
}

export type SelectionNode = FieldNode | FragmentNode;

export interface Operation {
  type: 'query' | 'mutation';
  name?: string;
  selections: SelectionNode[];
}

interface VarRef {
  __var: string;
}

class Parser {
  private pos = 0;
  private tokens: Token[];
  constructor(tokens: Token[]) {
    this.tokens = tokens;
  }

  private peek(): Token | undefined {
    return this.tokens[this.pos];
  }

  private next(): Token {
    const t = this.tokens[this.pos];
    if (!t) throw new Error('graphql: unexpected end of document');
    this.pos += 1;
    return t;
  }

  private expectPunct(value: string): void {
    const t = this.next();
    if (t.kind !== 'punct' || t.value !== value) throw new Error(`graphql: expected ${value}, got ${t.value}`);
  }

  private skipParens(): void {
    // Skip a balanced (...) block (variable definitions).
    if (this.peek()?.kind === 'punct' && this.peek()?.value === '(') {
      let depth = 0;
      while (this.pos < this.tokens.length) {
        const t = this.next();
        if (t.kind === 'punct' && t.value === '(') depth += 1;
        else if (t.kind === 'punct' && t.value === ')') {
          depth -= 1;
          if (depth === 0) break;
        }
      }
    }
  }

  parseDocument(): Operation[] {
    const ops: Operation[] = [];
    while (this.pos < this.tokens.length) {
      const t = this.peek()!;
      if (t.kind === 'name' && (t.value === 'query' || t.value === 'mutation')) {
        const type = this.next().value as 'query' | 'mutation';
        let name: string | undefined;
        const n = this.peek();
        if (n?.kind === 'name') name = this.next().value;
        this.skipParens();
        ops.push({ type, name, selections: this.parseSelectionSet() });
      } else if (t.kind === 'punct' && t.value === '{') {
        ops.push({ type: 'query', selections: this.parseSelectionSet() });
      } else {
        throw new Error(`graphql: unexpected token ${t.value}`);
      }
    }
    return ops;
  }

  private parseSelectionSet(): SelectionNode[] {
    this.expectPunct('{');
    const sels: SelectionNode[] = [];
    while (true) {
      const t = this.peek();
      if (!t) throw new Error('graphql: unterminated selection set');
      if (t.kind === 'punct' && t.value === '}') {
        this.next();
        break;
      }
      sels.push(this.parseSelection());
    }
    return sels;
  }

  private parseSelection(): SelectionNode {
    const t = this.peek()!;
    if (t.kind === 'spread') {
      this.next();
      let typeCondition: string | undefined;
      const n = this.peek();
      if (n?.kind === 'name' && n.value === 'on') {
        this.next();
        typeCondition = this.next().value;
      }
      return { kind: 'fragment', typeCondition, selections: this.parseSelectionSet() };
    }
    const first = this.next();
    if (first.kind !== 'name') throw new Error(`graphql: expected field name, got ${first.value}`);
    let alias: string | undefined;
    let name = first.value;
    const colon = this.peek();
    if (colon?.kind === 'punct' && colon.value === ':') {
      this.next();
      alias = name;
      name = this.next().value;
    }
    let args: Record<string, unknown> = {};
    const paren = this.peek();
    if (paren?.kind === 'punct' && paren.value === '(') args = this.parseArguments();
    let selections: SelectionNode[] | undefined;
    const brace = this.peek();
    if (brace?.kind === 'punct' && brace.value === '{') selections = this.parseSelectionSet();
    return { kind: 'field', alias, name, args, selections };
  }

  private parseArguments(): Record<string, unknown> {
    this.expectPunct('(');
    const args: Record<string, unknown> = {};
    while (true) {
      const t = this.peek();
      if (!t) throw new Error('graphql: unterminated arguments');
      if (t.kind === 'punct' && t.value === ')') {
        this.next();
        break;
      }
      const name = this.next().value;
      this.expectPunct(':');
      args[name] = this.parseValue();
    }
    return args;
  }

  private parseValue(): unknown {
    const t = this.next();
    if (t.kind === 'punct' && t.value === '$') {
      const name = this.next();
      return { __var: name.value } as VarRef;
    }
    if (t.kind === 'string') return t.value;
    if (t.kind === 'int') return parseInt(t.value, 10);
    if (t.kind === 'float') return parseFloat(t.value);
    if (t.kind === 'name') {
      if (t.value === 'true') return true;
      if (t.value === 'false') return false;
      if (t.value === 'null') return null;
      return t.value; // enum
    }
    if (t.kind === 'punct' && t.value === '[') {
      const list: unknown[] = [];
      while (true) {
        const n = this.peek();
        if (!n) throw new Error('graphql: unterminated list');
        if (n.kind === 'punct' && n.value === ']') {
          this.next();
          break;
        }
        list.push(this.parseValue());
      }
      return list;
    }
    if (t.kind === 'punct' && t.value === '{') {
      const obj: Record<string, unknown> = {};
      while (true) {
        const n = this.peek();
        if (!n) throw new Error('graphql: unterminated input object');
        if (n.kind === 'punct' && n.value === '}') {
          this.next();
          break;
        }
        const name = this.next().value;
        this.expectPunct(':');
        obj[name] = this.parseValue();
      }
      return obj;
    }
    throw new Error(`graphql: unexpected value token ${t.value}`);
  }
}

// ---------------------------------------------------------------------------
// value model
// ---------------------------------------------------------------------------

type GqlValue =
  | { __type: 'Query' }
  | { __type: 'Mutation' }
  | { __type: 'Viewer' }
  | { __type: 'Repository'; repo: Repo }
  | { __type: 'User'; login: string; id: number }
  | { __type: 'Ref'; name: string }
  | { __type: 'Issue'; repo: Repo; issue: Issue }
  | { __type: 'PullRequest'; repo: Repo; pr: PullRequest }
  | { __type: 'IssueConnection'; repo: Repo; items: Issue[]; hasNextPage: boolean; endCursor: string | null; startCursor: string | null }
  | { __type: 'PullRequestConnection'; repo: Repo; items: PullRequest[]; hasNextPage: boolean; endCursor: string | null }
  | { __type: 'FileConnection'; files: { path: string }[] }
  | { __type: 'PageInfo'; hasNextPage: boolean; hasPreviousPage: boolean; startCursor: string | null; endCursor: string | null }
  | { __type: 'Comment'; repo: Repo; issue: Issue; commentId: number; body: string }
  | { __type: 'CommentEdge'; repo: Repo; issue: Issue; commentId: number; body: string }
  | { __type: 'CreateIssuePayload'; repo: Repo; issue: Issue }
  | { __type: 'CloseIssuePayload'; repo: Repo; issue: Issue }
  | { __type: 'CreatePullRequestPayload'; repo: Repo; pr: PullRequest }
  | { __type: 'MergePullRequestPayload'; repo: Repo; pr: PullRequest }
  | { __type: 'AddCommentPayload'; repo: Repo; issue: Issue; commentId: number; body: string }
  | { __type: '__Type'; name: string };

const TYPE_FIELDS: Record<string, string[]> = {
  Issue: ['id', 'number', 'title', 'state', 'body', 'author', 'labels', 'comments', 'createdAt', 'url'],
  PullRequest: ['id', 'number', 'headRefName', 'baseRefName', 'mergeable', 'state', 'merged', 'files', 'title', 'body'],
  Repository: ['id', 'databaseId', 'name', 'nameWithOwner', 'owner', 'defaultBranchRef', 'viewerPermission', 'hasIssuesEnabled', 'issue', 'issues', 'pullRequests', 'pullRequest', 'description', 'url', 'createdAt'],
  User: ['id', 'login', 'name', 'email'],
  Viewer: ['login', 'id', 'name', 'email'],
  SearchType: [],
};

const TYPE_ENUM_VALUES: Record<string, string[]> = {
  SearchType: ['ISSUE', 'REPOSITORY', 'USER', 'DISCUSSION', 'COMMIT'],
};

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function resolveVariables(value: unknown, variables: Record<string, unknown>): unknown {
  if (isRecord(value) && '__var' in value && typeof value.__var === 'string') {
    return variables[value.__var] ?? null;
  }
  if (Array.isArray(value)) return value.map((v) => resolveVariables(v, variables));
  if (isRecord(value)) {
    const out: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(value)) out[k] = resolveVariables(v, variables);
    return out;
  }
  return value;
}

function cursorFor(number: number): string {
  return Buffer.from(`cursor:${number}`, 'utf8').toString('base64url');
}

function cursorNumber(cursor: string): number | null {
  try {
    const text = Buffer.from(cursor, 'base64url').toString('utf8');
    const m = text.match(/^cursor:(\d+)$/);
    return m ? Number(m[1]) : null;
  } catch {
    return null;
  }
}

function prState(pr: PullRequest): string {
  if (pr.merged) return 'MERGED';
  return pr.state === 'closed' ? 'CLOSED' : 'OPEN';
}

function issueState(issue: Issue): string {
  return issue.state === 'closed' ? 'CLOSED' : 'OPEN';
}

// ---------------------------------------------------------------------------
// executor
// ---------------------------------------------------------------------------

interface ExecCtx {
  store: Store;
  variables: Record<string, unknown>;
  errors: { message: string }[];
}

function fieldError(ctx: ExecCtx, typeName: string, field: string): null {
  ctx.errors.push({ message: `Cannot query field "${field}" on type "${typeName}".` });
  return null;
}

function resolveField(ctx: ExecCtx, value: GqlValue, field: FieldNode): unknown {
  const typeName = value.__type;
  const name = field.name;
  if (name === '__typename') return typeName;

  switch (typeName) {
    case 'Query': {
      if (name === 'viewer') return { __type: 'Viewer' } as GqlValue;
      if (name === 'repository') {
        const args = resolveVariables(field.args, ctx.variables) as Record<string, unknown>;
        const owner = String(args.owner ?? '');
        const repoName = String(args.name ?? '');
        if (owner !== VIEWER_LOGIN) return null;
        const repo = ctx.store.getRepo(owner, repoName);
        return repo ? ({ __type: 'Repository', repo } as GqlValue) : null;
      }
      if (name === 'node') {
        const args = resolveVariables(field.args, ctx.variables) as Record<string, unknown>;
        const resolved = ctx.store.resolveNode(String(args.id ?? ''));
        if (!resolved) return null;
        if (resolved.issue) return { __type: 'Issue', repo: resolved.repo, issue: resolved.issue } as GqlValue;
        if (resolved.pr) return { __type: 'PullRequest', repo: resolved.repo, pr: resolved.pr } as GqlValue;
        return { __type: 'Repository', repo: resolved.repo } as GqlValue;
      }
      if (name === '__type') {
        const args = resolveVariables(field.args, ctx.variables) as Record<string, unknown>;
        const tname = String(args.name ?? '');
        if (tname in TYPE_FIELDS || tname in TYPE_ENUM_VALUES) return { __type: '__Type', name: tname } as GqlValue;
        return null;
      }
      return fieldError(ctx, 'Query', name);
    }
    case 'Mutation': {
      if (name === 'createIssue') return mutateCreateIssue(ctx, field);
      if (name === 'closeIssue') return mutateCloseIssue(ctx, field);
      if (name === 'createPullRequest') return mutateCreatePullRequest(ctx, field);
      if (name === 'mergePullRequest') return mutateMergePullRequest(ctx, field);
      if (name === 'addComment') return mutateAddComment(ctx, field);
      return fieldError(ctx, 'Mutation', name);
    }
    case 'Viewer': {
      if (name === 'login') return VIEWER_LOGIN;
      if (name === 'id') return VIEWER_ID;
      if (name === 'name') return VIEWER_NAME;
      if (name === 'email') return VIEWER_EMAIL;
      return fieldError(ctx, 'Viewer', name);
    }
    case 'User': {
      if (name === 'login') return value.login;
      if (name === 'id') return value.id;
      if (name === 'name') return value.login;
      if (name === 'email') return `${value.login}@example.invalid`;
      return fieldError(ctx, 'User', name);
    }
    case 'Ref': {
      if (name === 'name') return value.name;
      return fieldError(ctx, 'Ref', name);
    }
    case 'Repository': {
      const repo = value.repo;
      if (name === 'id') return ctx.store.globalId('Repository', repo.id);
      if (name === 'databaseId') return repo.id;
      if (name === 'name') return repo.name;
      if (name === 'nameWithOwner') return `${repo.owner}/${repo.name}`;
      if (name === 'description') return repo.description;
      if (name === 'url') return `/api/v3/repos/${repo.owner}/${repo.name}`;
      if (name === 'createdAt') return trialNowIso();
      if (name === 'owner') return { __type: 'User', login: repo.owner, id: VIEWER_ID } as GqlValue;
      if (name === 'defaultBranchRef') {
        const tip = repo.defaultTip();
        if (!tip) return null;
        return { __type: 'Ref', name: repo.defaultBranch } as GqlValue;
      }
      if (name === 'viewerPermission') return 'ADMIN';
      if (name === 'hasIssuesEnabled') return true;
      if (name === 'issue') {
        const args = resolveVariables(field.args, ctx.variables) as Record<string, unknown>;
        const issue = repo.issues.get(Number(args.number));
        return issue ? ({ __type: 'Issue', repo, issue } as GqlValue) : null;
      }
      if (name === 'issues') return resolveIssuesConnection(ctx, repo, field);
      if (name === 'pullRequests') return resolvePullRequestsConnection(ctx, repo, field);
      if (name === 'pullRequest') {
        const args = resolveVariables(field.args, ctx.variables) as Record<string, unknown>;
        const pr = repo.prs.get(Number(args.number));
        return pr ? ({ __type: 'PullRequest', repo, pr } as GqlValue) : null;
      }
      return fieldError(ctx, 'Repository', name);
    }
    case 'Issue': {
      const issue = value.issue;
      if (name === 'id') return ctx.store.globalId('Issue', value.repo.id, issue.number);
      if (name === 'number') return issue.number;
      if (name === 'title') return issue.title;
      if (name === 'state') return issueState(issue);
      if (name === 'body') return issue.body;
      if (name === 'createdAt') return trialNowIso();
      if (name === 'url') return `/api/v3/repos/${value.repo.owner}/${value.repo.name}/issues/${issue.number}`;
      if (name === 'author') return { __type: 'User', login: VIEWER_LOGIN, id: VIEWER_ID } as GqlValue;
      if (name === 'labels') return { __type: 'FileConnection', files: issue.labels.map((l) => ({ path: l })) } as GqlValue;
      if (name === 'comments') return { __type: 'FileConnection', files: issue.comments.map((c) => ({ path: String(c.id) })) } as GqlValue;
      return fieldError(ctx, 'Issue', name);
    }
    case 'PullRequest': {
      const pr = value.pr;
      if (name === 'id') return ctx.store.globalId('PullRequest', value.repo.id, pr.number);
      if (name === 'number') return pr.number;
      if (name === 'title') return pr.title;
      if (name === 'body') return pr.body;
      if (name === 'state') return prState(pr);
      if (name === 'merged') return pr.merged;
      if (name === 'headRefName') return pr.headRef;
      if (name === 'baseRefName') return pr.baseRef;
      if (name === 'mergeable') return value.repo.mergeConflict(pr) ? 'CONFLICTING' : 'MERGEABLE';
      if (name === 'createdAt') return trialNowIso();
      if (name === 'url') return `/api/v3/repos/${value.repo.owner}/${value.repo.name}/pull/${pr.number}`;
      if (name === 'files') {
        const args = resolveVariables(field.args, ctx.variables) as Record<string, unknown>;
        const first = typeof args.first === 'number' ? args.first : 10;
        const files = value.repo.diffFiles(pr.baseSha, pr.headSha).map((f) => ({ path: f.filename }));
        return { __type: 'FileConnection', files: files.slice(0, first) } as GqlValue;
      }
      return fieldError(ctx, 'PullRequest', name);
    }
    case 'IssueConnection': {
      if (name === 'nodes') return value.items.map((issue) => ({ __type: 'Issue', repo: value.repo, issue }) as GqlValue);
      if (name === 'totalCount') return value.items.length;
      if (name === 'pageInfo') {
        return {
          __type: 'PageInfo',
          hasNextPage: value.hasNextPage,
          hasPreviousPage: value.startCursor !== null,
          startCursor: value.startCursor,
          endCursor: value.endCursor,
        } as GqlValue;
      }
      return fieldError(ctx, 'IssueConnection', name);
    }
    case 'PullRequestConnection': {
      if (name === 'nodes') return value.items.map((pr) => ({ __type: 'PullRequest', repo: value.repo, pr }) as GqlValue);
      if (name === 'totalCount') return value.items.length;
      if (name === 'pageInfo') {
        return {
          __type: 'PageInfo',
          hasNextPage: value.hasNextPage,
          hasPreviousPage: false,
          startCursor: value.items.length > 0 ? cursorFor(value.items[0].number) : null,
          endCursor: value.endCursor,
        } as GqlValue;
      }
      return fieldError(ctx, 'PullRequestConnection', name);
    }
    case 'FileConnection': {
      if (name === 'nodes') return value.files.map((f) => ({ __type: 'FileNode', path: f.path }) as unknown as GqlValue);
      if (name === 'totalCount') return value.files.length;
      return fieldError(ctx, 'FileConnection', name);
    }
    case 'PageInfo': {
      if (name === 'hasNextPage') return value.hasNextPage;
      if (name === 'hasPreviousPage') return value.hasPreviousPage;
      if (name === 'startCursor') return value.startCursor;
      if (name === 'endCursor') return value.endCursor;
      return fieldError(ctx, 'PageInfo', name);
    }
    case 'Comment': {
      if (name === 'id') return ctx.store.globalId('IssueComment', value.repo.id, value.issue.number, value.commentId);
      if (name === 'body') return value.body;
      if (name === 'author') return { __type: 'User', login: VIEWER_LOGIN, id: VIEWER_ID } as GqlValue;
      return fieldError(ctx, 'IssueComment', name);
    }
    case 'CommentEdge': {
      if (name === 'node') {
        return { __type: 'Comment', repo: value.repo, issue: value.issue, commentId: value.commentId, body: value.body } as GqlValue;
      }
      if (name === 'cursor') return cursorFor(value.commentId);
      return fieldError(ctx, 'IssueCommentEdge', name);
    }
    case 'CreateIssuePayload': {
      if (name === 'issue') return { __type: 'Issue', repo: value.repo, issue: value.issue } as GqlValue;
      return fieldError(ctx, 'CreateIssuePayload', name);
    }
    case 'CloseIssuePayload': {
      if (name === 'issue') return { __type: 'Issue', repo: value.repo, issue: value.issue } as GqlValue;
      return fieldError(ctx, 'CloseIssuePayload', name);
    }
    case 'CreatePullRequestPayload': {
      if (name === 'pullRequest') return { __type: 'PullRequest', repo: value.repo, pr: value.pr } as GqlValue;
      return fieldError(ctx, 'CreatePullRequestPayload', name);
    }
    case 'MergePullRequestPayload': {
      if (name === 'pullRequest') return { __type: 'PullRequest', repo: value.repo, pr: value.pr } as GqlValue;
      return fieldError(ctx, 'MergePullRequestPayload', name);
    }
    case 'AddCommentPayload': {
      if (name === 'commentEdge') {
        return { __type: 'CommentEdge', repo: value.repo, issue: value.issue, commentId: value.commentId, body: value.body } as GqlValue;
      }
      if (name === 'comment') {
        return { __type: 'Comment', repo: value.repo, issue: value.issue, commentId: value.commentId, body: value.body } as GqlValue;
      }
      return fieldError(ctx, 'AddCommentPayload', name);
    }
    case '__Type': {
      if (name === 'name') return value.name;
      if (name === 'kind') return value.name === 'SearchType' ? 'ENUM' : 'OBJECT';
      if (name === 'fields') {
        const fields = TYPE_FIELDS[value.name] ?? [];
        return fields.map((f) => ({ __type: '__Field', name: f }) as unknown as GqlValue);
      }
      if (name === 'enumValues') {
        const vals = TYPE_ENUM_VALUES[value.name] ?? [];
        return vals.map((v) => ({ __type: '__EnumValue', name: v }) as unknown as GqlValue);
      }
      return fieldError(ctx, '__Type', name);
    }
    default: {
      // __Field / __EnumValue pseudo-types and FileNode.
      const v = value as unknown as Record<string, unknown>;
      if (name === 'name') return v.name ?? null;
      if (name === 'path') return v.path ?? null;
      return fieldError(ctx, typeName, name);
    }
  }
}

function resolveIssuesConnection(ctx: ExecCtx, repo: Repo, field: FieldNode): GqlValue {
  const args = resolveVariables(field.args, ctx.variables) as Record<string, unknown>;
  const first = typeof args.first === 'number' ? args.first : 10;
  const after = typeof args.after === 'string' ? cursorNumber(args.after) : null;
  const states = Array.isArray(args.states) ? (args.states as string[]) : null;
  const orderBy = isRecord(args.orderBy) ? args.orderBy : {};
  const direction = String(orderBy.direction ?? 'DESC');
  let issues = [...repo.issues.values()];
  if (states) {
    const want = new Set(states);
    issues = issues.filter((i) => want.has(issueState(i)));
  }
  issues.sort((a, b) => (direction === 'ASC' ? a.createdSeq - b.createdSeq : b.createdSeq - a.createdSeq));
  if (after !== null) {
    issues = direction === 'ASC' ? issues.filter((i) => i.number > after) : issues.filter((i) => i.number < after);
  }
  const page = issues.slice(0, first);
  const hasNextPage = issues.length > first;
  return {
    __type: 'IssueConnection',
    repo,
    items: page,
    hasNextPage,
    endCursor: page.length > 0 ? cursorFor(page[page.length - 1].number) : null,
    startCursor: page.length > 0 ? cursorFor(page[0].number) : null,
  };
}

function resolvePullRequestsConnection(ctx: ExecCtx, repo: Repo, field: FieldNode): GqlValue {
  const args = resolveVariables(field.args, ctx.variables) as Record<string, unknown>;
  const first = typeof args.first === 'number' ? args.first : 10;
  const prs = [...repo.prs.values()].sort((a, b) => b.createdSeq - a.createdSeq);
  const page = prs.slice(0, first);
  return {
    __type: 'PullRequestConnection',
    repo,
    items: page,
    hasNextPage: prs.length > first,
    endCursor: page.length > 0 ? cursorFor(page[page.length - 1].number) : null,
  };
}

function mutateCreateIssue(ctx: ExecCtx, field: FieldNode): GqlValue | null {
  const args = resolveVariables(field.args, ctx.variables) as Record<string, unknown>;
  const input = isRecord(args.input) ? args.input : {};
  const resolved = ctx.store.resolveNode(String(input.repositoryId ?? ''));
  if (!resolved) {
    ctx.errors.push({ message: 'createIssue: repository not found' });
    return null;
  }
  const repo = resolved.repo;
  const title = String(input.title ?? '');
  if (!title) {
    ctx.errors.push({ message: 'createIssue: title is required' });
    return null;
  }
  const number = repo.nextNumber++;
  const issue: Issue = {
    id: ctx.store.nextId(),
    number,
    title,
    body: String(input.body ?? ''),
    state: 'open',
    labels: [],
    createdSeq: ++repo.seq,
    comments: [],
  };
  repo.issues.set(number, issue);
  return { __type: 'CreateIssuePayload', repo, issue };
}

function mutateCloseIssue(ctx: ExecCtx, field: FieldNode): GqlValue | null {
  const args = resolveVariables(field.args, ctx.variables) as Record<string, unknown>;
  const input = isRecord(args.input) ? args.input : {};
  const resolved = ctx.store.resolveNode(String(input.issueId ?? ''));
  if (!resolved || !resolved.issue) {
    ctx.errors.push({ message: 'closeIssue: issue not found' });
    return null;
  }
  resolved.issue.state = 'closed';
  return { __type: 'CloseIssuePayload', repo: resolved.repo, issue: resolved.issue };
}

function mutateCreatePullRequest(ctx: ExecCtx, field: FieldNode): GqlValue | null {
  const args = resolveVariables(field.args, ctx.variables) as Record<string, unknown>;
  const input = isRecord(args.input) ? args.input : {};
  const resolved = ctx.store.resolveNode(String(input.repositoryId ?? ''));
  if (!resolved) {
    ctx.errors.push({ message: 'createPullRequest: repository not found' });
    return null;
  }
  const repo = resolved.repo;
  const headRef = String(input.headRefName ?? '');
  const baseRef = String(input.baseRefName ?? '');
  const headSha = repo.branchTip(headRef);
  const baseSha = repo.branchTip(baseRef);
  if (!headSha || !baseSha) {
    ctx.errors.push({ message: 'createPullRequest: head or base branch not found' });
    return null;
  }
  const number = repo.nextNumber++;
  const pr: PullRequest = {
    id: ctx.store.nextId(),
    number,
    title: String(input.title ?? ''),
    body: String(input.body ?? ''),
    state: 'open',
    merged: false,
    mergedAt: null,
    mergeCommitSha: null,
    headRef,
    headSha,
    baseRef,
    baseSha,
    createdSeq: ++repo.seq,
  };
  repo.prs.set(number, pr);
  return { __type: 'CreatePullRequestPayload', repo, pr };
}

function mutateMergePullRequest(ctx: ExecCtx, field: FieldNode): GqlValue | null {
  const args = resolveVariables(field.args, ctx.variables) as Record<string, unknown>;
  const input = isRecord(args.input) ? args.input : {};
  const resolved = ctx.store.resolveNode(String(input.pullRequestId ?? ''));
  if (!resolved || !resolved.pr) {
    ctx.errors.push({ message: 'mergePullRequest: pull request not found' });
    return null;
  }
  const { repo, pr } = resolved;
  if (pr.state === 'closed') {
    ctx.errors.push({ message: 'mergePullRequest: pull request is closed' });
    return null;
  }
  const result = repo.squashMerge(pr);
  if ('conflict' in result) {
    ctx.errors.push({ message: 'mergePullRequest: merge conflict' });
    return null;
  }
  pr.merged = true;
  pr.state = 'closed';
  pr.mergedAt = trialNowIso();
  pr.mergeCommitSha = result.sha;
  pr.baseSha = result.sha;
  return { __type: 'MergePullRequestPayload', repo, pr };
}

function mutateAddComment(ctx: ExecCtx, field: FieldNode): GqlValue | null {
  const args = resolveVariables(field.args, ctx.variables) as Record<string, unknown>;
  const input = isRecord(args.input) ? args.input : {};
  const resolved = ctx.store.resolveNode(String(input.subjectId ?? ''));
  if (!resolved || !resolved.issue) {
    ctx.errors.push({ message: 'addComment: subject not found' });
    return null;
  }
  const body = String(input.body ?? '');
  const commentId = ctx.store.nextId();
  resolved.issue.comments.push({ id: commentId, body, createdSeq: ++resolved.repo.seq });
  return { __type: 'AddCommentPayload', repo: resolved.repo, issue: resolved.issue, commentId, body };
}

function executeSelections(ctx: ExecCtx, value: GqlValue, selections: SelectionNode[]): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const sel of selections) {
    if (sel.kind === 'fragment') {
      if (sel.typeCondition && sel.typeCondition !== value.__type) continue;
      Object.assign(out, executeSelections(ctx, value, sel.selections));
      continue;
    }
    const key = sel.alias ?? sel.name;
    const resolved = resolveField(ctx, value, sel);
    if (resolved === null || resolved === undefined) {
      out[key] = null;
      continue;
    }
    if (Array.isArray(resolved)) {
      out[key] = resolved.map((item) => {
        if (item && typeof item === 'object' && '__type' in item && sel.selections) {
          return executeSelections(ctx, item as GqlValue, sel.selections);
        }
        return item;
      });
      continue;
    }
    if (typeof resolved === 'object' && '__type' in resolved && sel.selections) {
      out[key] = executeSelections(ctx, resolved as GqlValue, sel.selections);
    } else {
      out[key] = resolved;
    }
  }
  return out;
}

export function executeGraphQL(
  store: Store,
  query: string,
  variables: Record<string, unknown>,
): { data: unknown; errors?: { message: string }[] } {
  let ops: Operation[];
  try {
    ops = new Parser(tokenize(query)).parseDocument();
  } catch (e) {
    return { data: null, errors: [{ message: e instanceof Error ? e.message : String(e) }] };
  }
  if (ops.length === 0) return { data: null, errors: [{ message: 'graphql: empty document' }] };
  const op = ops[0];
  const ctx: ExecCtx = { store, variables, errors: [] };
  const root: GqlValue = op.type === 'mutation' ? { __type: 'Mutation' } : { __type: 'Query' };
  const data = executeSelections(ctx, root, op.selections);
  return ctx.errors.length > 0 ? { data, errors: ctx.errors } : { data };
}
