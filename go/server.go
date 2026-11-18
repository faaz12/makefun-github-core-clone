// server.go — raw TCP HTTP/1.1 server, routing, auth. No net/http: the
// request line + headers are parsed straight off the socket, responses are
// written as one buffer, and keep-alive connections are served in a loop.
// Port of app.ts (same routes, same auth, same bodies).
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	store   *Store
	storeMu sync.RWMutex
	reqID   atomic.Uint64
)

var reasonPhrases = map[int]string{
	200: "OK",
	201: "Created",
	202: "Accepted",
	204: "No Content",
	400: "Bad Request",
	401: "Unauthorized",
	404: "Not Found",
	405: "Method Not Allowed",
	409: "Conflict",
	422: "Unprocessable Entity",
	500: "Internal Server Error",
}

type route struct {
	method  string
	pattern []string
	handler func(*Ctx) Resp
}

var routes = []route{
	{"GET", []string{"api", "v3"}, apiRoot},
	{"GET", []string{"api", "v3", "meta"}, metaHandler},
	{"GET", []string{"api", "v3", "user"}, getUser},
	{"POST", []string{"api", "v3", "user", "repos"}, createUserRepo},
	{"GET", []string{"api", "v3", "user", "repos"}, listUserRepos},
	{"GET", []string{"api", "v3", "repos", ":owner", ":repo"}, getRepo},
	{"PATCH", []string{"api", "v3", "repos", ":owner", ":repo"}, patchRepo},
	{"DELETE", []string{"api", "v3", "repos", ":owner", ":repo"}, deleteRepo},
	{"POST", []string{"api", "v3", "repos", ":owner", ":repo", "git", "refs"}, createRef},
	{"GET", []string{"api", "v3", "repos", ":owner", ":repo", "git", "refs", "*"}, getRef},
	{"GET", []string{"api", "v3", "repos", ":owner", ":repo", "branches"}, listBranches},
	{"GET", []string{"api", "v3", "repos", ":owner", ":repo", "branches", ":branch"}, getBranch},
	{"GET", []string{"api", "v3", "repos", ":owner", ":repo", "commits"}, listCommits},
	{"GET", []string{"api", "v3", "repos", ":owner", ":repo", "contents", "*"}, getContents},
	{"PUT", []string{"api", "v3", "repos", ":owner", ":repo", "contents", "*"}, putContents},
	{"DELETE", []string{"api", "v3", "repos", ":owner", ":repo", "contents", "*"}, deleteContents},
	{"POST", []string{"api", "v3", "repos", ":owner", ":repo", "labels"}, createLabel},
	{"GET", []string{"api", "v3", "repos", ":owner", ":repo", "labels"}, listLabels},
	{"POST", []string{"api", "v3", "repos", ":owner", ":repo", "issues"}, createIssue},
	{"GET", []string{"api", "v3", "repos", ":owner", ":repo", "issues"}, listIssues},
	{"GET", []string{"api", "v3", "repos", ":owner", ":repo", "issues", ":number"}, getIssue},
	{"PATCH", []string{"api", "v3", "repos", ":owner", ":repo", "issues", ":number"}, patchIssue},
	{"POST", []string{"api", "v3", "repos", ":owner", ":repo", "issues", ":number", "comments"}, createIssueComment},
	{"GET", []string{"api", "v3", "repos", ":owner", ":repo", "issues", ":number", "comments"}, listIssueComments},
	{"POST", []string{"api", "v3", "repos", ":owner", ":repo", "pulls"}, createPullRequest},
	{"GET", []string{"api", "v3", "repos", ":owner", ":repo", "pulls"}, listPullRequests},
	{"GET", []string{"api", "v3", "repos", ":owner", ":repo", "pulls", ":number"}, getPullRequest},
	{"GET", []string{"api", "v3", "repos", ":owner", ":repo", "pulls", ":number", "files"}, listPullRequestFiles},
	{"PUT", []string{"api", "v3", "repos", ":owner", ":repo", "pulls", ":number", "merge"}, mergePullRequest},
}

func matchPattern(pattern, segments []string) (map[string]string, bool) {
	params := make(map[string]string)
	for i, p := range pattern {
		if p == "*" {
			params["path"] = strings.Join(segments[i:], "/")
			return params, true
		}
		if i >= len(segments) {
			return nil, false
		}
		seg := segments[i]
		if strings.HasPrefix(p, ":") {
			params[p[1:]] = seg
		} else if p != seg {
			return nil, false
		}
	}
	if len(segments) != len(pattern) {
		return nil, false
	}
	return params, true
}

// ---------------------------------------------------------------------------
// connection handling
// ---------------------------------------------------------------------------

type conn struct {
	c  net.Conn
	br *bufio.Reader
}

func (cn *conn) header(name string, headers [][2]string) string {
	for _, h := range headers {
		if strings.EqualFold(h[0], name) {
			return h[1]
		}
	}
	return ""
}

// readRequest reads one request: method, target, version, headers, body.
// Parse failures return errParse (the caller answers 400) or errHeaderTooLarge
// (the caller answers 431); a clean EOF returns io.EOF so the caller just
// closes. This mirrors Node's llhttp behavior for malformed traffic.
var errParse = fmt.Errorf("bad request")
var errHeaderTooLarge = fmt.Errorf("headers too large")

var validMethods = map[string]bool{
	"GET": true, "HEAD": true, "POST": true, "PUT": true, "DELETE": true,
	"CONNECT": true, "OPTIONS": true, "TRACE": true, "PATCH": true,
}

func (cn *conn) readLineCRLF() (string, error) {
	line, err := cn.br.ReadString('\n')
	if err != nil {
		return "", err
	}
	if !strings.HasSuffix(line, "\r\n") {
		return "", errParse
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (cn *conn) readRequest() (method, target, version string, headers [][2]string, body []byte, err error) {
	// Tolerate leading empty lines (RFC 9112 s2.2: at least one), counting
	// them toward the header budget.
	headerBytes := 0
	line := ""
	for {
		line, err = cn.readLineCRLF()
		if err != nil {
			return "", "", "", nil, nil, err
		}
		headerBytes += len(line) + 2
		if headerBytes > 16384 {
			return "", "", "", nil, nil, errHeaderTooLarge
		}
		if line != "" {
			break
		}
	}
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return "", "", "", nil, nil, errParse
	}
	method, target, version = parts[0], parts[1], parts[2]
	if !validMethods[method] {
		return "", "", "", nil, nil, errParse
	}
	if version != "HTTP/1.0" && version != "HTTP/1.1" {
		return "", "", "", nil, nil, errParse
	}
	clCount := 0
	for {
		hline, err := cn.readLineCRLF()
		if err != nil {
			return "", "", "", nil, nil, err
		}
		headerBytes += len(hline) + 2
		if headerBytes > 16384 {
			return "", "", "", nil, nil, errHeaderTooLarge
		}
		if hline == "" {
			break
		}
		i := strings.IndexByte(hline, ':')
		if i < 0 {
			return "", "", "", nil, nil, errParse
		}
		name := strings.TrimSpace(hline[:i])
		value := strings.TrimSpace(hline[i+1:])
		headers = append(headers, [2]string{name, value})
		if strings.EqualFold(name, "Content-Length") {
			clCount++
		}
	}
	if method == "GET" || method == "HEAD" {
		return method, target, version, headers, nil, nil
	}
	// Body: Content-Length or chunked. Duplicate Content-Length, or
	// Content-Length together with Transfer-Encoding, is a 400 (smuggling).
	te := cn.header("Transfer-Encoding", headers)
	cl := cn.header("Content-Length", headers)
	if clCount > 1 || (cl != "" && te != "") {
		return "", "", "", nil, nil, errParse
	}
	// Expect: 100-continue — answer it before reading the body, like Node.
	if strings.EqualFold(strings.TrimSpace(cn.header("Expect", headers)), "100-continue") {
		if _, werr := cn.c.Write([]byte("HTTP/1.1 100 Continue\r\n\r\n")); werr != nil {
			return "", "", "", nil, nil, werr
		}
	}
	if strings.Contains(strings.ToLower(te), "chunked") {
		body, err = cn.readChunked()
		if err != nil {
			return "", "", "", nil, nil, errParse
		}
		return method, target, version, headers, body, nil
	}
	if cl == "" {
		return method, target, version, headers, nil, nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(cl))
	if err != nil || n < 0 {
		return "", "", "", nil, nil, errParse
	}
	if n == 0 {
		return method, target, version, headers, []byte{}, nil
	}
	body = make([]byte, n)
	if _, err := readFull(cn.br, body); err != nil {
		return "", "", "", nil, nil, errParse
	}
	return method, target, version, headers, body, nil
}

func readFull(br *bufio.Reader, buf []byte) (int, error) {
	n := 0
	for n < len(buf) {
		m, err := br.Read(buf[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

func (cn *conn) readChunked() ([]byte, error) {
	var out []byte
	for {
		line, err := cn.readLineCRLF()
		if err != nil {
			return nil, err
		}
		if i := strings.IndexByte(line, ';'); i >= 0 {
			line = line[:i]
		}
		size, err := strconv.ParseInt(strings.TrimSpace(line), 16, 64)
		if err != nil {
			return nil, err
		}
		if size == 0 {
			// trailers until an empty line
			for {
				tline, err := cn.readLineCRLF()
				if err != nil {
					return nil, err
				}
				if tline == "" {
					break
				}
			}
			return out, nil
		}
		chunk := make([]byte, size)
		if _, err := readFull(cn.br, chunk); err != nil {
			return nil, err
		}
		out = append(out, chunk...)
		// trailing CRLF
		if _, err := cn.readLineCRLF(); err != nil {
			return nil, err
		}
	}
}

func writeResponse(c net.Conn, status int, headers [][2]string, body []byte, headOnly bool) error {
	var buf bytes.Buffer
	reason, ok := reasonPhrases[status]
	if !ok {
		reason = ""
	}
	buf.WriteString("HTTP/1.1 " + strconv.Itoa(status) + " " + reason + "\r\n")
	// Node sends a Date header on every response (IMF-fixdate); match it.
	// It goes first, as Node orders it, and costs one Format per response.
	buf.WriteString("Date: " + time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT") + "\r\n")
	for _, h := range headers {
		buf.WriteString(h[0] + ": " + h[1] + "\r\n")
	}
	// RFC 9110 s8.6: no Content-Length on a 204 (or 1xx) response.
	if status != 204 {
		buf.WriteString("Content-Length: " + strconv.Itoa(len(body)) + "\r\n")
	}
	buf.WriteString("\r\n")
	if !headOnly {
		buf.Write(body)
	}
	_, err := c.Write(buf.Bytes())
	return err
}

func serveConn(c net.Conn) {
	defer c.Close()
	cn := &conn{c: c, br: bufio.NewReaderSize(c, 65536)}
	for {
		// Bound how long one request may take to arrive (slow-client guard);
		// legitimate trial traffic arrives in milliseconds.
		_ = c.SetReadDeadline(time.Now().Add(60 * time.Second))
		method, target, version, headers, body, err := cn.readRequest()
		if err != nil {
			date := time.Now().UTC().Format("Mon, 02 Jan 2006 15:04:05 GMT")
			if err == errHeaderTooLarge {
				_, _ = c.Write([]byte("HTTP/1.1 431 Request Header Fields Too Large\r\nDate: " + date + "\r\n\r\n"))
			} else if err != io.EOF {
				_, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\nDate: " + date + "\r\n\r\n"))
			}
			return
		}
		_ = c.SetReadDeadline(time.Time{})
		keepAlive := true
		connHdr := strings.ToLower(cn.header("Connection", headers))
		if version == "HTTP/1.0" {
			// Node answers HTTP/1.0 with Connection: close and closes, even
			// when the request asks keep-alive (observed on Node 24).
			keepAlive = false
		} else if connHdr == "close" {
			keepAlive = false
		}
		resp, closeAfter := handleRequest(method, target, headers, body)
		// response headers
		rh := [][2]string{}
		if resp.ContentType != "" {
			rh = append(rh, [2]string{"Content-Type", resp.ContentType})
		}
		for k, v := range resp.Headers {
			rh = append(rh, [2]string{k, v})
		}
		if !keepAlive || closeAfter {
			rh = append(rh, [2]string{"Connection", "close"})
		}
		if err := writeResponse(c, resp.Status, rh, resp.Body, method == "HEAD"); err != nil {
			return
		}
		if !keepAlive || closeAfter {
			return
		}
	}
}

// handleRequest routes one request. Returns the response and whether the
// connection must close after it.
func handleRequest(method, target string, headers [][2]string, body []byte) (resp Resp, closeAfter bool) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "handler panic: %v\n", r)
			resp = Resp{Status: 500, Body: []byte(`{"message":"Internal Server Error"}`), ContentType: "application/json"}
			closeAfter = false
		}
	}()
	id := reqID.Add(1)
	reqHeader := "req-" + strconv.FormatUint(id, 10)

	host := ""
	for _, h := range headers {
		if strings.EqualFold(h[0], "Host") {
			host = h[1]
			break
		}
	}
	if host == "" {
		host = "localhost"
	}
	baseURL := "http://" + host

	// Split target into path + query.
	rawPath, rawQuery := target, ""
	if i := strings.IndexByte(target, '?'); i >= 0 {
		rawPath, rawQuery = target[:i], target[i+1:]
	}
	// WHATWG URL: a target starting with "//" carries an authority (the env's
	// Node server parses it that way), so "//api/..." does not route to /api.
	if strings.HasPrefix(rawPath, "//") {
		if i := strings.Index(rawPath[2:], "/"); i >= 0 {
			rawPath = rawPath[2+i:]
		} else {
			rawPath = "/"
		}
	}
	rawSegs := strings.Split(rawPath, "/")
	segments := make([]string, 0, len(rawSegs))
	for _, s := range rawSegs {
		if s == "" {
			continue
		}
		dec, err := url.PathUnescape(s)
		if err != nil {
			return Resp{Status: 500, Body: []byte(`{"message":"Internal Server Error"}`), ContentType: "application/json"}, false
		}
		segments = append(segments, dec)
	}
	u, _ := url.Parse("http://" + host + target)
	_ = rawQuery

	withID := func(r Resp) Resp {
		if r.Headers == nil {
			r.Headers = map[string]string{}
		}
		r.Headers["X-GitHub-Request-Id"] = reqHeader
		return r
	}
	unauthorized := func() Resp {
		return withID(Resp{Status: 401, Body: []byte(`{"message":"Bad credentials"}`), ContentType: "application/json"})
	}

	// Health probe: no auth.
	if method == "GET" && len(segments) == 1 && segments[0] == "healthz" {
		return withID(Resp{Status: 200, Body: []byte(`{"ok":true}`), ContentType: "application/json"}), false
	}

	// Auth: Bearer token must match MAKEFUN_SHOP_TOKEN (when set).
	auth := ""
	for _, h := range headers {
		if strings.EqualFold(h[0], "Authorization") {
			auth = h[1]
			break
		}
	}
	expected := os.Getenv("MAKEFUN_SHOP_TOKEN")
	presented := ""
	if strings.HasPrefix(auth, "Bearer ") {
		presented = auth[7:]
	}
	if presented == "" || (expected != "" && presented != expected) {
		return unauthorized(), false
	}

	// runLocked invokes fn with the store lock held; the defer guarantees
	// the lock is released even if fn panics (the recover in handleRequest
	// turns the panic into a 500).
	runLocked := func(write bool, fn func() Resp) Resp {
		if write {
			storeMu.Lock()
			defer storeMu.Unlock()
		} else {
			storeMu.RLock()
			defer storeMu.RUnlock()
		}
		return fn()
	}

	// GraphQL.
	if method == "POST" && len(segments) == 2 && segments[0] == "api" && segments[1] == "graphql" {
		var parsed interface{}
		if len(body) > 0 {
			if err := json.Unmarshal(body, &parsed); err != nil {
				return withID(Resp{Status: 400, Body: []byte(`{"message":"Problems parsing JSON"}`), ContentType: "application/json"}), false
			}
		}
		storeMu.Lock()
		defer storeMu.Unlock()
		if arr, ok := parsed.([]interface{}); ok {
			results := make([]gqlResult, 0, len(arr))
			for _, doc := range arr {
				query := ""
				variables := map[string]interface{}{}
				if rec, ok := doc.(map[string]interface{}); ok {
					query, _ = rec["query"].(string)
					if v, ok := rec["variables"].(map[string]interface{}); ok {
						variables = v
					}
				}
				results = append(results, executeGraphQL(store, query, variables))
			}
			return withID(jsonResp(200, results)), false
		}
		query := ""
		variables := map[string]interface{}{}
		if rec, ok := parsed.(map[string]interface{}); ok {
			query, _ = rec["query"].(string)
			if v, ok := rec["variables"].(map[string]interface{}); ok {
				variables = v
			}
		}
		return withID(jsonResp(200, executeGraphQL(store, query, variables))), false
	}

	// MCP.
	if (method == "POST" || method == "DELETE") && len(segments) == 1 && segments[0] == "mcp" {
		if method == "DELETE" {
			return withID(Resp{Status: 200, Body: []byte(`{}`), ContentType: "application/json"}), false
		}
		var parsed interface{}
		if len(body) > 0 {
			if err := json.Unmarshal(body, &parsed); err != nil {
				return withID(Resp{Status: 400, Body: []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32700,"message":"Parse error"}}`), ContentType: "application/json", Headers: map[string]string{"Mcp-Protocol-Version": "2026-07-28"}}), false
			}
		}
		storeMu.Lock()
		defer storeMu.Unlock()
		ctx := &Ctx{Store: store, Method: method, URL: u, Params: map[string]string{}, Body: parsed, BaseURL: baseURL}
		out := handleMcp(ctx)
		if out.IsNotification {
			return withID(Resp{Status: out.Status, Body: nil, ContentType: "", Headers: map[string]string{"Mcp-Protocol-Version": "2026-07-28"}}), false
		}
		return withID(Resp{Status: out.Status, Body: out.Body, ContentType: "application/json", Headers: map[string]string{"Mcp-Protocol-Version": "2026-07-28"}}), false
	}

	// Git smart HTTP.
	// NOTE: the .git check runs on the RAW segment (before decoding), like TS.
	rawParts := strings.Split(rawPath, "/")
	rawSegs2 := make([]string, 0, len(rawParts))
	for _, s := range rawParts {
		if s != "" {
			rawSegs2 = append(rawSegs2, s)
		}
	}
	isGit := len(rawSegs2) >= 3 &&
		strings.HasSuffix(rawSegs2[1], ".git") &&
		((len(rawSegs2) == 4 && rawSegs2[2] == "info" && rawSegs2[3] == "refs") ||
			(len(rawSegs2) == 3 && (rawSegs2[2] == "git-upload-pack" || rawSegs2[2] == "git-receive-pack")))
	if isGit {
		owner, _ := url.PathUnescape(rawSegs2[0])
		repoName, _ := url.PathUnescape(rawSegs2[1][:len(rawSegs2[1])-4])
		if rawSegs2[2] == "info" {
			service := u.Query().Get("service")
			if service != "git-upload-pack" && service != "git-receive-pack" {
				return withID(Resp{Status: 400, Body: []byte(`{"message":"service is required"}`), ContentType: "application/json"}), false
			}
			gitProtocol := ""
			for _, h := range headers {
				if strings.EqualFold(h[0], "Git-Protocol") {
					gitProtocol = h[1]
					break
				}
			}
			out := runLocked(false, func() Resp {
				return handleInfoRefs(store.GetRepo(owner, repoName), service, gitProtocol)
			})
			return withID(out), false
		}
		if rawSegs2[2] == "git-upload-pack" {
			out := runLocked(false, func() Resp {
				return handleUploadPack(store.GetRepo(owner, repoName), body)
			})
			return withID(out), false
		}
		out := runLocked(true, func() Resp {
			return handleReceivePack(store.GetRepo(owner, repoName), body)
		})
		return withID(out), false
	}

	// REST routes.
	var parsedBody interface{}
	if len(body) > 0 {
		if err := json.Unmarshal(body, &parsedBody); err != nil {
			return withID(Resp{Status: 400, Body: []byte(`{"message":"Problems parsing JSON"}`), ContentType: "application/json"}), false
		}
	}
	for _, rt := range routes {
		if rt.method != method {
			continue
		}
		params, ok := matchPattern(rt.pattern, segments)
		if !ok {
			continue
		}
		isWrite := method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE"
		ctx := &Ctx{Store: store, Method: method, URL: u, Params: params, Body: parsedBody, BaseURL: baseURL}
		out := runLocked(isWrite, func() Resp { return rt.handler(ctx) })
		if isAPIRoot(rt) {
			if out.Headers == nil {
				out.Headers = map[string]string{}
			}
			out.Headers["X-OAuth-Scopes"] = "repo, read:org"
		}
		return withID(out), false
	}

	return withID(Resp{Status: 404, Body: []byte(`{"message":"Not Found"}`), ContentType: "application/json"}), false
}

func isAPIRoot(rt route) bool {
	return rt.method == "GET" && len(rt.pattern) == 2 && rt.pattern[0] == "api" && rt.pattern[1] == "v3"
}

func serve(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go serveConn(c)
	}
}
