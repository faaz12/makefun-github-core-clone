// store.go — in-memory data model: repositories, issues, PRs, labels,
// comments, and the per-repo git object store + refs. Port of store.ts.
package main

import (
	"encoding/base64"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

var trialNowSec int64 = 1700000000
var trialNowISO string

func initTrialNow() {
	raw := os.Getenv("MAKEFUN_TRIAL_NOW")
	if raw != "" {
		if n, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil && n > 0 {
			if n > 1e12 {
				trialNowSec = int64(n / 1000)
			} else {
				trialNowSec = int64(n)
			}
		} else if t, err := time.Parse(time.RFC3339, strings.TrimSpace(raw)); err == nil {
			trialNowSec = t.Unix()
		}
	}
	trialNowISO = time.Unix(trialNowSec, 0).UTC().Format("2006-01-02T15:04:05.000Z")
}

type Issue struct {
	ID         int
	Number     int
	Title      string
	Body       string
	State      string // "open" | "closed"
	Labels     []string
	CreatedSeq int
	Comments   []*Comment
}

type Comment struct {
	ID         int
	Body       string
	CreatedSeq int
}

type PullRequest struct {
	ID            int
	Number        int
	Title         string
	Body          string
	State         string // "open" | "closed"
	Merged        bool
	MergedAt      string // "" = null
	MergeCommitSha string // "" = null
	HeadRef       string
	HeadSha       string
	BaseRef       string
	BaseSha       string
	CreatedSeq    int
}

type Label struct {
	ID    int
	Name  string
	Color string
}

type Repo struct {
	ID            int
	Name          string
	Owner         string
	Description   string
	IsPrivate     bool
	DefaultBranch string
	CreatedSeq    int
	Refs          map[string]string // 'refs/heads/main' -> commit oid
	Objects       map[string]*GitObject
	Issues        map[int]*Issue
	PRs           map[int]*PullRequest
	Labels        map[string]*Label
	NextNumber    int
	Seq           int
}

func NewRepo(id int, name, owner, description string, isPrivate bool, createdSeq int) *Repo {
	return &Repo{
		ID: id, Name: name, Owner: owner, Description: description,
		IsPrivate: isPrivate, DefaultBranch: "main", CreatedSeq: createdSeq,
		Refs: make(map[string]string), Objects: make(map[string]*GitObject),
		Issues: make(map[int]*Issue), PRs: make(map[int]*PullRequest),
		Labels: make(map[string]*Label), NextNumber: 1,
	}
}

func (r *Repo) GetObject(oid string) *GitObject { return r.Objects[oid] }

func (r *Repo) PutObject(obj *GitObject) string {
	oid := hashObject(obj.Type, obj.Content)
	if _, ok := r.Objects[oid]; !ok {
		r.Objects[oid] = obj
	}
	return oid
}

func (r *Repo) GetRef(name string) string { return r.Refs[name] }
func (r *Repo) SetRef(name, oid string)   { r.Refs[name] = oid }
func (r *Repo) DeleteRef(name string)     { delete(r.Refs, name) }

func (r *Repo) DefaultTip() string { return r.Refs["refs/heads/"+r.DefaultBranch] }
func (r *Repo) BranchTip(branch string) string {
	return r.Refs["refs/heads/"+branch]
}

type BranchInfo struct {
	Name string
	Sha  string
}

func (r *Repo) Branches() []BranchInfo {
	var out []BranchInfo
	for ref, sha := range r.Refs {
		if strings.HasPrefix(ref, "refs/heads/") {
			out = append(out, BranchInfo{Name: ref[len("refs/heads/"):], Sha: sha})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func buildCommitContent(tree string, parents []string, message string) []byte {
	var sb strings.Builder
	sb.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		sb.WriteString("parent " + p + "\n")
	}
	sb.WriteString("author " + ViewerName + " <" + ViewerEmail + "> " + strconv.FormatInt(trialNowSec, 10) + " +0000\n")
	sb.WriteString("committer " + ViewerName + " <" + ViewerEmail + "> " + strconv.FormatInt(trialNowSec, 10) + " +0000\n")
	sb.WriteString("\n")
	if strings.HasSuffix(message, "\n") {
		sb.WriteString(message)
	} else {
		sb.WriteString(message + "\n")
	}
	return []byte(sb.String())
}

type CommitFileResult struct {
	CommitOID string
	BlobOID   string // "" = null
	Existed   bool
}

func (r *Repo) CommitFile(branch, path string, content []byte, hasContent bool, message string) CommitFileResult {
	branchRef := "refs/heads/" + branch
	tip := r.Refs[branchRef]
	baseFiles := make(map[string]FlatFile)
	if tip == "" {
		if defTip := r.DefaultTip(); defTip != "" && branch != r.DefaultBranch {
			tip = defTip
		}
	}
	existed := false
	if tip != "" {
		commit := parseCommit(r.Objects[tip].Content)
		baseFiles = flattenTree(r, commit.Tree)
		_, existed = baseFiles[path]
	}
	files := make(map[string]FlatFile, len(baseFiles)+1)
	for k, v := range baseFiles {
		files[k] = v
	}
	var blobOID string
	if !hasContent {
		delete(files, path)
	} else {
		blobOID = r.PutObject(&GitObject{Type: ObjBlob, Content: content})
		files[path] = FlatFile{OID: blobOID, Mode: "100644"}
	}
	treeOID := buildTreeFromFiles(r, files)
	var parents []string
	if tip != "" {
		parents = []string{tip}
	}
	commitContent := buildCommitContent(treeOID, parents, message)
	commitOID := hashObject(ObjCommit, commitContent)
	r.Objects[commitOID] = &GitObject{Type: ObjCommit, Content: commitContent}
	r.Refs[branchRef] = commitOID
	return CommitFileResult{CommitOID: commitOID, BlobOID: blobOID, Existed: existed}
}

type CommitEntry struct {
	Sha    string
	Commit CommitParts
}

func (r *Repo) ListCommits(tip string) []CommitEntry {
	var out []CommitEntry
	seen := make(map[string]bool)
	stack := []string{tip}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := r.Objects[oid]
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		parts := parseCommit(obj.Content)
		out = append(out, CommitEntry{Sha: oid, Commit: parts})
		stack = append(stack, parts.Parents...)
	}
	return out
}

type MergeResult struct {
	Sha      string
	Conflict bool
}

func (r *Repo) SquashMerge(pr *PullRequest) MergeResult {
	baseTip := r.BranchTip(pr.BaseRef)
	headTip := r.BranchTip(pr.HeadRef)
	if baseTip == "" || headTip == "" {
		return MergeResult{Conflict: true}
	}
	base := mergeBase(r, baseTip, headTip)
	var baseTree map[string]FlatFile
	if base != "" {
		baseTree = flattenTree(r, parseCommit(r.Objects[base].Content).Tree)
	} else {
		baseTree = make(map[string]FlatFile)
	}
	baseSide := flattenTree(r, parseCommit(r.Objects[baseTip].Content).Tree)
	headSide := flattenTree(r, parseCommit(r.Objects[headTip].Content).Tree)
	allPaths := make(map[string]bool)
	for p := range baseTree {
		allPaths[p] = true
	}
	for p := range baseSide {
		allPaths[p] = true
	}
	for p := range headSide {
		allPaths[p] = true
	}
	for path := range allPaths {
		mb := baseTree[path].OID
		bs := baseSide[path].OID
		hs := headSide[path].OID
		// NOTE: map zero value for a missing path is "" — but a real oid is
		// never "", so comparing oids directly is equivalent to the TS
		// undefined checks here (a path absent on a side has no oid).
		if bs != mb && hs != mb && bs != hs {
			return MergeResult{Conflict: true}
		}
	}
	result := make(map[string]FlatFile, len(baseSide))
	for k, v := range baseSide {
		result[k] = v
	}
	for path, file := range headSide {
		result[path] = file
	}
	for path := range baseTree {
		if _, onHead := headSide[path]; !onHead {
			if _, onBase := baseSide[path]; onBase {
				if baseSide[path].OID == baseTree[path].OID {
					delete(result, path)
				}
			}
		}
	}
	treeOID := buildTreeFromFiles(r, result)
	message := "Merge pull request #" + strconv.Itoa(pr.Number) + " from " + pr.HeadRef
	commitContent := buildCommitContent(treeOID, []string{baseTip}, message)
	commitOID := hashObject(ObjCommit, commitContent)
	r.Objects[commitOID] = &GitObject{Type: ObjCommit, Content: commitContent}
	r.Refs["refs/heads/"+pr.BaseRef] = commitOID
	return MergeResult{Sha: commitOID}
}

func (r *Repo) MergeConflict(pr *PullRequest) bool {
	baseTip := r.BranchTip(pr.BaseRef)
	headTip := r.BranchTip(pr.HeadRef)
	if baseTip == "" || headTip == "" {
		return true
	}
	base := mergeBase(r, baseTip, headTip)
	var baseTree map[string]FlatFile
	if base != "" {
		baseTree = flattenTree(r, parseCommit(r.Objects[base].Content).Tree)
	} else {
		baseTree = make(map[string]FlatFile)
	}
	baseSide := flattenTree(r, parseCommit(r.Objects[baseTip].Content).Tree)
	headSide := flattenTree(r, parseCommit(r.Objects[headTip].Content).Tree)
	allPaths := make(map[string]bool)
	for p := range baseTree {
		allPaths[p] = true
	}
	for p := range baseSide {
		allPaths[p] = true
	}
	for p := range headSide {
		allPaths[p] = true
	}
	for path := range allPaths {
		mb := baseTree[path].OID
		bs := baseSide[path].OID
		hs := headSide[path].OID
		if bs != mb && hs != mb && bs != hs {
			return true
		}
	}
	return false
}

type DiffFile struct {
	Filename  string
	Status    string // "added" | "removed" | "modified"
	Additions int
	Deletions int
	Changes   int
}

func (r *Repo) DiffFiles(baseSha, headSha string) []DiffFile {
	baseTree := flattenTree(r, parseCommit(r.Objects[baseSha].Content).Tree)
	headTree := flattenTree(r, parseCommit(r.Objects[headSha].Content).Tree)
	paths := make(map[string]bool)
	for p := range baseTree {
		paths[p] = true
	}
	for p := range headTree {
		paths[p] = true
	}
	var out []DiffFile
	for path := range paths {
		b, hasB := baseTree[path]
		h, hasH := headTree[path]
		switch {
		case !hasB && hasH:
			lines := 0
			if o := r.Objects[h.OID]; o != nil {
				lines = countLines(o.Content)
			}
			out = append(out, DiffFile{Filename: path, Status: "added", Additions: lines, Changes: lines})
		case hasB && !hasH:
			lines := 0
			if o := r.Objects[b.OID]; o != nil {
				lines = countLines(o.Content)
			}
			out = append(out, DiffFile{Filename: path, Status: "removed", Deletions: lines, Changes: lines})
		case hasB && hasH && b.OID != h.OID:
			oldLines := r.objLines(b.OID)
			newLines := r.objLines(h.OID)
			add, del := lineDiff(oldLines, newLines)
			out = append(out, DiffFile{Filename: path, Status: "modified", Additions: add, Deletions: del, Changes: add + del})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Filename < out[j].Filename })
	return out
}

// objLines splits an object's content like TS's .split('\n'); a missing
// object yields no lines at all (TS's `?? []`).
func (r *Repo) objLines(oid string) []string {
	o := r.Objects[oid]
	if o == nil {
		return []string{}
	}
	return strings.Split(string(o.Content), "\n")
}
func countLines(content []byte) int {
	if len(content) == 0 {
		return 1
	}
	n := 1
	for _, b := range content {
		if b == '\n' {
			n++
		}
	}
	return n
}

// lineDiff: LCS-based line diff (files are small in this contest).
func lineDiff(oldLines, newLines []string) (additions, deletions int) {
	m, n := len(oldLines), len(newLines)
	if m == 0 {
		return n, 0
	}
	if n == 0 {
		return 0, m
	}
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := m - 1; i >= 0; i-- {
		for j := n - 1; j >= 0; j-- {
			if oldLines[i] == newLines[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else if dp[i+1][j] > dp[i][j+1] {
				dp[i][j] = dp[i+1][j]
			} else {
				dp[i][j] = dp[i][j+1]
			}
		}
	}
	i, j := 0, 0
	for i < m && j < n {
		if oldLines[i] == newLines[j] {
			i++
			j++
		} else if dp[i+1][j] >= dp[i][j+1] {
			deletions++
			i++
		} else {
			additions++
			j++
		}
	}
	deletions += m - i
	additions += n - j
	return additions, deletions
}

type Store struct {
	Repos       map[string]*Repo
	NextRepoID  int
	NextObjectID int
	NextLabelID int
	Seq         int
}

func NewStore() *Store {
	return &Store{Repos: make(map[string]*Repo), NextRepoID: 1, NextObjectID: 1, NextLabelID: 1}
}

func (s *Store) GetRepo(owner, name string) *Repo {
	return s.Repos[owner+"/"+name]
}

func (s *Store) CreateRepo(name, owner, description string, isPrivate bool) *Repo {
	s.Seq++
	repo := NewRepo(s.NextRepoID, name, owner, description, isPrivate, s.Seq)
	s.NextRepoID++
	s.Repos[owner+"/"+name] = repo
	return repo
}

func (s *Store) DeleteRepo(owner, name string) bool {
	if _, ok := s.Repos[owner+"/"+name]; !ok {
		return false
	}
	delete(s.Repos, owner+"/"+name)
	return true
}

func (s *Store) ListRepos(owner string) []*Repo {
	var out []*Repo
	for _, repo := range s.Repos {
		if repo.Owner == owner {
			out = append(out, repo)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedSeq > out[j].CreatedSeq })
	return out
}

func (s *Store) NextID() int {
	id := s.NextObjectID
	s.NextObjectID++
	return id
}

// globalId: opaque, stable GraphQL global ids.
func (s *Store) GlobalID(kind string, parts ...interface{}) string {
	var sb strings.Builder
	sb.WriteString(kind)
	for _, p := range parts {
		sb.WriteString(":")
		switch v := p.(type) {
		case int:
			sb.WriteString(strconv.Itoa(v))
		case string:
			sb.WriteString(v)
		}
	}
	return base64.RawURLEncoding.EncodeToString([]byte(sb.String()))
}

type ParsedGlobalID struct {
	Kind  string
	Parts []string
}

func (s *Store) ParseGlobalID(id string) *ParsedGlobalID {
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return nil
	}
	text := string(b)
	parts := strings.Split(text, ":")
	if len(parts) == 0 || parts[0] == "" || len(parts) < 2 {
		return nil
	}
	return &ParsedGlobalID{Kind: parts[0], Parts: parts[1:]}
}

type ResolvedNode struct {
	Repo  *Repo
	Issue *Issue
	PR    *PullRequest
}

func (s *Store) ResolveNode(id string) *ResolvedNode {
	parsed := s.ParseGlobalID(id)
	if parsed == nil {
		return nil
	}
	switch parsed.Kind {
	case "Repository":
		dbID, err := strconv.Atoi(parsed.Parts[0])
		if err != nil {
			return nil
		}
		for _, repo := range s.Repos {
			if repo.ID == dbID {
				return &ResolvedNode{Repo: repo}
			}
		}
		return nil
	case "Issue", "PullRequest":
		dbID, err1 := strconv.Atoi(parsed.Parts[0])
		num, err2 := strconv.Atoi(parsed.Parts[1])
		if err1 != nil || err2 != nil {
			return nil
		}
		for _, repo := range s.Repos {
			if repo.ID != dbID {
				continue
			}
			if parsed.Kind == "Issue" {
				if issue, ok := repo.Issues[num]; ok {
					return &ResolvedNode{Repo: repo, Issue: issue}
				}
			} else {
				if pr, ok := repo.PRs[num]; ok {
					return &ResolvedNode{Repo: repo, PR: pr}
				}
			}
		}
		return nil
	}
	return nil
}
