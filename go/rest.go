// rest.go — GitHub REST v3 surface: repos, refs, branches, commits,
// contents, labels, issues, comments, pull requests. Port of rest.ts.
package main

import (
	"encoding/base64"
	"encoding/json"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Ctx struct {
	Store   *Store
	Method  string
	URL     *url.URL
	Params  map[string]string
	Body    interface{}
	BaseURL string
}

type Resp struct {
	Status      int
	Body        []byte // JSON bytes or raw binary
	ContentType string
	Headers     map[string]string
}

func jsonResp(status int, v interface{}) Resp {
	b, err := json.Marshal(v)
	if err != nil {
		return Resp{Status: 500, Body: []byte(`{"message":"Internal Server Error"}`), ContentType: "application/json"}
	}
	return Resp{Status: status, Body: b, ContentType: "application/json"}
}

func jsonErr(status int, msg string) Resp {
	return jsonResp(status, map[string]string{"message": msg})
}

func notFoundResp(msg string) Resp {
	if msg == "" {
		msg = "Not Found"
	}
	return jsonErr(404, msg)
}

func isRecord(v interface{}) bool {
	_, ok := v.(map[string]interface{})
	return ok
}

func bodyOf(ctx *Ctx) map[string]interface{} {
	if m, ok := ctx.Body.(map[string]interface{}); ok {
		return m
	}
	return map[string]interface{}{}
}

func strVal(v interface{}, fallback string) string {
	if s, ok := v.(string); ok {
		return s
	}
	return fallback
}

func boolVal(v interface{}, fallback bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return fallback
}

func numVal(v interface{}) (float64, bool) {
	f, ok := v.(float64)
	return f, ok
}

// ---------------------------------------------------------------------------
// JSON shapes (field order matches the TS output)
// ---------------------------------------------------------------------------

type UserJSON struct {
	Login    string `json:"login"`
	ID       int    `json:"id"`
	NodeID   string `json:"node_id"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	HTMLURL  string `json:"html_url"`
	URL      string `json:"url"`
	SiteAdmin bool  `json:"site_admin"`
}

func userJSON() UserJSON {
	return UserJSON{
		Login: ViewerLogin, ID: ViewerID, NodeID: "U_kgDOAAAD6A",
		Type: "User", Name: ViewerName, Email: ViewerEmail,
		HTMLURL: "https://example.invalid/" + ViewerLogin,
		URL:     "/api/v3/users/" + ViewerLogin, SiteAdmin: false,
	}
}

type OwnerJSON struct {
	Login string `json:"login"`
	ID    int    `json:"id"`
	Type  string `json:"type"`
}

type RepoJSON struct {
	ID              int       `json:"id"`
	NodeID          string    `json:"node_id"`
	Name            string    `json:"name"`
	FullName        string    `json:"full_name"`
	Owner           OwnerJSON `json:"owner"`
	Private         bool      `json:"private"`
	Description     string    `json:"description"`
	Fork            bool      `json:"fork"`
	HTMLURL         string    `json:"html_url"`
	URL             string    `json:"url"`
	CreatedAt       string    `json:"created_at"`
	UpdatedAt       string    `json:"updated_at"`
	PushedAt        string    `json:"pushed_at"`
	DefaultBranch   string    `json:"default_branch"`
	HasIssues       bool      `json:"has_issues"`
	HasProjects     bool      `json:"has_projects"`
	HasWiki         bool      `json:"has_wiki"`
	HasPages        bool      `json:"has_pages"`
	OpenIssuesCount int       `json:"open_issues_count"`
	WatchersCount   int       `json:"watchers_count"`
	ForksCount      int       `json:"forks_count"`
	StargazersCount int       `json:"stargazers_count"`
	Size            int       `json:"size"`
	Language        *string   `json:"language"`
	Topics          []string  `json:"topics"`
	Visibility      string    `json:"visibility"`
	Archived        bool      `json:"archived"`
	Disabled        bool      `json:"disabled"`
}

func repoJSON(repo *Repo, baseURL string) RepoJSON {
	open := 0
	for _, i := range repo.Issues {
		if i.State == "open" {
			open++
		}
	}
	vis := "public"
	if repo.IsPrivate {
		vis = "private"
	}
	return RepoJSON{
		ID: repo.ID, NodeID: "R_kgDO" + strconv.Itoa(repo.ID), Name: repo.Name,
		FullName: repo.Owner + "/" + repo.Name,
		Owner:    OwnerJSON{Login: repo.Owner, ID: ViewerID, Type: "User"},
		Private:  repo.IsPrivate, Description: repo.Description, Fork: false,
		HTMLURL: "https://example.invalid/" + repo.Owner + "/" + repo.Name,
		URL:     baseURL + "/api/v3/repos/" + repo.Owner + "/" + repo.Name,
		CreatedAt: trialNowISO, UpdatedAt: trialNowISO, PushedAt: trialNowISO,
		DefaultBranch: repo.DefaultBranch,
		HasIssues: true, HasProjects: false, HasWiki: false, HasPages: false,
		OpenIssuesCount: open,
		Language:        nil, Topics: []string{},
		Visibility: vis, Archived: false, Disabled: false,
	}
}

type LabelJSON struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
	Default     bool   `json:"default"`
}

func labelJSON(l *Label) LabelJSON {
	return LabelJSON{ID: l.ID, Name: l.Name, Color: l.Color, Description: "", Default: false}
}

type IssueLabelJSON struct {
	Name        string `json:"name"`
	Color       string `json:"color"`
	Description string `json:"description"`
}

type IssueJSON struct {
	ID        int              `json:"id"`
	Number    int              `json:"number"`
	Title     string           `json:"title"`
	Body      string           `json:"body"`
	State     string           `json:"state"`
	Labels    []IssueLabelJSON `json:"labels"`
	User      OwnerJSON        `json:"user"`
	Assignees []interface{}    `json:"assignees"`
	CreatedAt string           `json:"created_at"`
	UpdatedAt string           `json:"updated_at"`
	ClosedAt  *string          `json:"closed_at"`
	Comments  int              `json:"comments"`
	HTMLURL   string           `json:"html_url"`
	URL       string           `json:"url"`
}

func issueJSON(repo *Repo, issue *Issue) IssueJSON {
	labels := make([]IssueLabelJSON, 0, len(issue.Labels))
	for _, name := range issue.Labels {
		color := "ededed"
		if l, ok := repo.Labels[name]; ok {
			color = l.Color
		}
		labels = append(labels, IssueLabelJSON{Name: name, Color: color, Description: ""})
	}
	var closedAt *string
	if issue.State == "closed" {
		s := trialNowISO
		closedAt = &s
	}
	return IssueJSON{
		ID: issue.ID, Number: issue.Number, Title: issue.Title, Body: issue.Body,
		State: issue.State, Labels: labels,
		User: OwnerJSON{Login: ViewerLogin, ID: ViewerID, Type: "User"},
		Assignees: []interface{}{},
		CreatedAt: trialNowISO, UpdatedAt: trialNowISO, ClosedAt: closedAt,
		Comments: len(issue.Comments),
		HTMLURL: "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number),
		URL:     "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number),
	}
}

type CommentJSON struct {
	ID        int       `json:"id"`
	Body      string    `json:"body"`
	User      OwnerJSON `json:"user"`
	CreatedAt string    `json:"created_at"`
	UpdatedAt string    `json:"updated_at"`
	HTMLURL   string    `json:"html_url"`
	URL       string    `json:"url"`
}

func commentJSON(repo *Repo, issue *Issue, c *Comment) CommentJSON {
	return CommentJSON{
		ID: c.ID, Body: c.Body,
		User: OwnerJSON{Login: ViewerLogin, ID: ViewerID, Type: "User"},
		CreatedAt: trialNowISO, UpdatedAt: trialNowISO,
		HTMLURL: "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number) + "#issuecomment-" + strconv.Itoa(c.ID),
		URL:     "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/issues/comments/" + strconv.Itoa(c.ID),
	}
}

type PRRefJSON struct {
	Ref   string   `json:"ref"`
	Sha   string   `json:"sha"`
	Label string   `json:"label"`
	User  OwnerJSON `json:"user"`
	Repo  RepoJSON `json:"repo"`
}

type PRJSON struct {
	ID             int        `json:"id"`
	Number         int        `json:"number"`
	Title          string     `json:"title"`
	Body           string     `json:"body"`
	State          string     `json:"state"`
	Locked         bool       `json:"locked"`
	TitleDraft     bool       `json:"title_draft"`
	User           OwnerJSON  `json:"user"`
	Head           PRRefJSON  `json:"head"`
	Base           PRRefJSON  `json:"base"`
	Merged         bool       `json:"merged"`
	Mergeable      bool       `json:"mergeable"`
	MergedAt       *string    `json:"merged_at"`
	MergeCommitSha *string    `json:"merge_commit_sha"`
	CreatedAt      string     `json:"created_at"`
	UpdatedAt      string     `json:"updated_at"`
	ClosedAt       *string    `json:"closed_at"`
	HTMLURL        string     `json:"html_url"`
	URL            string     `json:"url"`
}

func prJSON(repo *Repo, pr *PullRequest) PRJSON {
	var mergedAt, mergeSha, closedAt *string
	if pr.MergedAt != "" {
		s := pr.MergedAt
		mergedAt = &s
	}
	if pr.MergeCommitSha != "" {
		s := pr.MergeCommitSha
		mergeSha = &s
	}
	if pr.State == "closed" {
		s := trialNowISO
		closedAt = &s
	}
	head := PRRefJSON{
		Ref: pr.HeadRef, Sha: pr.HeadSha, Label: ViewerLogin + ":" + pr.HeadRef,
		User: OwnerJSON{Login: ViewerLogin, ID: ViewerID, Type: "User"},
		Repo: repoJSON(repo, ""),
	}
	base := PRRefJSON{
		Ref: pr.BaseRef, Sha: pr.BaseSha, Label: ViewerLogin + ":" + pr.BaseRef,
		User: OwnerJSON{Login: ViewerLogin, ID: ViewerID, Type: "User"},
		Repo: repoJSON(repo, ""),
	}
	return PRJSON{
		ID: pr.ID, Number: pr.Number, Title: pr.Title, Body: pr.Body, State: pr.State,
		Locked: false, TitleDraft: false,
		User: OwnerJSON{Login: ViewerLogin, ID: ViewerID, Type: "User"},
		Head: head, Base: base,
		Merged: pr.Merged, Mergeable: true, MergedAt: mergedAt, MergeCommitSha: mergeSha,
		CreatedAt: trialNowISO, UpdatedAt: trialNowISO, ClosedAt: closedAt,
		HTMLURL: "https://example.invalid/" + repo.Owner + "/" + repo.Name + "/pull/" + strconv.Itoa(pr.Number),
		URL:     "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/pulls/" + strconv.Itoa(pr.Number),
	}
}

type RefObjectJSON struct {
	Sha  string `json:"sha"`
	Type string `json:"type"`
	URL  string `json:"url"`
}

type RefJSON struct {
	Ref    string        `json:"ref"`
	Object RefObjectJSON `json:"object"`
	URL    string        `json:"url"`
}

func refJSON(repo *Repo, ref, sha string) RefJSON {
	return RefJSON{
		Ref: ref,
		Object: RefObjectJSON{Sha: sha, Type: "commit",
			URL: "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/git/commits/" + sha},
		URL: "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/git/refs/" + ref,
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func resolveRepo(ctx *Ctx) *Repo {
	owner := ctx.Params["owner"]
	name := ctx.Params["repo"]
	if owner == "" || name == "" {
		return nil
	}
	if owner != ViewerLogin {
		return nil
	}
	return ctx.Store.GetRepo(owner, name)
}

// numParam mirrors JS Number(x): NaN/empty -> NaN; "2.5" -> 2.5.
func numParam(s string) float64 {
	if s == "" {
		return math.NaN()
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return math.NaN()
	}
	return f
}

func pagination(ctx *Ctx) (page, perPage int) {
	q := ctx.URL.Query()
	p := numParam(q.Get("page"))
	if math.IsNaN(p) || p == 0 {
		p = 1
	}
	page = int(math.Max(1, p))
	pp := numParam(q.Get("per_page"))
	if math.IsNaN(pp) || pp == 0 {
		pp = 30
	}
	perPage = int(math.Min(100, math.Max(1, pp)))
	return page, perPage
}

// ordered query pairs, preserving the TS URLSearchParams order.
type queryPairs struct {
	keys []string
	vals map[string]string
}

func parseQuery(raw string) *queryPairs {
	qp := &queryPairs{vals: map[string]string{}}
	if raw == "" {
		return qp
	}
	for _, pair := range strings.Split(raw, "&") {
		if pair == "" {
			continue
		}
		k, v := pair, ""
		if i := strings.IndexByte(pair, '='); i >= 0 {
			k, v = pair[:i], pair[i+1:]
		}
		dk, _ := url.QueryUnescape(strings.ReplaceAll(k, "+", " "))
		dv, _ := url.QueryUnescape(strings.ReplaceAll(v, "+", " "))
		if _, ok := qp.vals[dk]; !ok {
			qp.keys = append(qp.keys, dk)
		}
		qp.vals[dk] = dv
	}
	return qp
}

func (qp *queryPairs) set(k, v string) {
	if _, ok := qp.vals[k]; !ok {
		qp.keys = append(qp.keys, k)
	}
	qp.vals[k] = v
}

func (qp *queryPairs) encode() string {
	var sb strings.Builder
	for i, k := range qp.keys {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(url.QueryEscape(k))
		sb.WriteByte('=')
		sb.WriteString(url.QueryEscape(qp.vals[k]))
	}
	return sb.String()
}

func linkHeader(ctx *Ctx, page, perPage, total int) string {
	totalPages := int(math.Max(1, math.Ceil(float64(total)/float64(perPage))))
	var parts []string
	make := func(p int, rel string) {
		u := *ctx.URL
		qp := parseQuery(u.RawQuery)
		qp.set("page", strconv.Itoa(p))
		qp.set("per_page", strconv.Itoa(perPage))
		u.RawQuery = qp.encode()
		parts = append(parts, "<"+u.String()+">; rel=\""+rel+"\"")
	}
	if page < totalPages {
		make(page+1, "next")
	}
	if page > 1 {
		make(page-1, "prev")
	}
	make(1, "first")
	make(totalPages, "last")
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, ", ")
}

func paginateLen(total, page, perPage int) (start, end int) {
	start = (page - 1) * perPage
	if start < 0 {
		start = 0
	}
	if start > total {
		start = total
	}
	end = page * perPage
	if end > total {
		end = total
	}
	if end < start {
		end = start
	}
	return start, end
}

func isoTime(ts int64) string {
	return time.Unix(ts, 0).UTC().Format("2006-01-02T15:04:05.000Z")
}

// ---------------------------------------------------------------------------
// root / meta / user
// ---------------------------------------------------------------------------

func apiRoot(ctx *Ctx) Resp {
	return jsonResp(200, map[string]string{
		"current_user_url": ctx.BaseURL + "/api/v3/user",
		"repository_url":   ctx.BaseURL + "/api/v3/repos/{owner}/{repo}",
		"user_url":         ctx.BaseURL + "/api/v3/users/{user}",
		"meta":             ctx.BaseURL + "/api/v3/meta",
	})
}

func metaHandler(ctx *Ctx) Resp {
	return jsonResp(200, map[string]string{"installed_version": "1.0.0"})
}

func getUser(ctx *Ctx) Resp { return jsonResp(200, userJSON()) }

// ---------------------------------------------------------------------------
// repos
// ---------------------------------------------------------------------------

func createUserRepo(ctx *Ctx) Resp {
	b := bodyOf(ctx)
	name := strings.TrimSpace(strVal(b["name"], ""))
	if name == "" {
		return jsonErr(400, "name is required")
	}
	if existing := ctx.Store.GetRepo(ViewerLogin, name); existing != nil {
		return jsonErr(422, "A repository named "+name+" already exists")
	}
	repo := ctx.Store.CreateRepo(name, ViewerLogin, strVal(b["description"], ""), boolVal(b["private"], false))
	return jsonResp(201, repoJSON(repo, ctx.BaseURL))
}

func listUserRepos(ctx *Ctx) Resp {
	page, perPage := pagination(ctx)
	repos := ctx.Store.ListRepos(ViewerLogin)
	link := linkHeader(ctx, page, perPage, len(repos))
	headers := map[string]string{}
	if link != "" {
		headers["Link"] = link
	}
	start, end := paginateLen(len(repos), page, perPage)
	out := make([]RepoJSON, 0, end-start)
	for _, r := range repos[start:end] {
		out = append(out, repoJSON(r, ctx.BaseURL))
	}
	return Resp{Status: 200, Body: mustJSON(out), ContentType: "application/json", Headers: headers}
}

func mustJSON(v interface{}) []byte {
	b, _ := json.Marshal(v)
	return b
}

func b64encode(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

func b64decode(s string) ([]byte, error) {
	return base64.StdEncoding.DecodeString(s)
}

func getRepo(ctx *Ctx) Resp {
	repo := resolveRepo(ctx)
	if repo == nil {
		return notFoundResp("Repository not found")
	}
	return jsonResp(200, repoJSON(repo, ctx.BaseURL))
}

func patchRepo(ctx *Ctx) Resp {
	repo := resolveRepo(ctx)
	if repo == nil {
		return notFoundResp("Repository not found")
	}
	b := bodyOf(ctx)
	if d, ok := b["description"].(string); ok {
		repo.Description = d
	}
	if p, ok := b["private"].(bool); ok {
		repo.IsPrivate = p
	}
	if db, ok := b["default_branch"].(string); ok && db != "" {
		repo.DefaultBranch = db
	}
	return jsonResp(200, repoJSON(repo, ctx.BaseURL))
}

func deleteRepo(ctx *Ctx) Resp {
	repo := resolveRepo(ctx)
	if repo == nil {
		return notFoundResp("Repository not found")
	}
	ctx.Store.DeleteRepo(repo.Owner, repo.Name)
	return Resp{Status: 204, Body: nil, ContentType: ""}
}

// ---------------------------------------------------------------------------
// git refs / branches / commits
// ---------------------------------------------------------------------------

func isHex40(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func createRef(ctx *Ctx) Resp {
	repo := resolveRepo(ctx)
	if repo == nil {
		return notFoundResp("Repository not found")
	}
	b := bodyOf(ctx)
	ref := strVal(b["ref"], "")
	sha := strVal(b["sha"], "")
	if ref == "" || sha == "" {
		return jsonErr(400, "ref and sha are required")
	}
	// mirror /^refs\/(heads|tags)\/.+/
	if !validRefName(ref) {
		return jsonErr(422, "ref must be refs/heads/<name> or refs/tags/<name>")
	}
	if !isHex40(sha) {
		return jsonErr(422, "sha is not a valid object")
	}
	if _, exists := repo.Refs[ref]; exists {
		return jsonErr(422, "Reference "+ref+" already exists")
	}
	obj := repo.GetObject(sha)
	if obj == nil || obj.Type != ObjCommit {
		return jsonErr(422, "sha does not reference a commit")
	}
	repo.Refs[ref] = sha
	return jsonResp(201, refJSON(repo, ref, sha))
}

func validRefName(ref string) bool {
	if strings.HasPrefix(ref, "refs/heads/") {
		return len(ref) > len("refs/heads/")
	}
	if strings.HasPrefix(ref, "refs/tags/") {
		return len(ref) > len("refs/tags/")
	}
	return false
}

func getRef(ctx *Ctx) Resp {
	repo := resolveRepo(ctx)
	if repo == nil {
		return notFoundResp("Repository not found")
	}
	ref := "refs/" + ctx.Params["path"]
	sha, ok := repo.Refs[ref]
	if !ok {
		return notFoundResp("Reference not found")
	}
	return jsonResp(200, refJSON(repo, ref, sha))
}

func listBranches(ctx *Ctx) Resp {
	repo := resolveRepo(ctx)
	if repo == nil {
		return notFoundResp("Repository not found")
	}
	type branch struct {
		Name      string `json:"name"`
		Commit    struct {
			Sha string `json:"sha"`
		} `json:"commit"`
		Protected bool `json:"protected"`
	}
	out := make([]branch, 0)
	for _, b := range repo.Branches() {
		var br branch
		br.Name = b.Name
		br.Commit.Sha = b.Sha
		out = append(out, br)
	}
	return jsonResp(200, out)
}

func getBranch(ctx *Ctx) Resp {
	repo := resolveRepo(ctx)
	if repo == nil {
		return notFoundResp("Repository not found")
	}
	sha := repo.BranchTip(ctx.Params["branch"])
	if sha == "" {
		return notFoundResp("Branch not found")
	}
	return jsonResp(200, map[string]interface{}{
		"name": ctx.Params["branch"],
		"commit": map[string]string{"sha": sha},
		"protected": false,
	})
}

func listCommits(ctx *Ctx) Resp {
	repo := resolveRepo(ctx)
	if repo == nil {
		return notFoundResp("Repository not found")
	}
	shaParam := ctx.URL.Query().Get("sha")
	var tip string
	if shaParam != "" {
		if isHex40(shaParam) {
			tip = shaParam
		} else {
			tip = repo.BranchTip(shaParam)
		}
		if tip == "" {
			return notFoundResp("Reference not found")
		}
	} else {
		tip = repo.DefaultTip()
		if tip == "" {
			return jsonResp(200, []interface{}{})
		}
	}
	commits := repo.ListCommits(tip)
	page, perPage := pagination(ctx)
	start, end := paginateLen(len(commits), page, perPage)
	type commitJSON struct {
		Sha    string `json:"sha"`
		Commit struct {
			Message   string `json:"message"`
			Author    struct {
				Name  string `json:"name"`
				Email string `json:"email"`
				Date  string `json:"date"`
			} `json:"author"`
			Committer struct {
				Name  string `json:"name"`
				Email string `json:"email"`
				Date  string `json:"date"`
			} `json:"committer"`
			Tree struct {
				Sha string `json:"sha"`
			} `json:"tree"`
		} `json:"commit"`
		Author    OwnerJSON `json:"author"`
		Committer OwnerJSON `json:"committer"`
		Parents   []struct {
			Sha string `json:"sha"`
		} `json:"parents"`
		URL string `json:"url"`
	}
	out := make([]commitJSON, 0, end-start)
	for _, c := range commits[start:end] {
		var cj commitJSON
		cj.Sha = c.Sha
		cj.Commit.Message = c.Commit.Message
		cj.Commit.Author.Name = c.Commit.AuthorName
		cj.Commit.Author.Email = c.Commit.AuthorEmail
		cj.Commit.Author.Date = isoTime(c.Commit.AuthorTs)
		cj.Commit.Committer.Name = c.Commit.CommitterName
		cj.Commit.Committer.Email = c.Commit.CommitterEmail
		cj.Commit.Committer.Date = isoTime(c.Commit.CommitterTs)
		cj.Commit.Tree.Sha = c.Commit.Tree
		cj.Author = OwnerJSON{Login: ViewerLogin, ID: ViewerID, Type: "User"}
		cj.Committer = OwnerJSON{Login: ViewerLogin, ID: ViewerID, Type: "User"}
		for _, p := range c.Commit.Parents {
			cj.Parents = append(cj.Parents, struct {
				Sha string `json:"sha"`
			}{Sha: p})
		}
		if cj.Parents == nil {
			cj.Parents = []struct {
				Sha string `json:"sha"`
			}{}
		}
		cj.URL = "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/commits/" + c.Sha
		out = append(out, cj)
	}
	return jsonResp(200, out)
}

// ---------------------------------------------------------------------------
// contents
// ---------------------------------------------------------------------------

func readTreeAt(repo *Repo, refParam string, hasRef bool) (map[string]FlatFile, bool) {
	var tip string
	if hasRef {
		if isHex40(refParam) {
			tip = refParam
		} else {
			tip = repo.BranchTip(refParam)
		}
		if tip == "" {
			return nil, false
		}
	} else {
		tip = repo.DefaultTip()
		if tip == "" {
			return map[string]FlatFile{}, true
		}
	}
	obj := repo.GetObject(tip)
	if obj == nil || obj.Type != ObjCommit {
		return nil, false
	}
	parts := parseCommit(obj.Content)
	return flattenTree(repo, parts.Tree), true
}

type contentEntry struct {
	Type string `json:"type"`
	Name string `json:"name"`
	Path string `json:"path"`
	Sha  string `json:"sha"`
	Size int    `json:"size"`
	URL  string `json:"url"`
}

func getContents(ctx *Ctx) Resp {
	repo := resolveRepo(ctx)
	if repo == nil {
		return notFoundResp("Repository not found")
	}
	path := ctx.Params["path"]
	q := ctx.URL.Query()
	refParam, hasRef := q["ref"]
	ref := ""
	if hasRef {
		ref = refParam[0]
	}
	files, ok := readTreeAt(repo, ref, hasRef)
	if !ok {
		return notFoundResp("Reference not found")
	}
	base := "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/contents/"
	if path == "" {
		entries := []contentEntry{}
		dirs := map[string]bool{}
		for p, f := range files {
			if slash := strings.IndexByte(p, '/'); slash < 0 {
				size := 0
				if o := repo.GetObject(f.OID); o != nil {
					size = len(o.Content)
				}
				entries = append(entries, contentEntry{Type: "file", Name: p, Path: p, Sha: f.OID, Size: size, URL: base + p})
			} else {
				dirs[p[:slash]] = true
			}
		}
		for d := range dirs {
			entries = append(entries, contentEntry{Type: "dir", Name: d, Path: d, Sha: "", Size: 0, URL: base + d})
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
		return jsonResp(200, entries)
	}
	if file, ok := files[path]; ok {
		content := []byte{}
		if o := repo.GetObject(file.OID); o != nil {
			content = o.Content
		}
		return jsonResp(200, map[string]interface{}{
			"type": "file", "encoding": "base64", "size": len(content),
			"name": path[strings.LastIndexByte(path, '/')+1:], "path": path,
			"content": b64encode(content), "sha": file.OID,
			"url": base + path,
		})
	}
	prefix := path + "/"
	children := []contentEntry{}
	dirs := map[string]bool{}
	for p, f := range files {
		if !strings.HasPrefix(p, prefix) {
			continue
		}
		rest := p[len(prefix):]
		if slash := strings.IndexByte(rest, '/'); slash < 0 {
			size := 0
			if o := repo.GetObject(f.OID); o != nil {
				size = len(o.Content)
			}
			children = append(children, contentEntry{Type: "file", Name: rest, Path: p, Sha: f.OID, Size: size, URL: base + p})
		} else {
			dirs[rest[:slash]] = true
		}
	}
	if len(children) > 0 || len(dirs) > 0 {
		for d := range dirs {
			children = append(children, contentEntry{Type: "dir", Name: d, Path: path + "/" + d, Sha: "", Size: 0, URL: base + path + "/" + d})
		}
		sort.Slice(children, func(i, j int) bool { return children[i].Name < children[j].Name })
		return jsonResp(200, children)
	}
	return notFoundResp("Not Found")
}

func putContents(ctx *Ctx) Resp {
	repo := resolveRepo(ctx)
	if repo == nil {
		return notFoundResp("Repository not found")
	}
	path := ctx.Params["path"]
	if path == "" {
		return jsonErr(400, "path is required")
	}
	b := bodyOf(ctx)
	message := strVal(b["message"], "")
	contentB64 := strVal(b["content"], "")
	if message == "" {
		return jsonErr(400, "message is required")
	}
	if contentB64 == "" {
		return jsonErr(400, "content is required")
	}
	content, err := b64decode(contentB64)
	if err != nil {
		return jsonErr(400, "content is not valid base64")
	}
	branch := strVal(b["branch"], repo.DefaultBranch)
	if branch == "" {
		branch = repo.DefaultBranch
	}
	sha := strVal(b["sha"], "")
	tip := repo.BranchTip(branch)
	if tip == "" {
		tip = repo.DefaultTip()
	}
	if sha != "" && tip != "" {
		if obj := repo.GetObject(tip); obj != nil && obj.Type == ObjCommit {
			files := flattenTree(repo, parseCommit(obj.Content).Tree)
			cur, ok := files[path]
			if !ok {
				return jsonErr(422, "sha does not match the current file")
			}
			if cur.OID != sha {
				return jsonErr(409, "sha does not match the current file")
			}
		}
	}
	result := repo.CommitFile(branch, path, content, true, message)
	status := 201
	if result.Existed {
		status = 200
	}
	name := path[strings.LastIndexByte(path, '/')+1:]
	return jsonResp(status, map[string]interface{}{
		"content": map[string]interface{}{
			"name": name, "path": path, "sha": result.BlobOID, "size": len(content),
			"type": "file", "encoding": "base64",
		},
		"commit": map[string]interface{}{
			"sha": result.CommitOID, "message": message,
			"author": map[string]string{"name": ViewerName, "email": ViewerEmail, "date": trialNowISO},
			"committer": map[string]string{"name": ViewerName, "email": ViewerEmail, "date": trialNowISO},
		},
	})
}

func deleteContents(ctx *Ctx) Resp {
	repo := resolveRepo(ctx)
	if repo == nil {
		return notFoundResp("Repository not found")
	}
	path := ctx.Params["path"]
	if path == "" {
		return jsonErr(400, "path is required")
	}
	b := bodyOf(ctx)
	message := strVal(b["message"], "")
	if message == "" {
		return jsonErr(400, "message is required")
	}
	branch := strVal(b["branch"], repo.DefaultBranch)
	if branch == "" {
		branch = repo.DefaultBranch
	}
	sha := strVal(b["sha"], "")
	tip := repo.BranchTip(branch)
	if tip == "" {
		tip = repo.DefaultTip()
	}
	if tip == "" {
		return notFoundResp("Not Found")
	}
	obj := repo.GetObject(tip)
	if obj == nil || obj.Type != ObjCommit {
		return notFoundResp("Not Found")
	}
	files := flattenTree(repo, parseCommit(obj.Content).Tree)
	cur, ok := files[path]
	if !ok {
		return notFoundResp("Not Found")
	}
	if sha != "" && cur.OID != sha {
		return jsonErr(409, "sha does not match the current file")
	}
	result := repo.CommitFile(branch, path, nil, false, message)
	return jsonResp(200, map[string]interface{}{
		"commit": map[string]interface{}{
			"sha": result.CommitOID, "message": message,
			"author": map[string]string{"name": ViewerName, "email": ViewerEmail, "date": trialNowISO},
			"committer": map[string]string{"name": ViewerName, "email": ViewerEmail, "date": trialNowISO},
		},
	})
}

// ---------------------------------------------------------------------------
// labels
// ---------------------------------------------------------------------------

func createLabel(ctx *Ctx) Resp {
	repo := resolveRepo(ctx)
	if repo == nil {
		return notFoundResp("Repository not found")
	}
	b := bodyOf(ctx)
	name := strings.TrimSpace(strVal(b["name"], ""))
	if name == "" {
		return jsonErr(400, "name is required")
	}
	color := strings.TrimPrefix(strVal(b["color"], "ededed"), "#")
	if existing, ok := repo.Labels[name]; ok {
		return jsonResp(201, labelJSON(existing))
	}
	label := &Label{ID: ctx.Store.NextLabelID, Name: name, Color: color}
	ctx.Store.NextLabelID++
	repo.Labels[name] = label
	return jsonResp(201, labelJSON(label))
}

func listLabels(ctx *Ctx) Resp {
	repo := resolveRepo(ctx)
	if repo == nil {
		return notFoundResp("Repository not found")
	}
	out := make([]LabelJSON, 0, len(repo.Labels))
	for _, l := range repo.Labels {
		out = append(out, labelJSON(l))
	}
	return jsonResp(200, out)
}

// ---------------------------------------------------------------------------
// issues
// ---------------------------------------------------------------------------

func normalizeLabels(v interface{}) []string {
	arr, ok := v.([]interface{})
	if !ok {
		return []string{}
	}
	out := []string{}
	for _, item := range arr {
		switch t := item.(type) {
		case string:
			out = append(out, t)
		case map[string]interface{}:
			if name, ok := t["name"].(string); ok {
				out = append(out, name)
			}
		}
	}
	return out
}

func createIssue(ctx *Ctx) Resp {
	repo := resolveRepo(ctx)
	if repo == nil {
		return notFoundResp("Repository not found")
	}
	b := bodyOf(ctx)
	title := strings.TrimSpace(strVal(b["title"], ""))
	if title == "" {
		return jsonErr(400, "title is required")
	}
	labels := normalizeLabels(b["labels"])
	for _, name := range labels {
		if _, ok := repo.Labels[name]; !ok {
			return jsonErr(422, "Label '"+name+"' does not exist")
		}
	}
	number := repo.NextNumber
	repo.NextNumber++
	repo.Seq++
	issue := &Issue{
		ID: ctx.Store.NextID(), Number: number, Title: title, Body: strVal(b["body"], ""),
		State: "open", Labels: labels, CreatedSeq: repo.Seq, Comments: []*Comment{},
	}
	repo.Issues[number] = issue
	return jsonResp(201, issueJSON(repo, issue))
}

func listIssues(ctx *Ctx) Resp {
	repo := resolveRepo(ctx)
	if repo == nil {
		return notFoundResp("Repository not found")
	}
	q := ctx.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	sortBy := q.Get("sort")
	if sortBy == "" {
		sortBy = "created"
	}
	direction := q.Get("direction")
	if direction == "" {
		direction = "desc"
	}
	issues := make([]*Issue, 0, len(repo.Issues))
	for _, i := range repo.Issues {
		if state != "all" && i.State != state {
			continue
		}
		issues = append(issues, i)
	}
	if sortBy == "created" {
		if direction == "asc" {
			sort.Slice(issues, func(a, b int) bool { return issues[a].CreatedSeq < issues[b].CreatedSeq })
		} else {
			sort.Slice(issues, func(a, b int) bool { return issues[a].CreatedSeq > issues[b].CreatedSeq })
		}
	}
	page, perPage := pagination(ctx)
	link := linkHeader(ctx, page, perPage, len(issues))
	headers := map[string]string{}
	if link != "" {
		headers["Link"] = link
	}
	start, end := paginateLen(len(issues), page, perPage)
	out := make([]IssueJSON, 0, end-start)
	for _, i := range issues[start:end] {
		out = append(out, issueJSON(repo, i))
	}
	return Resp{Status: 200, Body: mustJSON(out), ContentType: "application/json", Headers: headers}
}

func getIssue(ctx *Ctx) Resp {
	repo := resolveRepo(ctx)
	if repo == nil {
		return notFoundResp("Repository not found")
	}
	num, _ := strconv.Atoi(ctx.Params["number"])
	issue, ok := repo.Issues[num]
	if !ok {
		return notFoundResp("Issue not found")
	}
	return jsonResp(200, issueJSON(repo, issue))
}

func patchIssue(ctx *Ctx) Resp {
	repo := resolveRepo(ctx)
	if repo == nil {
		return notFoundResp("Repository not found")
	}
	num, _ := strconv.Atoi(ctx.Params["number"])
	issue, ok := repo.Issues[num]
	if !ok {
		return notFoundResp("Issue not found")
	}
	b := bodyOf(ctx)
	if t, ok := b["title"].(string); ok {
		issue.Title = t
	}
	if bo, ok := b["body"].(string); ok {
		issue.Body = bo
	}
	if st, ok := b["state"].(string); ok && (st == "open" || st == "closed") {
		issue.State = st
	}
	if labelsRaw, present := b["labels"]; present {
		labels := normalizeLabels(labelsRaw)
		for _, name := range labels {
			if _, ok := repo.Labels[name]; !ok {
				return jsonErr(422, "Label '"+name+"' does not exist")
			}
		}
		issue.Labels = labels
	}
	return jsonResp(200, issueJSON(repo, issue))
}

func createIssueComment(ctx *Ctx) Resp {
	repo := resolveRepo(ctx)
	if repo == nil {
		return notFoundResp("Repository not found")
	}
	num, _ := strconv.Atoi(ctx.Params["number"])
	issue, ok := repo.Issues[num]
	if !ok {
		return notFoundResp("Issue not found")
	}
	b := bodyOf(ctx)
	body := strings.TrimSpace(strVal(b["body"], ""))
	if body == "" {
		return jsonErr(400, "body is required")
	}
	repo.Seq++
	comment := &Comment{ID: ctx.Store.NextID(), Body: body, CreatedSeq: repo.Seq}
	issue.Comments = append(issue.Comments, comment)
	return jsonResp(201, commentJSON(repo, issue, comment))
}

func listIssueComments(ctx *Ctx) Resp {
	repo := resolveRepo(ctx)
	if repo == nil {
		return notFoundResp("Repository not found")
	}
	num, _ := strconv.Atoi(ctx.Params["number"])
	issue, ok := repo.Issues[num]
	if !ok {
		return notFoundResp("Issue not found")
	}
	out := make([]CommentJSON, 0, len(issue.Comments))
	for _, c := range issue.Comments {
		out = append(out, commentJSON(repo, issue, c))
	}
	return jsonResp(200, out)
}

// ---------------------------------------------------------------------------
// pull requests
// ---------------------------------------------------------------------------

func createPullRequest(ctx *Ctx) Resp {
	repo := resolveRepo(ctx)
	if repo == nil {
		return notFoundResp("Repository not found")
	}
	b := bodyOf(ctx)
	title := strings.TrimSpace(strVal(b["title"], ""))
	head := strings.TrimSpace(strVal(b["head"], ""))
	base := strings.TrimSpace(strVal(b["base"], ""))
	if title == "" {
		return jsonErr(400, "title is required")
	}
	if head == "" || base == "" {
		return jsonErr(400, "head and base are required")
	}
	headSha := repo.BranchTip(head)
	baseSha := repo.BranchTip(base)
	if headSha == "" {
		return jsonErr(422, "head branch "+head+" does not exist")
	}
	if baseSha == "" {
		return jsonErr(422, "base branch "+base+" does not exist")
	}
	number := repo.NextNumber
	repo.NextNumber++
	repo.Seq++
	pr := &PullRequest{
		ID: ctx.Store.NextID(), Number: number, Title: title, Body: strVal(b["body"], ""),
		State: "open", HeadRef: head, HeadSha: headSha, BaseRef: base, BaseSha: baseSha,
		CreatedSeq: repo.Seq,
	}
	repo.PRs[number] = pr
	return jsonResp(201, prJSON(repo, pr))
}

func listPullRequests(ctx *Ctx) Resp {
	repo := resolveRepo(ctx)
	if repo == nil {
		return notFoundResp("Repository not found")
	}
	state := ctx.URL.Query().Get("state")
	if state == "" {
		state = "open"
	}
	prs := make([]*PullRequest, 0, len(repo.PRs))
	for _, p := range repo.PRs {
		if state != "all" && p.State != state {
			continue
		}
		prs = append(prs, p)
	}
	sort.Slice(prs, func(a, b int) bool { return prs[a].CreatedSeq > prs[b].CreatedSeq })
	page, perPage := pagination(ctx)
	start, end := paginateLen(len(prs), page, perPage)
	out := make([]PRJSON, 0, end-start)
	for _, p := range prs[start:end] {
		out = append(out, prJSON(repo, p))
	}
	return jsonResp(200, out)
}

func getPullRequest(ctx *Ctx) Resp {
	repo := resolveRepo(ctx)
	if repo == nil {
		return notFoundResp("Repository not found")
	}
	num, _ := strconv.Atoi(ctx.Params["number"])
	pr, ok := repo.PRs[num]
	if !ok {
		return notFoundResp("Pull request not found")
	}
	return jsonResp(200, prJSON(repo, pr))
}

func listPullRequestFiles(ctx *Ctx) Resp {
	repo := resolveRepo(ctx)
	if repo == nil {
		return notFoundResp("Repository not found")
	}
	num, _ := strconv.Atoi(ctx.Params["number"])
	pr, ok := repo.PRs[num]
	if !ok {
		return notFoundResp("Pull request not found")
	}
	files := repo.DiffFiles(pr.BaseSha, pr.HeadSha)
	type fileJSON struct {
		Sha       string `json:"sha"`
		Filename  string `json:"filename"`
		Status    string `json:"status"`
		Additions int    `json:"additions"`
		Deletions int    `json:"deletions"`
		Changes   int    `json:"changes"`
		BlobURL   string `json:"blob_url"`
		RawURL    string `json:"raw_url"`
		ContentsURL string `json:"contents_url"`
		Patch     string `json:"patch"`
	}
	out := make([]fileJSON, 0, len(files))
	for _, f := range files {
		out = append(out, fileJSON{
			Sha: "", Filename: f.Filename, Status: f.Status,
			Additions: f.Additions, Deletions: f.Deletions, Changes: f.Changes,
			BlobURL: "", RawURL: "", ContentsURL: "", Patch: "",
		})
	}
	return jsonResp(200, out)
}

func mergePullRequest(ctx *Ctx) Resp {
	repo := resolveRepo(ctx)
	if repo == nil {
		return notFoundResp("Repository not found")
	}
	num, _ := strconv.Atoi(ctx.Params["number"])
	pr, ok := repo.PRs[num]
	if !ok {
		return notFoundResp("Pull request not found")
	}
	if pr.State == "closed" {
		msg := "Pull request is closed"
		if pr.Merged {
			msg = "Pull request is already merged"
		}
		return jsonResp(405, map[string]string{"message": msg})
	}
	result := repo.SquashMerge(pr)
	if result.Conflict {
		return jsonResp(405, map[string]string{"message": "Merge conflict"})
	}
	pr.Merged = true
	pr.State = "closed"
	pr.MergedAt = trialNowISO
	pr.MergeCommitSha = result.Sha
	pr.BaseSha = result.Sha
	return jsonResp(200, map[string]interface{}{
		"sha": result.Sha, "merged": true,
		"message": "Pull request #" + strconv.Itoa(pr.Number) + " successfully merged",
	})
}
