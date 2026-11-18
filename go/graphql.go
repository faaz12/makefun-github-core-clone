// graphql.go — a small GraphQL executor for the fixed set of documents the
// gh CLI / github-mcp-server shapes send. Port of graphql.ts.
package main

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// tokenizer + parser
// ---------------------------------------------------------------------------

type tokenKind int

const (
	tPunct tokenKind = iota
	tName
	tString
	tInt
	tFloat
	tSpread
)

type token struct {
	kind  tokenKind
	value string
}

func tokenize(src string) ([]token, error) {
	var tokens []token
	i := 0
	for i < len(src) {
		ch := src[i]
		switch {
		case ch == ' ' || ch == '\t' || ch == '\n' || ch == '\r' || ch == ',':
			i++
		case ch == '#':
			for i < len(src) && src[i] != '\n' {
				i++
			}
		case ch == '.' && i+2 < len(src) && src[i+1] == '.' && src[i+2] == '.':
			tokens = append(tokens, token{tSpread, "..."})
			i += 3
		case strings.IndexByte("{}()[]:!=$", ch) >= 0:
			tokens = append(tokens, token{tPunct, string(ch)})
			i++
		case ch == '"':
			j := i + 1
			var sb strings.Builder
			for j < len(src) && src[j] != '"' {
				if src[j] == '\\' && j+1 < len(src) {
					esc := src[j+1]
					switch esc {
					case 'n':
						sb.WriteByte('\n')
					case 't':
						sb.WriteByte('\t')
					case 'r':
						sb.WriteByte('\r')
					default:
						sb.WriteByte(esc)
					}
					j += 2
				} else {
					sb.WriteByte(src[j])
					j++
				}
			}
			tokens = append(tokens, token{tString, sb.String()})
			i = j + 1
		case (ch >= '0' && ch <= '9') || (ch == '-' && i+1 < len(src) && src[i+1] >= '0' && src[i+1] <= '9'):
			j := i
			isFloat := false
			if src[j] == '-' {
				j++
			}
			for j < len(src) && src[j] >= '0' && src[j] <= '9' {
				j++
			}
			if j < len(src) && src[j] == '.' {
				isFloat = true
				j++
				for j < len(src) && src[j] >= '0' && src[j] <= '9' {
					j++
				}
			}
			if j < len(src) && (src[j] == 'e' || src[j] == 'E') {
				isFloat = true
				j++
				if j < len(src) && (src[j] == '+' || src[j] == '-') {
					j++
				}
				for j < len(src) && src[j] >= '0' && src[j] <= '9' {
					j++
				}
			}
			kind := tInt
			if isFloat {
				kind = tFloat
			}
			tokens = append(tokens, token{kind, src[i:j]})
			i = j
		case (ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || ch == '_':
			j := i
			for j < len(src) && ((src[j] >= 'A' && src[j] <= 'Z') || (src[j] >= 'a' && src[j] <= 'z') || (src[j] >= '0' && src[j] <= '9') || src[j] == '_') {
				j++
			}
			tokens = append(tokens, token{tName, src[i:j]})
			i = j
		default:
			return nil, fmt.Errorf("graphql: unexpected character %q", string(ch))
		}
	}
	return tokens, nil
}

type fieldNode struct {
	alias      string
	name       string
	args       map[string]interface{}
	selections []selNode
}

type fragmentNode struct {
	typeCondition string
	selections    []selNode
}

type selNode struct {
	isFragment bool
	field      *fieldNode
	fragment   *fragmentNode
}

type operation struct {
	typ        string // "query" | "mutation"
	name       string
	selections []selNode
}

type varRef struct{ name string }

type parser struct {
	pos    int
	tokens []token
}

func (p *parser) peek() *token {
	if p.pos < len(p.tokens) {
		return &p.tokens[p.pos]
	}
	return nil
}

func (p *parser) next() (token, error) {
	if p.pos >= len(p.tokens) {
		return token{}, fmt.Errorf("graphql: unexpected end of document")
	}
	t := p.tokens[p.pos]
	p.pos++
	return t, nil
}

func (p *parser) expectPunct(value string) error {
	t, err := p.next()
	if err != nil {
		return err
	}
	if t.kind != tPunct || t.value != value {
		return fmt.Errorf("graphql: expected %s, got %s", value, t.value)
	}
	return nil
}

func (p *parser) skipParens() {
	if t := p.peek(); t != nil && t.kind == tPunct && t.value == "(" {
		depth := 0
		for p.pos < len(p.tokens) {
			t, _ := p.next()
			if t.kind == tPunct && t.value == "(" {
				depth++
			} else if t.kind == tPunct && t.value == ")" {
				depth--
				if depth == 0 {
					break
				}
			}
		}
	}
}

func (p *parser) parseDocument() ([]operation, error) {
	var ops []operation
	for p.pos < len(p.tokens) {
		t := p.peek()
		if t.kind == tName && (t.value == "query" || t.value == "mutation") {
			typ, _ := p.next()
			var name string
			if n := p.peek(); n != nil && n.kind == tName {
				nt, _ := p.next()
				name = nt.value
			}
			p.skipParens()
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			ops = append(ops, operation{typ: typ.value, name: name, selections: sels})
		} else if t.kind == tPunct && t.value == "{" {
			sels, err := p.parseSelectionSet()
			if err != nil {
				return nil, err
			}
			ops = append(ops, operation{typ: "query", selections: sels})
		} else {
			return nil, fmt.Errorf("graphql: unexpected token %s", t.value)
		}
	}
	return ops, nil
}

func (p *parser) parseSelectionSet() ([]selNode, error) {
	if err := p.expectPunct("{"); err != nil {
		return nil, err
	}
	var sels []selNode
	for {
		t := p.peek()
		if t == nil {
			return nil, fmt.Errorf("graphql: unterminated selection set")
		}
		if t.kind == tPunct && t.value == "}" {
			p.next()
			break
		}
		sel, err := p.parseSelection()
		if err != nil {
			return nil, err
		}
		sels = append(sels, sel)
	}
	return sels, nil
}

func (p *parser) parseSelection() (selNode, error) {
	t := p.peek()
	if t == nil {
		return selNode{}, fmt.Errorf("graphql: unexpected end of document")
	}
	if t.kind == tSpread {
		p.next()
		var typeCondition string
		if n := p.peek(); n != nil && n.kind == tName && n.value == "on" {
			p.next()
			tc, err := p.next()
			if err != nil {
				return selNode{}, err
			}
			typeCondition = tc.value
		}
		sels, err := p.parseSelectionSet()
		if err != nil {
			return selNode{}, err
		}
		return selNode{isFragment: true, fragment: &fragmentNode{typeCondition: typeCondition, selections: sels}}, nil
	}
	first, err := p.next()
	if err != nil {
		return selNode{}, err
	}
	if first.kind != tName {
		return selNode{}, fmt.Errorf("graphql: expected field name, got %s", first.value)
	}
	var alias, name string
	name = first.value
	if colon := p.peek(); colon != nil && colon.kind == tPunct && colon.value == ":" {
		p.next()
		alias = name
		nm, err := p.next()
		if err != nil {
			return selNode{}, err
		}
		name = nm.value
	}
	args := map[string]interface{}{}
	if paren := p.peek(); paren != nil && paren.kind == tPunct && paren.value == "(" {
		args, err = p.parseArguments()
		if err != nil {
			return selNode{}, err
		}
	}
	var sels []selNode
	if brace := p.peek(); brace != nil && brace.kind == tPunct && brace.value == "{" {
		sels, err = p.parseSelectionSet()
		if err != nil {
			return selNode{}, err
		}
	}
	return selNode{field: &fieldNode{alias: alias, name: name, args: args, selections: sels}}, nil
}

func (p *parser) parseArguments() (map[string]interface{}, error) {
	if err := p.expectPunct("("); err != nil {
		return nil, err
	}
	args := map[string]interface{}{}
	for {
		t := p.peek()
		if t == nil {
			return nil, fmt.Errorf("graphql: unterminated arguments")
		}
		if t.kind == tPunct && t.value == ")" {
			p.next()
			break
		}
		name, err := p.next()
		if err != nil {
			return nil, err
		}
		if err := p.expectPunct(":"); err != nil {
			return nil, err
		}
		v, err := p.parseValue()
		if err != nil {
			return nil, err
		}
		args[name.value] = v
	}
	return args, nil
}

func (p *parser) parseValue() (interface{}, error) {
	t, err := p.next()
	if err != nil {
		return nil, err
	}
	switch {
	case t.kind == tPunct && t.value == "$":
		name, err := p.next()
		if err != nil {
			return nil, err
		}
		return varRef{name.value}, nil
	case t.kind == tString:
		return t.value, nil
	case t.kind == tInt:
		n, _ := strconv.ParseInt(t.value, 10, 64)
		return float64(n), nil
	case t.kind == tFloat:
		f, _ := strconv.ParseFloat(t.value, 64)
		return f, nil
	case t.kind == tName:
		switch t.value {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "null":
			return nil, nil
		default:
			return t.value, nil
		}
	case t.kind == tPunct && t.value == "[":
		var list []interface{}
		for {
			n := p.peek()
			if n == nil {
				return nil, fmt.Errorf("graphql: unterminated list")
			}
			if n.kind == tPunct && n.value == "]" {
				p.next()
				break
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			list = append(list, v)
		}
		return list, nil
	case t.kind == tPunct && t.value == "{":
		obj := map[string]interface{}{}
		for {
			n := p.peek()
			if n == nil {
				return nil, fmt.Errorf("graphql: unterminated input object")
			}
			if n.kind == tPunct && n.value == "}" {
				p.next()
				break
			}
			name, err := p.next()
			if err != nil {
				return nil, err
			}
			if err := p.expectPunct(":"); err != nil {
				return nil, err
			}
			v, err := p.parseValue()
			if err != nil {
				return nil, err
			}
			obj[name.value] = v
		}
		return obj, nil
	}
	return nil, fmt.Errorf("graphql: unexpected value token %s", t.value)
}

// ---------------------------------------------------------------------------
// value model
// ---------------------------------------------------------------------------

type gqlValue struct {
	kind string
	repo *Repo
	issue *Issue
	pr *PullRequest
	// User / Ref
	login   string
	id      int
	refName string
	// connections
	issueItems []*Issue
	prItems    []*PullRequest
	hasNextPage bool
	hasPrevPage bool
	startCursor *string
	endCursor   *string
	// FileConnection
	paths []string
	// Comment / CommentEdge / payloads
	commentID int
	body      string
	// __Type / pseudo types
	typeName string
	name      string
	path      string
}

var gqlTypeFields = map[string][]string{
	"Issue":       {"id", "number", "title", "state", "body", "author", "labels", "comments", "createdAt", "url"},
	"PullRequest": {"id", "number", "headRefName", "baseRefName", "mergeable", "state", "merged", "files", "title", "body"},
	"Repository":  {"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "issue", "issues", "pullRequests", "pullRequest", "description", "url", "createdAt"},
	"User":        {"id", "login", "name", "email"},
	"Viewer":      {"login", "id", "name", "email"},
	"SearchType":  {},
}

var gqlTypeEnumValues = map[string][]string{
	"SearchType": {"ISSUE", "REPOSITORY", "USER", "DISCUSSION", "COMMIT"},
}

func resolveVariables(value interface{}, variables map[string]interface{}) interface{} {
	switch v := value.(type) {
	case varRef:
		if variables == nil {
			return nil
		}
		if val, ok := variables[v.name]; ok {
			return val
		}
		return nil
	case []interface{}:
		out := make([]interface{}, len(v))
		for i, e := range v {
			out[i] = resolveVariables(e, variables)
		}
		return out
	case map[string]interface{}:
		out := make(map[string]interface{}, len(v))
		for k, e := range v {
			out[k] = resolveVariables(e, variables)
		}
		return out
	}
	return value
}

func cursorFor(number int) string {
	return base64.RawURLEncoding.EncodeToString([]byte("cursor:" + strconv.Itoa(number)))
}

func cursorNumber(cursor string) (int, bool) {
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, false
	}
	text := string(b)
	if !strings.HasPrefix(text, "cursor:") {
		return 0, false
	}
	n, err := strconv.Atoi(text[len("cursor:"):])
	if err != nil {
		return 0, false
	}
	return n, true
}

func prState(pr *PullRequest) string {
	if pr.Merged {
		return "MERGED"
	}
	if pr.State == "closed" {
		return "CLOSED"
	}
	return "OPEN"
}

func issueState(issue *Issue) string {
	if issue.State == "closed" {
		return "CLOSED"
	}
	return "OPEN"
}

// ---------------------------------------------------------------------------
// executor
// ---------------------------------------------------------------------------

type gqlError struct {
	Message string `json:"message"`
}

type execCtx struct {
	store     *Store
	variables map[string]interface{}
	errors    []gqlError
}

func fieldError(ctx *execCtx, typeName, field string) interface{} {
	ctx.errors = append(ctx.errors, gqlError{Message: "Cannot query field \"" + field + "\" on type \"" + typeName + "\"."})
	return nil
}

func strArg(args map[string]interface{}, key string) string {
	if v, ok := args[key].(string); ok {
		return v
	}
	return ""
}

func numArg(args map[string]interface{}, key string) (float64, bool) {
	f, ok := args[key].(float64)
	return f, ok
}

func resolveField(ctx *execCtx, value *gqlValue, field *fieldNode) interface{} {
	typeName := value.kind
	name := field.name
	if name == "__typename" {
		return typeName
	}

	switch typeName {
	case "Query":
		switch name {
		case "viewer":
			return &gqlValue{kind: "Viewer"}
		case "repository":
			args := resolveVariables(field.args, ctx.variables).(map[string]interface{})
			owner := strArg(args, "owner")
			repoName := strArg(args, "name")
			if owner != ViewerLogin {
				return nil
			}
			repo := ctx.store.GetRepo(owner, repoName)
			if repo == nil {
				return nil
			}
			return &gqlValue{kind: "Repository", repo: repo}
		case "node":
			args := resolveVariables(field.args, ctx.variables).(map[string]interface{})
			resolved := ctx.store.ResolveNode(strArg(args, "id"))
			if resolved == nil {
				return nil
			}
			if resolved.Issue != nil {
				return &gqlValue{kind: "Issue", repo: resolved.Repo, issue: resolved.Issue}
			}
			if resolved.PR != nil {
				return &gqlValue{kind: "PullRequest", repo: resolved.Repo, pr: resolved.PR}
			}
			return &gqlValue{kind: "Repository", repo: resolved.Repo}
		case "__type":
			args := resolveVariables(field.args, ctx.variables).(map[string]interface{})
			tname := strArg(args, "name")
			if _, ok := gqlTypeFields[tname]; ok {
				return &gqlValue{kind: "__Type", typeName: tname}
			}
			if _, ok := gqlTypeEnumValues[tname]; ok {
				return &gqlValue{kind: "__Type", typeName: tname}
			}
			return nil
		}
		return fieldError(ctx, "Query", name)
	case "Mutation":
		switch name {
		case "createIssue":
			return mutateCreateIssue(ctx, field)
		case "closeIssue":
			return mutateCloseIssue(ctx, field)
		case "createPullRequest":
			return mutateCreatePullRequest(ctx, field)
		case "mergePullRequest":
			return mutateMergePullRequest(ctx, field)
		case "addComment":
			return mutateAddComment(ctx, field)
		}
		return fieldError(ctx, "Mutation", name)
	case "Viewer":
		switch name {
		case "login":
			return ViewerLogin
		case "id":
			return float64(ViewerID)
		case "name":
			return ViewerName
		case "email":
			return ViewerEmail
		}
		return fieldError(ctx, "Viewer", name)
	case "User":
		switch name {
		case "login":
			return value.login
		case "id":
			return float64(value.id)
		case "name":
			return value.login
		case "email":
			return value.login + "@example.invalid"
		}
		return fieldError(ctx, "User", name)
	case "Ref":
		if name == "name" {
			return value.refName
		}
		return fieldError(ctx, "Ref", name)
	case "Repository":
		repo := value.repo
		switch name {
		case "id":
			return ctx.store.GlobalID("Repository", repo.ID)
		case "databaseId":
			return float64(repo.ID)
		case "name":
			return repo.Name
		case "nameWithOwner":
			return repo.Owner + "/" + repo.Name
		case "description":
			return repo.Description
		case "url":
			return "/api/v3/repos/" + repo.Owner + "/" + repo.Name
		case "createdAt":
			return trialNowISO
		case "owner":
			return &gqlValue{kind: "User", login: repo.Owner, id: ViewerID}
		case "defaultBranchRef":
			if repo.DefaultTip() == "" {
				return nil
			}
			return &gqlValue{kind: "Ref", refName: repo.DefaultBranch}
		case "viewerPermission":
			return "ADMIN"
		case "hasIssuesEnabled":
			return true
		case "issue":
			args := resolveVariables(field.args, ctx.variables).(map[string]interface{})
			num, _ := numArg(args, "number")
			issue := repo.Issues[int(num)]
			if issue == nil {
				return nil
			}
			return &gqlValue{kind: "Issue", repo: repo, issue: issue}
		case "issues":
			return resolveIssuesConnection(ctx, repo, field)
		case "pullRequests":
			return resolvePullRequestsConnection(ctx, repo, field)
		case "pullRequest":
			args := resolveVariables(field.args, ctx.variables).(map[string]interface{})
			num, _ := numArg(args, "number")
			pr := repo.PRs[int(num)]
			if pr == nil {
				return nil
			}
			return &gqlValue{kind: "PullRequest", repo: repo, pr: pr}
		}
		return fieldError(ctx, "Repository", name)
	case "Issue":
		issue := value.issue
		switch name {
		case "id":
			return ctx.store.GlobalID("Issue", value.repo.ID, issue.Number)
		case "number":
			return float64(issue.Number)
		case "title":
			return issue.Title
		case "state":
			return issueState(issue)
		case "body":
			return issue.Body
		case "createdAt":
			return trialNowISO
		case "url":
			return "/api/v3/repos/" + value.repo.Owner + "/" + value.repo.Name + "/issues/" + strconv.Itoa(issue.Number)
		case "author":
			return &gqlValue{kind: "User", login: ViewerLogin, id: ViewerID}
		case "labels":
			return &gqlValue{kind: "FileConnection", paths: append([]string(nil), issue.Labels...)}
		case "comments":
			paths := make([]string, 0, len(issue.Comments))
			for _, c := range issue.Comments {
				paths = append(paths, strconv.Itoa(c.ID))
			}
			return &gqlValue{kind: "FileConnection", paths: paths}
		}
		return fieldError(ctx, "Issue", name)
	case "PullRequest":
		pr := value.pr
		switch name {
		case "id":
			return ctx.store.GlobalID("PullRequest", value.repo.ID, pr.Number)
		case "number":
			return float64(pr.Number)
		case "title":
			return pr.Title
		case "body":
			return pr.Body
		case "state":
			return prState(pr)
		case "merged":
			return pr.Merged
		case "headRefName":
			return pr.HeadRef
		case "baseRefName":
			return pr.BaseRef
		case "mergeable":
			if value.repo.MergeConflict(pr) {
				return "CONFLICTING"
			}
			return "MERGEABLE"
		case "createdAt":
			return trialNowISO
		case "url":
			return "/api/v3/repos/" + value.repo.Owner + "/" + value.repo.Name + "/pull/" + strconv.Itoa(pr.Number)
		case "files":
			args := resolveVariables(field.args, ctx.variables).(map[string]interface{})
			first := 10
			if f, ok := numArg(args, "first"); ok {
				first = int(f)
			}
			diffs := value.repo.DiffFiles(pr.BaseSha, pr.HeadSha)
			paths := make([]string, 0, len(diffs))
			for _, d := range diffs {
				paths = append(paths, d.Filename)
			}
			if first < len(paths) {
				paths = paths[:first]
			}
			return &gqlValue{kind: "FileConnection", paths: paths}
		}
		return fieldError(ctx, "PullRequest", name)
	case "IssueConnection":
		switch name {
		case "nodes":
			items := make([]interface{}, 0, len(value.issueItems))
			for _, issue := range value.issueItems {
				items = append(items, &gqlValue{kind: "Issue", repo: value.repo, issue: issue})
			}
			return items
		case "totalCount":
			return float64(len(value.issueItems))
		case "pageInfo":
			return &gqlValue{kind: "PageInfo", hasNextPage: value.hasNextPage, hasPrevPage: value.hasPrevPage, startCursor: value.startCursor, endCursor: value.endCursor}
		}
		return fieldError(ctx, "IssueConnection", name)
	case "PullRequestConnection":
		switch name {
		case "nodes":
			items := make([]interface{}, 0, len(value.prItems))
			for _, pr := range value.prItems {
				items = append(items, &gqlValue{kind: "PullRequest", repo: value.repo, pr: pr})
			}
			return items
		case "totalCount":
			return float64(len(value.prItems))
		case "pageInfo":
			var startCursor *string
			if len(value.prItems) > 0 {
				s := cursorFor(value.prItems[0].Number)
				startCursor = &s
			}
			return &gqlValue{kind: "PageInfo", hasNextPage: value.hasNextPage, hasPrevPage: false, startCursor: startCursor, endCursor: value.endCursor}
		}
		return fieldError(ctx, "PullRequestConnection", name)
	case "FileConnection":
		switch name {
		case "nodes":
			items := make([]interface{}, 0, len(value.paths))
			for _, p := range value.paths {
				items = append(items, &gqlValue{kind: "FileNode", path: p})
			}
			return items
		case "totalCount":
			return float64(len(value.paths))
		}
		return fieldError(ctx, "FileConnection", name)
	case "PageInfo":
		switch name {
		case "hasNextPage":
			return value.hasNextPage
		case "hasPreviousPage":
			return value.hasPrevPage
		case "startCursor":
			if value.startCursor == nil {
				return nil
			}
			return *value.startCursor
		case "endCursor":
			if value.endCursor == nil {
				return nil
			}
			return *value.endCursor
		}
		return fieldError(ctx, "PageInfo", name)
	case "Comment":
		switch name {
		case "id":
			return ctx.store.GlobalID("IssueComment", value.repo.ID, value.issue.Number, value.commentID)
		case "body":
			return value.body
		case "author":
			return &gqlValue{kind: "User", login: ViewerLogin, id: ViewerID}
		}
		return fieldError(ctx, "IssueComment", name)
	case "CommentEdge":
		switch name {
		case "node":
			return &gqlValue{kind: "Comment", repo: value.repo, issue: value.issue, commentID: value.commentID, body: value.body}
		case "cursor":
			return cursorFor(value.commentID)
		}
		return fieldError(ctx, "IssueCommentEdge", name)
	case "CreateIssuePayload":
		if name == "issue" {
			return &gqlValue{kind: "Issue", repo: value.repo, issue: value.issue}
		}
		return fieldError(ctx, "CreateIssuePayload", name)
	case "CloseIssuePayload":
		if name == "issue" {
			return &gqlValue{kind: "Issue", repo: value.repo, issue: value.issue}
		}
		return fieldError(ctx, "CloseIssuePayload", name)
	case "CreatePullRequestPayload":
		if name == "pullRequest" {
			return &gqlValue{kind: "PullRequest", repo: value.repo, pr: value.pr}
		}
		return fieldError(ctx, "CreatePullRequestPayload", name)
	case "MergePullRequestPayload":
		if name == "pullRequest" {
			return &gqlValue{kind: "PullRequest", repo: value.repo, pr: value.pr}
		}
		return fieldError(ctx, "MergePullRequestPayload", name)
	case "AddCommentPayload":
		switch name {
		case "commentEdge":
			return &gqlValue{kind: "CommentEdge", repo: value.repo, issue: value.issue, commentID: value.commentID, body: value.body}
		case "comment":
			return &gqlValue{kind: "Comment", repo: value.repo, issue: value.issue, commentID: value.commentID, body: value.body}
		}
		return fieldError(ctx, "AddCommentPayload", name)
	case "__Type":
		switch name {
		case "name":
			return value.typeName
		case "kind":
			if value.typeName == "SearchType" {
				return "ENUM"
			}
			return "OBJECT"
		case "fields":
			fields := gqlTypeFields[value.typeName]
			items := make([]interface{}, 0, len(fields))
			for _, f := range fields {
				items = append(items, &gqlValue{kind: "__Field", name: f})
			}
			return items
		case "enumValues":
			vals := gqlTypeEnumValues[value.typeName]
			items := make([]interface{}, 0, len(vals))
			for _, v := range vals {
				items = append(items, &gqlValue{kind: "__EnumValue", name: v})
			}
			return items
		}
		return fieldError(ctx, "__Type", name)
	default:
		if name == "name" {
			if value.name == "" {
				return nil
			}
			return value.name
		}
		if name == "path" {
			if value.path == "" {
				return nil
			}
			return value.path
		}
		return fieldError(ctx, typeName, name)
	}
}

func resolveIssuesConnection(ctx *execCtx, repo *Repo, field *fieldNode) *gqlValue {
	args := resolveVariables(field.args, ctx.variables).(map[string]interface{})
	first := 10
	if f, ok := numArg(args, "first"); ok {
		first = int(f)
	}
	after := -1
	hasAfter := false
	if s, ok := args["after"].(string); ok {
		if n, ok := cursorNumber(s); ok {
			after = n
			hasAfter = true
		}
	}
	var states map[string]bool
	if arr, ok := args["states"].([]interface{}); ok {
		states = make(map[string]bool, len(arr))
		for _, s := range arr {
			if str, ok := s.(string); ok {
				states[str] = true
			}
		}
	}
	direction := "DESC"
	if ob, ok := args["orderBy"].(map[string]interface{}); ok {
		if d, ok := ob["direction"].(string); ok {
			direction = d
		}
	}
	issues := make([]*Issue, 0, len(repo.Issues))
	for _, i := range repo.Issues {
		if states != nil && !states[issueState(i)] {
			continue
		}
		issues = append(issues, i)
	}
	if direction == "ASC" {
		sort.Slice(issues, func(a, b int) bool { return issues[a].CreatedSeq < issues[b].CreatedSeq })
	} else {
		sort.Slice(issues, func(a, b int) bool { return issues[a].CreatedSeq > issues[b].CreatedSeq })
	}
	if hasAfter {
		filtered := issues[:0]
		for _, i := range issues {
			if direction == "ASC" {
				if i.Number > after {
					filtered = append(filtered, i)
				}
			} else {
				if i.Number < after {
					filtered = append(filtered, i)
				}
			}
		}
		issues = filtered
	}
	page := issues
	if len(page) > first {
		page = page[:first]
	}
	hasNextPage := len(issues) > first
	var endCursor, startCursor *string
	if len(page) > 0 {
		e := cursorFor(page[len(page)-1].Number)
		s := cursorFor(page[0].Number)
		endCursor, startCursor = &e, &s
	}
	return &gqlValue{kind: "IssueConnection", repo: repo, issueItems: page, hasNextPage: hasNextPage, endCursor: endCursor, startCursor: startCursor}
}

func resolvePullRequestsConnection(ctx *execCtx, repo *Repo, field *fieldNode) *gqlValue {
	args := resolveVariables(field.args, ctx.variables).(map[string]interface{})
	first := 10
	if f, ok := numArg(args, "first"); ok {
		first = int(f)
	}
	prs := make([]*PullRequest, 0, len(repo.PRs))
	for _, p := range repo.PRs {
		prs = append(prs, p)
	}
	sort.Slice(prs, func(a, b int) bool { return prs[a].CreatedSeq > prs[b].CreatedSeq })
	page := prs
	if len(page) > first {
		page = page[:first]
	}
	hasNextPage := len(prs) > first
	var endCursor *string
	if len(page) > 0 {
		e := cursorFor(page[len(page)-1].Number)
		endCursor = &e
	}
	return &gqlValue{kind: "PullRequestConnection", repo: repo, prItems: page, hasNextPage: hasNextPage, endCursor: endCursor}
}

func mutateCreateIssue(ctx *execCtx, field *fieldNode) interface{} {
	args := resolveVariables(field.args, ctx.variables).(map[string]interface{})
	input, _ := args["input"].(map[string]interface{})
	resolved := ctx.store.ResolveNode(strArg(input, "repositoryId"))
	if resolved == nil {
		ctx.errors = append(ctx.errors, gqlError{Message: "createIssue: repository not found"})
		return nil
	}
	repo := resolved.Repo
	title := strArg(input, "title")
	if title == "" {
		ctx.errors = append(ctx.errors, gqlError{Message: "createIssue: title is required"})
		return nil
	}
	number := repo.NextNumber
	repo.NextNumber++
	repo.Seq++
	issue := &Issue{
		ID: ctx.store.NextID(), Number: number, Title: title, Body: strArg(input, "body"),
		State: "open", Labels: []string{}, CreatedSeq: repo.Seq, Comments: []*Comment{},
	}
	repo.Issues[number] = issue
	return &gqlValue{kind: "CreateIssuePayload", repo: repo, issue: issue}
}

func mutateCloseIssue(ctx *execCtx, field *fieldNode) interface{} {
	args := resolveVariables(field.args, ctx.variables).(map[string]interface{})
	input, _ := args["input"].(map[string]interface{})
	resolved := ctx.store.ResolveNode(strArg(input, "issueId"))
	if resolved == nil || resolved.Issue == nil {
		ctx.errors = append(ctx.errors, gqlError{Message: "closeIssue: issue not found"})
		return nil
	}
	resolved.Issue.State = "closed"
	return &gqlValue{kind: "CloseIssuePayload", repo: resolved.Repo, issue: resolved.Issue}
}

func mutateCreatePullRequest(ctx *execCtx, field *fieldNode) interface{} {
	args := resolveVariables(field.args, ctx.variables).(map[string]interface{})
	input, _ := args["input"].(map[string]interface{})
	resolved := ctx.store.ResolveNode(strArg(input, "repositoryId"))
	if resolved == nil {
		ctx.errors = append(ctx.errors, gqlError{Message: "createPullRequest: repository not found"})
		return nil
	}
	repo := resolved.Repo
	headRef := strArg(input, "headRefName")
	baseRef := strArg(input, "baseRefName")
	headSha := repo.BranchTip(headRef)
	baseSha := repo.BranchTip(baseRef)
	if headSha == "" || baseSha == "" {
		ctx.errors = append(ctx.errors, gqlError{Message: "createPullRequest: head or base branch not found"})
		return nil
	}
	number := repo.NextNumber
	repo.NextNumber++
	repo.Seq++
	pr := &PullRequest{
		ID: ctx.store.NextID(), Number: number, Title: strArg(input, "title"), Body: strArg(input, "body"),
		State: "open", HeadRef: headRef, HeadSha: headSha, BaseRef: baseRef, BaseSha: baseSha,
		CreatedSeq: repo.Seq,
	}
	repo.PRs[number] = pr
	return &gqlValue{kind: "CreatePullRequestPayload", repo: repo, pr: pr}
}

func mutateMergePullRequest(ctx *execCtx, field *fieldNode) interface{} {
	args := resolveVariables(field.args, ctx.variables).(map[string]interface{})
	input, _ := args["input"].(map[string]interface{})
	resolved := ctx.store.ResolveNode(strArg(input, "pullRequestId"))
	if resolved == nil || resolved.PR == nil {
		ctx.errors = append(ctx.errors, gqlError{Message: "mergePullRequest: pull request not found"})
		return nil
	}
	repo, pr := resolved.Repo, resolved.PR
	if pr.State == "closed" {
		ctx.errors = append(ctx.errors, gqlError{Message: "mergePullRequest: pull request is closed"})
		return nil
	}
	result := repo.SquashMerge(pr)
	if result.Conflict {
		ctx.errors = append(ctx.errors, gqlError{Message: "mergePullRequest: merge conflict"})
		return nil
	}
	pr.Merged = true
	pr.State = "closed"
	pr.MergedAt = trialNowISO
	pr.MergeCommitSha = result.Sha
	pr.BaseSha = result.Sha
	return &gqlValue{kind: "MergePullRequestPayload", repo: repo, pr: pr}
}

func mutateAddComment(ctx *execCtx, field *fieldNode) interface{} {
	args := resolveVariables(field.args, ctx.variables).(map[string]interface{})
	input, _ := args["input"].(map[string]interface{})
	resolved := ctx.store.ResolveNode(strArg(input, "subjectId"))
	if resolved == nil || resolved.Issue == nil {
		ctx.errors = append(ctx.errors, gqlError{Message: "addComment: subject not found"})
		return nil
	}
	body := strArg(input, "body")
	commentID := ctx.store.NextID()
	resolved.Repo.Seq++
	resolved.Issue.Comments = append(resolved.Issue.Comments, &Comment{ID: commentID, Body: body, CreatedSeq: resolved.Repo.Seq})
	return &gqlValue{kind: "AddCommentPayload", repo: resolved.Repo, issue: resolved.Issue, commentID: commentID, body: body}
}

func executeSelections(ctx *execCtx, value *gqlValue, selections []selNode) map[string]interface{} {
	out := map[string]interface{}{}
	for _, sel := range selections {
		if sel.isFragment {
			if sel.fragment.typeCondition != "" && sel.fragment.typeCondition != value.kind {
				continue
			}
			for k, v := range executeSelections(ctx, value, sel.fragment.selections) {
				out[k] = v
			}
			continue
		}
		f := sel.field
		key := f.name
		if f.alias != "" {
			key = f.alias
		}
		resolved := resolveField(ctx, value, f)
		if resolved == nil {
			out[key] = nil
			continue
		}
		switch v := resolved.(type) {
		case []interface{}:
			arr := make([]interface{}, 0, len(v))
			for _, item := range v {
				if gv, ok := item.(*gqlValue); ok && f.selections != nil {
					arr = append(arr, executeSelections(ctx, gv, f.selections))
				} else {
					arr = append(arr, item)
				}
			}
			out[key] = arr
		case *gqlValue:
			if f.selections != nil {
				out[key] = executeSelections(ctx, v, f.selections)
			} else {
				out[key] = v
			}
		default:
			out[key] = resolved
		}
	}
	return out
}

type gqlResult struct {
	Data   interface{} `json:"data"`
	Errors []gqlError  `json:"errors,omitempty"`
}

func executeGraphQL(store *Store, query string, variables map[string]interface{}) gqlResult {
	tokens, err := tokenize(query)
	if err != nil {
		return gqlResult{Data: nil, Errors: []gqlError{{Message: err.Error()}}}
	}
	p := &parser{tokens: tokens}
	ops, err := p.parseDocument()
	if err != nil {
		return gqlResult{Data: nil, Errors: []gqlError{{Message: err.Error()}}}
	}
	if len(ops) == 0 {
		return gqlResult{Data: nil, Errors: []gqlError{{Message: "graphql: empty document"}}}
	}
	op := ops[0]
	ctx := &execCtx{store: store, variables: variables}
	var root *gqlValue
	if op.typ == "mutation" {
		root = &gqlValue{kind: "Mutation"}
	} else {
		root = &gqlValue{kind: "Query"}
	}
	data := executeSelections(ctx, root, op.selections)
	res := gqlResult{Data: data}
	if len(ctx.errors) > 0 {
		res.Errors = ctx.errors
	}
	return res
}
