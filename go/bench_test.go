package main

// R2 profiling benchmarks: mirror the kit workloads in-process.
// Clone = depth-1 fetch on a 200-commit repo; push = decodePack of a
// 3-object pack; read = GET issue; write = POST issue.

import (
	"bytes"
	"github.com/klauspost/compress/zlib"
	"crypto/sha1"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"
)

func seedRepo200(tb testing.TB) *Repo {
	tb.Helper()
	s := NewStore()
	store = s
	repo := s.CreateRepo("bench", ViewerLogin, "", false)
	var parent string
	for i := 0; i < 200; i++ {
		blob := bytes.Repeat([]byte(fmt.Sprintf("line %d ", i)), 200)[:1024] // ~1 KiB
		blobOID := repo.PutObject(&GitObject{Type: ObjBlob, Content: blob})
		treeContent := append([]byte("100644 file.txt\x00"), oidBytes(blobOID)...)
		treeOID := repo.PutObject(&GitObject{Type: ObjTree, Content: treeContent})
		parents := []string{}
		if parent != "" {
			parents = []string{parent}
		}
		commitOID, commitContent := buildCommit(treeOID, parents, "commit "+strconv.Itoa(i), ViewerName, ViewerEmail, 1700000000+int64(i))
		repo.PutObject(&GitObject{Type: ObjCommit, Content: commitContent})
		parent = commitOID
	}
	repo.SetRef("refs/heads/main", parent)
	// One issue for the read/write benches.
	repo.Issues[1] = &Issue{Number: 1, Title: "bench issue", Body: "hello", State: "open"}
	repo.NextNumber = 2
	return repo
}

func shallowFetchArgs(repo *Repo) fetchArgs {
	deepen := 1
	return fetchArgs{wants: []string{repo.DefaultTip()}, haves: nil, deepen: &deepen}
}

func BenchmarkFetchResponseV2Shallow(b *testing.B) {
	repo := seedRepo200(b)
	args := shallowFetchArgs(repo)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out := fetchResponse(repo, args, true)
		if len(out) == 0 {
			b.Fatal("empty fetch response")
		}
	}
}

func BenchmarkEncodePackShallowL6(b *testing.B) {
	repo := seedRepo200(b)
	args := shallowFetchArgs(repo)
	res := reachableFrom(repo, args.wants, map[string]bool{}, args.deepen)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out := encodePack(res.Objects)
		if len(out) == 0 {
			b.Fatal("empty pack")
		}
	}
}

func BenchmarkEncodePackFullL6(b *testing.B) {
	repo := seedRepo200(b)
	res := reachableFrom(repo, []string{repo.DefaultTip()}, map[string]bool{}, nil)
	b.Logf("full clone objects: %d", len(res.Objects))
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out := encodePack(res.Objects)
		if len(out) == 0 {
			b.Fatal("empty pack")
		}
	}
}

func BenchmarkDecodePackPush(b *testing.B) {
	repo := seedRepo200(b)
	// A push pack: new commit + tree + 1 KiB blob on top of the tip.
	blob := bytes.Repeat([]byte("push change "), 100)[:1024]
	blobOID := hashObject(ObjBlob, blob)
	treeContent := append([]byte("100644 file.txt\x00"), oidBytes(blobOID)...)
	treeOID := hashObject(ObjTree, treeContent)
	commitOID, commitContent := buildCommit(treeOID, []string{repo.DefaultTip()}, "push", ViewerName, ViewerEmail, 1700000201)
	_ = commitOID
	pack := encodePack([]PackObject{
		{Type: ObjBlob, Content: blob},
		{Type: ObjTree, Content: treeContent},
		{Type: ObjCommit, Content: commitContent},
	})
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		decoded, err := decodePack(pack, func(oid string) *GitObject { return repo.GetObject(oid) })
		if err != nil || len(decoded) != 3 {
			b.Fatalf("decodePack: %v %d", err, len(decoded))
		}
	}
}

func BenchmarkHandleRequestGetIssue(b *testing.B) {
	seedRepo200(b)
	headers := [][2]string{{"Authorization", "Bearer test-token"}, {"Host", "127.0.0.1:1"}}
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		resp, _ := handleRequest("GET", "/api/v3/repos/adaiversen/bench/issues/1", headers, nil)
		if resp.Status != 200 {
			b.Fatalf("status %d", resp.Status)
		}
	}
}

func BenchmarkHandleRequestPostIssue(b *testing.B) {
	seedRepo200(b)
	headers := [][2]string{{"Authorization", "Bearer test-token"}, {"Host", "127.0.0.1:1"}, {"Content-Type", "application/json"}}
	body := []byte(`{"title":"t","body":"b"}`)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		resp, _ := handleRequest("POST", "/api/v3/repos/adaiversen/bench/issues", headers, body)
		if resp.Status != 201 {
			b.Fatalf("status %d", resp.Status)
		}
	}
}

func BenchmarkFetchResponseV2ShallowParallel(b *testing.B) {
	repo := seedRepo200(b)
	args := shallowFetchArgs(repo)
	// Per-goroutine latency slices; merged at the end for percentiles.
	var mu sync.Mutex
	var all []time.Duration
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		var local []time.Duration
		for pb.Next() {
			start := time.Now()
			out := fetchResponse(repo, args, true)
			if len(out) == 0 {
				b.Fatal("empty fetch response")
			}
			local = append(local, time.Since(start))
		}
		mu.Lock()
		all = append(all, local...)
		mu.Unlock()
	})
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	n := len(all)
	if n > 0 {
		pct := func(p float64) time.Duration { return all[int(float64(n)*p)] }
		b.Logf("parallel fetch n=%d p50=%s p95=%s p99=%s max=%s", n, pct(0.50), pct(0.95), pct(0.99), all[n-1])
	}
}

func BenchmarkHandleUploadPackShallowParallel(b *testing.B) {
	repo := seedRepo200(b)
	tip := repo.DefaultTip()
	var body bytes.Buffer
	writePkt := func(s string) {
		body.WriteString(fmt.Sprintf("%04x%s", len(s)+4, s))
	}
	writePkt("command=fetch\n")
	body.WriteString("0001")
	writePkt("want " + tip + "\n")
	writePkt("deepen 1\n")
	body.WriteString("0000")
	req := body.Bytes()
	var mu sync.Mutex
	var all []time.Duration
	b.ResetTimer()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		var local []time.Duration
		for pb.Next() {
			start := time.Now()
			resp := handleUploadPack(repo, req)
			if resp.Status != 200 || len(resp.Body) == 0 {
				b.Fatalf("upload-pack status %d len %d", resp.Status, len(resp.Body))
			}
			local = append(local, time.Since(start))
		}
		mu.Lock()
		all = append(all, local...)
		mu.Unlock()
	})
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	n := len(all)
	if n > 0 {
		pct := func(p float64) time.Duration { return all[int(float64(n)*p)] }
		b.Logf("parallel upload-pack n=%d p50=%s p95=%s p99=%s max=%s", n, pct(0.50), pct(0.95), pct(0.99), all[n-1])
	}
}

func BenchmarkHandleUploadPackShallow(b *testing.B) {
	repo := seedRepo200(b)
	tip := repo.DefaultTip()
	// Minimal v2 fetch request body: command=fetch + want + deepen 1.
	var body bytes.Buffer
	writePkt := func(s string) {
		body.WriteString(fmt.Sprintf("%04x%s", len(s)+4, s))
	}
	writePkt("command=fetch\n")
	body.WriteString("0001") // delim
	writePkt("want " + tip + "\n")
	writePkt("deepen 1\n")
	body.WriteString("0000") // flush
	req := body.Bytes()
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		resp := handleUploadPack(repo, req)
		if resp.Status != 200 || len(resp.Body) == 0 {
			b.Fatalf("upload-pack status %d len %d", resp.Status, len(resp.Body))
		}
	}
}

// --- zlib level experiment: unpooled encode at an arbitrary level, for
// comparison against the production (pooled, level 6) encodePack. ---

func encodePackLevel(objects []PackObject, level int) []byte {
	var body bytes.Buffer
	body.Grow(12)
	body.WriteString("PACK")
	body.Write([]byte{0, 0, 0, 2})
	n := len(objects)
	body.Write([]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
	for _, obj := range objects {
		body.Write(encodeObjectHeader(obj.Type, len(obj.Content)))
		zw, _ := zlib.NewWriterLevel(&body, level)
		zw.Write(obj.Content)
		zw.Close()
	}
	sum := sha1.Sum(body.Bytes())
	return append(body.Bytes(), sum[:]...)
}

func BenchmarkEncodePackShallowL1(b *testing.B) {
	repo := seedRepo200(b)
	args := shallowFetchArgs(repo)
	res := reachableFrom(repo, args.wants, map[string]bool{}, args.deepen)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if out := encodePackLevel(res.Objects, 1); len(out) == 0 {
			b.Fatal("empty")
		}
	}
}

func BenchmarkEncodePackShallow(b *testing.B) {
	repo := seedRepo200(b)
	args := shallowFetchArgs(repo)
	res := reachableFrom(repo, args.wants, map[string]bool{}, args.deepen)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if out := encodePack(res.Objects); len(out) == 0 {
			b.Fatal("empty")
		}
	}
}

func BenchmarkEncodePackFull(b *testing.B) {
	repo := seedRepo200(b)
	res := reachableFrom(repo, []string{repo.DefaultTip()}, map[string]bool{}, nil)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if out := encodePack(res.Objects); len(out) == 0 {
			b.Fatal("empty")
		}
	}
}

func BenchmarkEncodePackFullL1(b *testing.B) {
	repo := seedRepo200(b)
	res := reachableFrom(repo, []string{repo.DefaultTip()}, map[string]bool{}, nil)
	b.ResetTimer()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if out := encodePackLevel(res.Objects, 1); len(out) == 0 {
			b.Fatal("empty")
		}
	}
}
