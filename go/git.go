// git.go — pure git object model, pack codec, pkt-line and smart-HTTP
// protocol (v0 + v2). Port of git.ts: same behavior, same byte output
// where the wire is concerned. Hot paths are allocation-light:
// single-inflate pack ingest (the zlib stream's length is found by the
// reader itself, no binary-search re-inflation), SHA-1 straight into the
// hash, no per-object Buffer.concat.
package main

import (
	"bytes"
	"github.com/klauspost/compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Pooled zlib (de)compressors: a fresh compress/zlib writer allocates ~800 KiB
// of compressor state per object, which dominated encodePack (2.4 MiB/op for
// a 3-object shallow fetch). Writers are Reset before each use; readers are
// Reset with a fresh bytes.Reader. sync.Pool is goroutine-safe, so 50
// concurrent clones each get their own compressor.
var zlibWriterPool = sync.Pool{New: func() interface{} {
	zw, _ := zlib.NewWriterLevel(nil, 6)
	return zw
}}

var emptyZlibStream = func() []byte {
	var b bytes.Buffer
	zw := zlib.NewWriter(&b)
	zw.Write([]byte{})
	zw.Close()
	return b.Bytes()
}()

var zlibReaderPool = sync.Pool{New: func() interface{} {
	zr, _ := zlib.NewReader(bytes.NewReader(emptyZlibStream))
	return zr
}}

type ObjectType string

const (
	ObjCommit ObjectType = "commit"
	ObjTree   ObjectType = "tree"
	ObjBlob   ObjectType = "blob"
	ObjTag    ObjectType = "tag"
)

type GitObject struct {
	Type    ObjectType
	Content []byte
}

const ZeroOID = "0000000000000000000000000000000000000000"
const ViewerLogin = "adaiversen" // fixed scenario viewer (SCENARIO_SEED 0x6a09e667f3bcc909)
const ViewerID = 1000
const ViewerName = "ada iversen"
const ViewerEmail = "adaiversen@example.invalid"

var typeCodes = map[ObjectType]byte{ObjCommit: 1, ObjTree: 2, ObjBlob: 3, ObjTag: 4}
var codeTypes = map[byte]ObjectType{1: ObjCommit, 2: ObjTree, 3: ObjBlob, 4: ObjTag}

func sha1Hex(data []byte) string {
	h := sha1.Sum(data)
	return hex.EncodeToString(h[:])
}

// hashObject hashes "type len\0content" without concatenating first.
func hashObject(typ ObjectType, content []byte) string {
	h := sha1.New()
	h.Write([]byte(string(typ) + " " + strconv.Itoa(len(content)) + "\x00"))
	h.Write(content)
	return hex.EncodeToString(h.Sum(nil))
}

func oidBytes(oid string) []byte {
	b, _ := hex.DecodeString(oid)
	return b
}

type CommitParts struct {
	Tree          string
	Parents       []string
	AuthorName    string
	AuthorEmail   string
	AuthorTs      int64
	CommitterName string
	CommitterEmail string
	CommitterTs   int64
	Message       string
}

func buildCommit(tree string, parents []string, message, authorName, authorEmail string, timestamp int64) (string, []byte) {
	var sb strings.Builder
	sb.WriteString("tree " + tree + "\n")
	for _, p := range parents {
		sb.WriteString("parent " + p + "\n")
	}
	sb.WriteString("author " + authorName + " <" + authorEmail + "> " + strconv.FormatInt(timestamp, 10) + " +0000\n")
	sb.WriteString("committer " + authorName + " <" + authorEmail + "> " + strconv.FormatInt(timestamp, 10) + " +0000\n")
	sb.WriteString("\n")
	if strings.HasSuffix(message, "\n") {
		sb.WriteString(message)
	} else {
		sb.WriteString(message + "\n")
	}
	content := []byte(sb.String())
	return hashObject(ObjCommit, content), content
}

// parseIdent parses "NAME <EMAIL> TS +0000" after the "author "/"committer " prefix.
func parseIdent(s string) (name, email string, ts int64) {
	lt := strings.IndexByte(s, '<')
	gt := strings.IndexByte(s, '>')
	if lt < 0 || gt < 0 || gt < lt {
		return s, "", 0
	}
	name = strings.TrimSuffix(s[:lt], " ")
	email = s[lt+1 : gt]
	rest := strings.TrimSpace(s[gt+1:])
	if i := strings.IndexByte(rest, ' '); i >= 0 {
		rest = rest[:i]
	}
	ts, _ = strconv.ParseInt(rest, 10, 64)
	return name, email, ts
}

func parseCommit(content []byte) CommitParts {
	text := string(content)
	headerEnd := strings.Index(text, "\n\n")
	header, message := text, ""
	if headerEnd >= 0 {
		header = text[:headerEnd]
		message = text[headerEnd+2:]
	}
	parts := CommitParts{Message: strings.TrimSuffix(message, "\n")}
	for _, line := range strings.Split(header, "\n") {
		switch {
		case strings.HasPrefix(line, "tree "):
			parts.Tree = strings.TrimSpace(line[5:])
		case strings.HasPrefix(line, "parent "):
			parts.Parents = append(parts.Parents, strings.TrimSpace(line[7:]))
		case strings.HasPrefix(line, "author "):
			parts.AuthorName, parts.AuthorEmail, parts.AuthorTs = parseIdent(line[7:])
		case strings.HasPrefix(line, "committer "):
			parts.CommitterName, parts.CommitterEmail, parts.CommitterTs = parseIdent(line[10:])
		}
	}
	return parts
}

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

func buildTree(entries []TreeEntry) []byte {
	sorted := make([]TreeEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool {
		ki, kj := sorted[i].Name, sorted[j].Name
		if sorted[i].Mode == "40000" {
			ki += "/"
		}
		if sorted[j].Mode == "40000" {
			kj += "/"
		}
		return ki < kj
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.Mode + " " + e.Name + "\x00")
		buf.Write(oidBytes(e.OID))
	}
	return buf.Bytes()
}

func parseTree(content []byte) []TreeEntry {
	var entries []TreeEntry
	pos := 0
	for pos < len(content) {
		sp := bytes.IndexByte(content[pos:], ' ')
		if sp < 0 {
			break
		}
		sp += pos
		nul := bytes.IndexByte(content[sp:], 0)
		if nul < 0 {
			break
		}
		nul += sp
		mode := string(content[pos:sp])
		name := string(content[sp+1 : nul])
		oid := hex.EncodeToString(content[nul+1 : nul+21])
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid})
		pos = nul + 21
	}
	return entries
}

type TagParts struct {
	Object string
	Type   ObjectType
	Tag    string
	Tagger string
}

func parseTag(content []byte) TagParts {
	text := string(content)
	headerEnd := strings.Index(text, "\n\n")
	header := text
	if headerEnd >= 0 {
		header = text[:headerEnd]
	}
	parts := TagParts{Type: ObjCommit}
	for _, line := range strings.Split(header, "\n") {
		switch {
		case strings.HasPrefix(line, "object "):
			parts.Object = strings.TrimSpace(line[7:])
		case strings.HasPrefix(line, "type "):
			parts.Type = ObjectType(strings.TrimSpace(line[5:]))
		case strings.HasPrefix(line, "tag "):
			parts.Tag = strings.TrimSpace(line[4:])
		case strings.HasPrefix(line, "tagger "):
			parts.Tagger = strings.TrimSpace(line[7:])
		}
	}
	return parts
}

// ---------------------------------------------------------------------------
// pack codec
// ---------------------------------------------------------------------------

func encodeObjectHeader(typ ObjectType, size int) []byte {
	code := typeCodes[typ]
	var out []byte
	b := (code << 4) | byte(size&0x0f)
	size >>= 4
	if size > 0 {
		b |= 0x80
	}
	out = append(out, b)
	for size > 0 {
		b = byte(size & 0x7f)
		size >>= 7
		if size > 0 {
			b |= 0x80
		}
		out = append(out, b)
	}
	return out
}

type PackObject struct {
	OID     string
	Type    ObjectType
	Content []byte
}

func encodePack(objects []PackObject) []byte {
	var body bytes.Buffer
	// Pre-size for the deflated payload: git objects deflate hard (the
	// bench repo's 600 objects are ~600 KiB raw but ~46 KiB packed), so
	// content/8 + slack avoids repeated buffer growth without
	// over-reserving for the common shallow fetch (3 objects, ~3 KiB raw).
	totalContent := 0
	for _, obj := range objects {
		totalContent += len(obj.Content)
	}
	body.Grow(totalContent/8 + 256)
	body.WriteString("PACK")
	var tmp [4]byte
	tmp[0] = 0
	tmp[1] = 0
	tmp[2] = 0
	tmp[3] = 2
	body.Write(tmp[:])
	n := len(objects)
	tmp[0] = byte(n >> 24)
	tmp[1] = byte(n >> 16)
	tmp[2] = byte(n >> 8)
	tmp[3] = byte(n)
	body.Write(tmp[:])
	var hdr [8]byte
	for _, obj := range objects {
		// Object header into a stack array (no per-object slice alloc).
		hdrLen := 0
		code := typeCodes[obj.Type]
		size := len(obj.Content)
		b := (code << 4) | byte(size&0x0f)
		size >>= 4
		if size > 0 {
			b |= 0x80
		}
		hdr[hdrLen] = b
		hdrLen++
		for size > 0 {
			b = byte(size & 0x7f)
			size >>= 7
			if size > 0 {
				b |= 0x80
			}
			hdr[hdrLen] = b
			hdrLen++
		}
		body.Write(hdr[:hdrLen])
		zw := zlibWriterPool.Get().(*zlib.Writer)
		zw.Reset(&body)
		zw.Write(obj.Content)
		zw.Close()
		zlibWriterPool.Put(zw)
	}
	sum := sha1.Sum(body.Bytes())
	// Write the trailer checksum into the same buffer instead of copying
	// the whole pack once more with append.
	body.Write(sum[:])
	return body.Bytes()
}

// inflateOne inflates exactly one zlib stream starting at pos and reports
// the data plus the bytes consumed — a single inflate. The bytes.Reader is
// an io.ByteReader, so flate reads one byte at a time and stops at the
// stream's final byte; the reader position after the read is the exact end
// of the stream (zlib streams end on a byte boundary).
func inflateOne(buf []byte, pos int) ([]byte, int, error) {
	br := bytes.NewReader(buf[pos:])
	zr := zlibReaderPool.Get().(io.ReadCloser)
	if err := zr.(zlib.Resetter).Reset(br, nil); err != nil {
		zlibReaderPool.Put(zr)
		return nil, 0, errors.New("pack: bad zlib stream")
	}
	data, err := io.ReadAll(zr)
	zr.Close()
	zlibReaderPool.Put(zr)
	if err != nil {
		return nil, 0, errors.New("pack: bad zlib stream")
	}
	consumed := len(buf[pos:]) - br.Len()
	return data, consumed, nil
}

type packEntry struct {
	typ        int // 1..4 object, 6 ofs-delta, 7 ref-delta
	size       int
	data       []byte
	baseOID    string
	baseOffset int
}

func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	readVarint := func() (int, error) {
		value := 0
		shift := uint(0)
		for {
			if pos >= len(delta) {
				return 0, errors.New("pack: delta truncated")
			}
			b := delta[pos]
			pos++
			value |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return value, nil
	}
	srcSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	tgtSize, err := readVarint()
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, errors.New("pack: delta base size mismatch")
	}
	out := make([]byte, 0, tgtSize)
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			offset, size := 0, 0
			if op&0x01 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("pack: delta truncated")
				}
				offset = int(delta[pos])
				pos++
			}
			if op&0x02 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("pack: delta truncated")
				}
				offset |= int(delta[pos]) << 8
				pos++
			}
			if op&0x04 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("pack: delta truncated")
				}
				offset |= int(delta[pos]) << 16
				pos++
			}
			if op&0x08 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("pack: delta truncated")
				}
				offset |= int(delta[pos]) << 24
				pos++
			}
			if op&0x10 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("pack: delta truncated")
				}
				size = int(delta[pos])
				pos++
			}
			if op&0x20 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("pack: delta truncated")
				}
				size |= int(delta[pos]) << 8
				pos++
			}
			if op&0x40 != 0 {
				if pos >= len(delta) {
					return nil, errors.New("pack: delta truncated")
				}
				size |= int(delta[pos]) << 16
				pos++
			}
			if size == 0 {
				size = 0x10000
			}
			if offset < 0 || size < 0 || offset+size > len(base) {
				return nil, errors.New("pack: delta copy out of range")
			}
			out = append(out, base[offset:offset+size]...)
		} else if op != 0 {
			if pos+int(op) > len(delta) {
				return nil, errors.New("pack: delta truncated")
			}
			out = append(out, delta[pos:pos+int(op)]...)
			pos += int(op)
		} else {
			return nil, errors.New("pack: delta opcode 0")
		}
	}
	if len(out) != tgtSize {
		return nil, errors.New("pack: delta target size mismatch")
	}
	return out, nil
}

func decodePack(buf []byte, resolveBase func(oid string) *GitObject) (map[string]*GitObject, error) {
	if len(buf) < 12 || string(buf[0:4]) != "PACK" {
		return nil, errors.New("pack: bad magic")
	}
	version := int(buf[4])<<24 | int(buf[5])<<16 | int(buf[6])<<8 | int(buf[7])
	if version != 2 {
		return nil, fmt.Errorf("pack: unsupported version %d", version)
	}
	count := int(buf[8])<<24 | int(buf[9])<<16 | int(buf[10])<<8 | int(buf[11])
	pos := 12
	entries := make([]packEntry, 0, count)
	offsets := make([]int, 0, count)
	for i := 0; i < count; i++ {
		start := pos
		offsets = append(offsets, start)
		first := buf[pos]
		pos++
		typeCode := (first >> 4) & 0x07
		size := int(first & 0x0f)
		shift := uint(4)
		for first&0x80 != 0 {
			b := buf[pos]
			pos++
			size |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		if typeCode == 6 {
			b := buf[pos]
			pos++
			off := int(b & 0x7f)
			for b&0x80 != 0 {
				b = buf[pos]
				pos++
				off = ((off + 1) << 7) | int(b&0x7f)
			}
			data, consumed, err := inflateOne(buf, pos)
			if err != nil {
				return nil, err
			}
			pos += consumed
			entries = append(entries, packEntry{typ: 6, size: size, data: data, baseOffset: start - off})
		} else if typeCode == 7 {
			baseOID := hex.EncodeToString(buf[pos : pos+20])
			pos += 20
			data, consumed, err := inflateOne(buf, pos)
			if err != nil {
				return nil, err
			}
			pos += consumed
			entries = append(entries, packEntry{typ: 7, size: size, data: data, baseOID: baseOID})
		} else {
			typ, ok := codeTypes[typeCode]
			if !ok {
				return nil, fmt.Errorf("pack: unknown object type %d", typeCode)
			}
			data, consumed, err := inflateOne(buf, pos)
			if err != nil {
				return nil, err
			}
			pos += consumed
			entries = append(entries, packEntry{typ: int(typeCode), size: size, data: data})
			_ = typ
		}
	}
	byOffset := make(map[int]*GitObject, count)
	byOID := make(map[string]*GitObject, count)
	resolved := make(map[string]*GitObject, count)
	var resolveEntry func(idx, depth int) (*GitObject, error)
	resolveEntry = func(idx, depth int) (*GitObject, error) {
		if depth > 64 {
			return nil, errors.New("pack: delta chain too deep")
		}
		entry := entries[idx]
		if entry.typ != 6 && entry.typ != 7 {
			obj := &GitObject{Type: codeTypes[byte(entry.typ)], Content: entry.data}
			oid := hashObject(obj.Type, obj.Content)
			byOffset[offsets[idx]] = obj
			if _, ok := byOID[oid]; !ok {
				byOID[oid] = obj
			}
			resolved[oid] = obj
			return obj, nil
		}
		var base *GitObject
		if entry.typ == 6 {
			base = byOffset[entry.baseOffset]
			if base == nil {
				baseIdx := -1
				for j, off := range offsets {
					if off == entry.baseOffset {
						baseIdx = j
						break
					}
				}
				if baseIdx < 0 {
					return nil, errors.New("pack: ofs-delta base not in pack")
				}
				var err error
				base, err = resolveEntry(baseIdx, depth+1)
				if err != nil {
					return nil, err
				}
			}
		} else {
			base = resolveBase(entry.baseOID)
			if base == nil {
				base = byOID[entry.baseOID]
			}
			if base == nil {
				// base may be an earlier pack entry with that oid
				for j := 0; j < idx; j++ {
					if entries[j].typ == 6 || entries[j].typ == 7 {
						continue
					}
					cand, err := resolveEntry(j, depth+1)
					if err != nil {
						return nil, err
					}
					if hashObject(cand.Type, cand.Content) == entry.baseOID {
						base = cand
						break
					}
				}
			}
			if base == nil {
				return nil, fmt.Errorf("pack: ref-delta base %s not found", entry.baseOID)
			}
		}
		content, err := applyDelta(base.Content, entry.data)
		if err != nil {
			return nil, err
		}
		obj := &GitObject{Type: base.Type, Content: content}
		oid := hashObject(obj.Type, obj.Content)
		byOffset[offsets[idx]] = obj
		if _, ok := byOID[oid]; !ok {
			byOID[oid] = obj
		}
		resolved[oid] = obj
		return obj, nil
	}
	for i := range entries {
		if _, err := resolveEntry(i, 0); err != nil {
			return nil, err
		}
	}
	return resolved, nil
}

// ---------------------------------------------------------------------------
// pkt-line
// ---------------------------------------------------------------------------

type PktLine struct {
	Kind string // "data" | "flush" | "delim" | "end"
	Data []byte
}

func pktLine(data []byte) []byte {
	l := len(data) + 4
	head := fmt.Sprintf("%04x", l)
	out := make([]byte, 0, l)
	out = append(out, head...)
	out = append(out, data...)
	return out
}

func pktLineStr(s string) []byte { return pktLine([]byte(s)) }

var flushPkt = []byte("0000")
var delimPkt = []byte("0001")

func parsePktLines(buf []byte) []PktLine {
	var out []PktLine
	pos := 0
	for pos+4 <= len(buf) {
		lenHex := string(buf[pos : pos+4])
		l, err := strconv.ParseInt(lenHex, 16, 32)
		if err != nil {
			break
		}
		switch l {
		case 0:
			out = append(out, PktLine{Kind: "flush"})
			pos += 4
		case 1:
			out = append(out, PktLine{Kind: "delim"})
			pos += 4
		case 2:
			out = append(out, PktLine{Kind: "end"})
			pos += 4
		default:
			if pos+int(l) > len(buf) {
				return out
			}
			out = append(out, PktLine{Kind: "data", Data: buf[pos+4 : pos+int(l)]})
			pos += int(l)
		}
	}
	return out
}

// pktLineLen returns the total wire length of one parsed line.
func pktLineLen(l PktLine) int {
	if l.Kind == "data" {
		return len(l.Data) + 4
	}
	return 4
}

func sidebandPacket(band byte, data []byte) []byte {
	var out []byte
	const maxChunk = 65516 - 1
	for off := 0; off < len(data); {
		end := off + maxChunk
		if end > len(data) {
			end = len(data)
		}
		payload := make([]byte, 0, end-off+1)
		payload = append(payload, band)
		payload = append(payload, data[off:end]...)
		out = append(out, pktLine(payload)...)
		off = end
	}
	return out
}

// ---------------------------------------------------------------------------
// reachability / pack selection
// ---------------------------------------------------------------------------

type RepoObjects interface {
	GetObject(oid string) *GitObject
	GetRef(name string) string
}

// reachableObjects: full object closure reachable from tips.
func reachableObjects(repo RepoObjects, tips []string, stopAt map[string]bool) map[string]bool {
	out := make(map[string]bool)
	commitQueue := append([]string(nil), tips...)
	seenCommits := make(map[string]bool)
	for len(commitQueue) > 0 {
		oid := commitQueue[len(commitQueue)-1]
		commitQueue = commitQueue[:len(commitQueue)-1]
		if seenCommits[oid] {
			continue
		}
		seenCommits[oid] = true
		obj := repo.GetObject(oid)
		if obj == nil {
			continue
		}
		out[oid] = true
		if obj.Type == ObjCommit {
			parts := parseCommit(obj.Content)
			var walk func(treeOID string)
			walk = func(treeOID string) {
				if out[treeOID] {
					return
				}
				out[treeOID] = true
				tree := repo.GetObject(treeOID)
				if tree == nil || tree.Type != ObjTree {
					return
				}
				for _, e := range parseTree(tree.Content) {
					if e.Mode == "40000" {
						walk(e.OID)
					} else {
						out[e.OID] = true
					}
				}
			}
			walk(parts.Tree)
			if stopAt != nil && stopAt[oid] {
				continue
			}
			commitQueue = append(commitQueue, parts.Parents...)
		} else if obj.Type == ObjTag {
			tag := parseTag(obj.Content)
			if !seenCommits[tag.Object] {
				commitQueue = append(commitQueue, tag.Object)
			}
		}
	}
	return out
}

type ReachableResult struct {
	Commits  []string
	Boundary []string
	Objects  []PackObject
}

func reachableFrom(repo RepoObjects, tips []string, exclude map[string]bool, depth *int) ReachableResult {
	seen := make(map[string]bool, len(exclude))
	for k := range exclude {
		seen[k] = true
	}
	var commits []string
	var tags []string
	type qitem struct {
		oid  string
		dist int
	}
	queue := make([]qitem, 0, len(tips))
	for _, oid := range tips {
		queue = append(queue, qitem{oid, 0})
	}
	visited := make(map[string]bool)
	head := 0
	for head < len(queue) {
		it := queue[head]
		head++
		oid, dist := it.oid, it.dist
		if visited[oid] {
			continue
		}
		visited[oid] = true
		obj := repo.GetObject(oid)
		if obj == nil {
			continue
		}
		if obj.Type == ObjTag {
			if !seen[oid] {
				tags = append(tags, oid)
			}
			target := parseTag(obj.Content).Object
			if !visited[target] {
				queue = append(queue, qitem{target, dist})
			}
			continue
		}
		if obj.Type != ObjCommit {
			continue
		}
		if !seen[oid] {
			commits = append(commits, oid)
		}
		parts := parseCommit(obj.Content)
		if depth == nil || dist+1 < *depth {
			for _, p := range parts.Parents {
				queue = append(queue, qitem{p, dist + 1})
			}
		}
	}
	included := make(map[string]bool, len(commits))
	for _, oid := range commits {
		included[oid] = true
	}
	var boundary []string
	for _, oid := range commits {
		obj := repo.GetObject(oid)
		if obj == nil {
			continue
		}
		parents := parseCommit(obj.Content).Parents
		all := true
		for _, p := range parents {
			if !included[p] {
				all = false
				break
			}
		}
		if len(parents) == 0 || !all {
			boundary = append(boundary, oid)
		}
	}
	var objects []PackObject
	objSeen := make(map[string]bool)
	addObj := func(oid string) {
		if objSeen[oid] || seen[oid] {
			return
		}
		obj := repo.GetObject(oid)
		if obj == nil {
			return
		}
		objSeen[oid] = true
		objects = append(objects, PackObject{OID: oid, Type: obj.Type, Content: obj.Content})
	}
	var walkTree func(treeOID string)
	walkTree = func(treeOID string) {
		if seen[treeOID] || objSeen[treeOID] {
			return
		}
		addObj(treeOID)
		obj := repo.GetObject(treeOID)
		if obj == nil || obj.Type != ObjTree {
			return
		}
		for _, e := range parseTree(obj.Content) {
			if e.Mode == "40000" {
				walkTree(e.OID)
			} else {
				addObj(e.OID)
			}
		}
	}
	var commitObjs []PackObject
	for _, oid := range commits {
		obj := repo.GetObject(oid)
		if obj != nil {
			commitObjs = append(commitObjs, PackObject{OID: oid, Type: obj.Type, Content: obj.Content})
		}
	}
	var tagObjs []PackObject
	for _, oid := range tags {
		obj := repo.GetObject(oid)
		if obj != nil {
			tagObjs = append(tagObjs, PackObject{OID: oid, Type: obj.Type, Content: obj.Content})
		}
	}
	for _, oid := range commits {
		obj := repo.GetObject(oid)
		if obj == nil {
			continue
		}
		parts := parseCommit(obj.Content)
		walkTree(parts.Tree)
	}
	all := make([]PackObject, 0, len(commitObjs)+len(tagObjs)+len(objects))
	all = append(all, commitObjs...)
	all = append(all, tagObjs...)
	all = append(all, objects...)
	return ReachableResult{Commits: commits, Boundary: boundary, Objects: all}
}

func isAncestor(repo RepoObjects, ancestor, tip string) bool {
	if ancestor == tip {
		return true
	}
	seen := make(map[string]bool)
	stack := []string{tip}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if oid == ancestor {
			return true
		}
		if seen[oid] {
			continue
		}
		seen[oid] = true
		obj := repo.GetObject(oid)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		stack = append(stack, parseCommit(obj.Content).Parents...)
	}
	return false
}

func mergeBase(repo RepoObjects, a, b string) string {
	ancestorsA := make(map[string]bool)
	stackA := []string{a}
	for len(stackA) > 0 {
		oid := stackA[len(stackA)-1]
		stackA = stackA[:len(stackA)-1]
		if ancestorsA[oid] {
			continue
		}
		ancestorsA[oid] = true
		obj := repo.GetObject(oid)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		stackA = append(stackA, parseCommit(obj.Content).Parents...)
	}
	stackB := []string{b}
	seenB := make(map[string]bool)
	for len(stackB) > 0 {
		oid := stackB[len(stackB)-1]
		stackB = stackB[:len(stackB)-1]
		if ancestorsA[oid] {
			return oid
		}
		if seenB[oid] {
			continue
		}
		seenB[oid] = true
		obj := repo.GetObject(oid)
		if obj == nil || obj.Type != ObjCommit {
			continue
		}
		stackB = append(stackB, parseCommit(obj.Content).Parents...)
	}
	return ""
}

// flattenTree: path -> {oid, mode}.
type FlatFile struct {
	OID  string
	Mode string
}

func flattenTree(repo RepoObjects, treeOID string) map[string]FlatFile {
	out := make(map[string]FlatFile)
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		obj := repo.GetObject(oid)
		if obj == nil || obj.Type != ObjTree {
			return
		}
		for _, e := range parseTree(obj.Content) {
			path := e.Name
			if prefix != "" {
				path = prefix + "/" + e.Name
			}
			if e.Mode == "40000" {
				walk(e.OID, path)
			} else {
				out[path] = FlatFile{OID: e.OID, Mode: e.Mode}
			}
		}
	}
	walk(treeOID, "")
	return out
}

type ObjectStore interface {
	GetObject(oid string) *GitObject
	PutObject(obj *GitObject) string
}

// buildTreeFromFiles builds nested trees bottom-up from a flat file map.
func buildTreeFromFiles(repo ObjectStore, files map[string]FlatFile) string {
	var buildDir func(prefix string, m map[string]FlatFile) string
	buildDir = func(prefix string, m map[string]FlatFile) string {
		var entries []TreeEntry
		subdirs := make(map[string]map[string]FlatFile)
		var subdirNames []string
		for path, file := range m {
			rest := path[len(prefix):]
			if slash := strings.IndexByte(rest, '/'); slash < 0 {
				entries = append(entries, TreeEntry{Mode: file.Mode, Name: rest, OID: file.OID})
			} else {
				dir := rest[:slash]
				sub, ok := subdirs[dir]
				if !ok {
					sub = make(map[string]FlatFile)
					subdirs[dir] = sub
					subdirNames = append(subdirNames, dir)
				}
				sub[path] = file
			}
		}
		sort.Strings(subdirNames)
		for _, dir := range subdirNames {
			treeOID := buildDir(prefix+dir+"/", subdirs[dir])
			entries = append(entries, TreeEntry{Mode: "40000", Name: dir, OID: treeOID})
		}
		content := buildTree(entries)
		return repo.PutObject(&GitObject{Type: ObjTree, Content: content})
	}
	return buildDir("", files)
}
