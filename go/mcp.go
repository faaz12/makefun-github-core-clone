// mcp.go — Model Context Protocol over Streamable HTTP (JSON-RPC 2.0).
// Port of mcp.ts. The tool list is a constant so two tools/list calls
// answer byte-identical.
package main

import (
	"encoding/json"
	"strings"
)

type mcpRequest struct {
	JSONRPC string                 `json:"jsonrpc"`
	ID      interface{}            `json:"id"`
	Method  string                 `json:"method"`
	Params  map[string]interface{} `json:"params"`
}

type mcpTool struct {
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	InputSchema map[string]interface{} `json:"inputSchema"`
}

func prop(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "string", "description": desc}
}

func propBool(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "boolean", "description": desc}
}

func propInt(desc string) map[string]interface{} {
	return map[string]interface{}{"type": "integer", "description": desc}
}

func propEnum(desc string, values []string) map[string]interface{} {
	vals := make([]interface{}, len(values))
	for i, v := range values {
		vals[i] = v
	}
	return map[string]interface{}{"type": "string", "description": desc, "enum": vals}
}

func schema(props map[string]interface{}, required []string) map[string]interface{} {
	return map[string]interface{}{
		"type": "object", "properties": props, "required": required,
	}
}

var mcpTools = []mcpTool{
	{"create_repo", "Create a new repository owned by the authenticated user.", schema(map[string]interface{}{
		"name": prop("Repository name"), "description": prop("Repository description"),
		"private": propBool("Whether the repository is private"),
	}, []string{"name"})},
	{"get_repo", "Get a repository by owner and name.", schema(map[string]interface{}{
		"owner": prop("Repository owner login"), "repo": prop("Repository name"),
		"full_name": prop("owner/repo"),
	}, []string{"owner", "repo"})},
	{"create_issue", "Create an issue in a repository.", schema(map[string]interface{}{
		"owner": prop("Repository owner login"), "repo": prop("Repository name"),
		"full_name": prop("owner/repo"), "title": prop("Issue title"), "body": prop("Issue body"),
	}, []string{"owner", "repo", "title"})},
	{"list_issues", "List issues in a repository.", schema(map[string]interface{}{
		"owner": prop("Repository owner login"), "repo": prop("Repository name"),
		"full_name": prop("owner/repo"),
		"state": propEnum("open, closed or all", []string{"open", "closed", "all"}),
	}, []string{"owner", "repo"})},
	{"create_pull_request", "Create a pull request in a repository.", schema(map[string]interface{}{
		"owner": prop("Repository owner login"), "repo": prop("Repository name"),
		"full_name": prop("owner/repo"), "title": prop("Pull request title"),
		"body": prop("Issue body"), "head": prop("Head branch name"), "base": prop("Base branch name"),
	}, []string{"owner", "repo", "title", "head", "base"})},
	{"merge_pull_request", "Merge a pull request with a squash merge.", schema(map[string]interface{}{
		"owner": prop("Repository owner login"), "repo": prop("Repository name"),
		"full_name": prop("owner/repo"),
		"pull_number": propInt("Pull request number"),
	}, []string{"owner", "repo", "pull_number"})},
}

// toolsListJSON is the pre-serialized tools/list result payload.
var toolsListJSON []byte

func initToolsList() {
	b, _ := json.Marshal(map[string]interface{}{"tools": mcpTools})
	toolsListJSON = b
}

var mcpServerInfo = map[string]interface{}{"name": "forge", "version": "1.0.0"}

func toolResult(result interface{}) map[string]interface{} {
	text, _ := json.Marshal(result)
	return map[string]interface{}{
		"content":           []interface{}{map[string]interface{}{"type": "text", "text": string(text)}},
		"structuredContent": result,
	}
}

func toolError(message string) map[string]interface{} {
	b, _ := json.Marshal(map[string]interface{}{"error": message})
	return map[string]interface{}{
		"content": []interface{}{map[string]interface{}{"type": "text", "text": string(b)}},
		"isError": true,
	}
}

func resolveRepoArg(ctx *Ctx, args map[string]interface{}) *Repo {
	owner := strVal(args["owner"], "")
	name := strVal(args["repo"], "")
	fullName := strVal(args["full_name"], "")
	if (owner == "" || name == "") && strings.Contains(fullName, "/") {
		parts := strings.Split(fullName, "/")
		if owner == "" {
			owner = parts[0]
		}
		if name == "" {
			name = parts[1]
		}
	}
	if owner == "" || name == "" {
		return nil
	}
	if owner != ViewerLogin {
		return nil
	}
	return ctx.Store.GetRepo(owner, name)
}

func callTool(ctx *Ctx, name string, args map[string]interface{}) map[string]interface{} {
	normalized := normalizeToolName(name)
	switch normalized {
	case "create_repo":
		repoName := strings.TrimSpace(strVal(args["name"], ""))
		if repoName == "" {
			return toolError("name is required")
		}
		if existing := ctx.Store.GetRepo(ViewerLogin, repoName); existing != nil {
			return toolError("A repository named " + repoName + " already exists")
		}
		repo := ctx.Store.CreateRepo(repoName, ViewerLogin, strVal(args["description"], ""), args["private"] == true)
		return toolResult(repoJSON(repo, ctx.BaseURL))
	case "get_repo":
		repo := resolveRepoArg(ctx, args)
		if repo == nil {
			return toolError("Repository not found")
		}
		return toolResult(repoJSON(repo, ctx.BaseURL))
	case "create_issue":
		repo := resolveRepoArg(ctx, args)
		if repo == nil {
			return toolError("Repository not found")
		}
		title := strings.TrimSpace(strVal(args["title"], ""))
		if title == "" {
			return toolError("title is required")
		}
		number := repo.NextNumber
		repo.NextNumber++
		repo.Seq++
		issue := &Issue{
			ID: ctx.Store.NextID(), Number: number, Title: title, Body: strVal(args["body"], ""),
			State: "open", Labels: []string{}, CreatedSeq: repo.Seq, Comments: []*Comment{},
		}
		repo.Issues[number] = issue
		return toolResult(issueJSON(repo, issue))
	case "list_issues":
		repo := resolveRepoArg(ctx, args)
		if repo == nil {
			return toolError("Repository not found")
		}
		state := strVal(args["state"], "open")
		issues := make([]*Issue, 0, len(repo.Issues))
		for _, i := range repo.Issues {
			if state != "all" && i.State != state {
				continue
			}
			issues = append(issues, i)
		}
		sortIssuesDesc(issues)
		out := make([]IssueJSON, 0, len(issues))
		for _, i := range issues {
			out = append(out, issueJSON(repo, i))
		}
		return toolResult(map[string]interface{}{"issues": out})
	case "create_pull_request":
		repo := resolveRepoArg(ctx, args)
		if repo == nil {
			return toolError("Repository not found")
		}
		title := strings.TrimSpace(strVal(args["title"], ""))
		head := strings.TrimSpace(strVal(args["head"], ""))
		base := strings.TrimSpace(strVal(args["base"], ""))
		if title == "" || head == "" || base == "" {
			return toolError("title, head and base are required")
		}
		headSha := repo.BranchTip(head)
		baseSha := repo.BranchTip(base)
		if headSha == "" {
			return toolError("head branch " + head + " does not exist")
		}
		if baseSha == "" {
			return toolError("base branch " + base + " does not exist")
		}
		number := repo.NextNumber
		repo.NextNumber++
		repo.Seq++
		pr := &PullRequest{
			ID: ctx.Store.NextID(), Number: number, Title: title, Body: strVal(args["body"], ""),
			State: "open", HeadRef: head, HeadSha: headSha, BaseRef: base, BaseSha: baseSha,
			CreatedSeq: repo.Seq,
		}
		repo.PRs[number] = pr
		return toolResult(prJSON(repo, pr))
	case "merge_pull_request":
		repo := resolveRepoArg(ctx, args)
		if repo == nil {
			return toolError("Repository not found")
		}
		num := -1
		if f, ok := args["pull_number"].(float64); ok {
			num = int(f)
		} else if f, ok := args["pullNumber"].(float64); ok {
			num = int(f)
		}
		pr := repo.PRs[num]
		if pr == nil {
			return toolError("Pull request not found")
		}
		if pr.State == "closed" {
			return toolError("Pull request is closed")
		}
		result := repo.SquashMerge(pr)
		if result.Conflict {
			return toolError("Merge conflict")
		}
		pr.Merged = true
		pr.State = "closed"
		pr.MergedAt = trialNowISO
		pr.MergeCommitSha = result.Sha
		pr.BaseSha = result.Sha
		return toolResult(map[string]interface{}{
			"merged": true, "sha": result.Sha,
			"message": "Pull request #" + itoa(pr.Number) + " successfully merged",
		})
	}
	return toolError("Unknown tool: " + name)
}

func normalizeToolName(name string) string {
	lower := strings.ToLower(name)
	var sb strings.Builder
	for _, c := range lower {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			sb.WriteRune(c)
		} else {
			sb.WriteByte('_')
		}
	}
	return sb.String()
}

func sortIssuesDesc(issues []*Issue) {
	// insertion sort is fine (small); use sort.Slice for clarity.
	for i := 1; i < len(issues); i++ {
		for j := i; j > 0 && issues[j].CreatedSeq > issues[j-1].CreatedSeq; j-- {
			issues[j], issues[j-1] = issues[j-1], issues[j]
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

type mcpOutcome struct {
	Status         int
	Body           []byte
	IsNotification bool
}

func handleMcp(ctx *Ctx) mcpOutcome {
	req, ok := ctx.Body.(map[string]interface{})
	if !ok {
		return mcpOutcome{Status: 400, Body: mustJSON(map[string]interface{}{
			"jsonrpc": "2.0", "id": nil,
			"error": map[string]interface{}{"code": -32700, "message": "Parse error"},
		})}
	}
	id, hasID := req["id"]
	if !hasID || id == nil {
		// Notification (no id): acknowledge, no JSON-RPC body.
		return mcpOutcome{Status: 202, IsNotification: true}
	}
	method := strVal(req["method"], "")
	result := func(r interface{}) mcpOutcome {
		return mcpOutcome{Status: 200, Body: mustJSON(map[string]interface{}{
			"jsonrpc": "2.0", "id": id, "result": r,
		})}
	}
	errResp := func(code int, message string) mcpOutcome {
		return mcpOutcome{Status: 200, Body: mustJSON(map[string]interface{}{
			"jsonrpc": "2.0", "id": id,
			"error": map[string]interface{}{"code": code, "message": message},
		})}
	}

	switch method {
	case "server/discover":
		return result(map[string]interface{}{
			"serverInfo":        mcpServerInfo,
			"capabilities":      map[string]interface{}{"tools": map[string]interface{}{}},
			"supportedVersions": []string{"2026-07-28", "2025-06-18"},
			"protocolVersion":   "2026-07-28",
			"_meta":             map[string]interface{}{"io.modelcontextprotocol/serverInfo": mcpServerInfo},
		})
	case "initialize":
		return result(map[string]interface{}{
			"protocolVersion": "2026-07-28",
			"serverInfo":      mcpServerInfo,
			"capabilities":    map[string]interface{}{"tools": map[string]interface{}{}},
		})
	case "ping":
		return result(map[string]interface{}{})
	case "tools/list":
		// Byte-identical every time: the result payload is a pre-serialized constant,
		// embedded verbatim in the envelope.
		idJSON, _ := json.Marshal(id)
		body := make([]byte, 0, len(toolsListJSON)+64)
		body = append(body, []byte(`{"jsonrpc":"2.0","id":`)...)
		body = append(body, idJSON...)
		body = append(body, []byte(`,"result":`)...)
		body = append(body, toolsListJSON...)
		body = append(body, '}')
		return mcpOutcome{Status: 200, Body: body}
	case "tools/call":
		params, _ := req["params"].(map[string]interface{})
		toolName := strVal(params["name"], "")
		args, _ := params["arguments"].(map[string]interface{})
		known := false
		for _, t := range mcpTools {
			if t.Name == normalizeToolName(toolName) {
				known = true
				break
			}
		}
		if !known {
			return errResp(-32602, "Unknown tool: "+toolName)
		}
		res := callTool(ctx, toolName, args)
		if isErr, _ := res["isError"].(bool); isErr {
			text := ""
			if content, ok := res["content"].([]interface{}); ok && len(content) > 0 {
				if first, ok := content[0].(map[string]interface{}); ok {
					text, _ = first["text"].(string)
				}
			}
			return errResp(-32602, text)
		}
		return result(res)
	}
	return errResp(-32601, "Method not found: "+method)
}
