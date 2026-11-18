// githttp.go — git smart-HTTP handlers: info/refs (v0 + v2), upload-pack
// (v2 ls-refs / fetch, v0 fetch), receive-pack (v0). Port of githttp.ts.
package main

import (
	"sort"
	"strings"
)

var v2Caps = []string{"fetch=shallow filter", "ls-refs=unborn"}

const v0UploadCaps = "multi_ack thin-pack side-band-64k side-band ofs-delta shallow symref=HEAD:refs/heads/main agent=forge/1.0"
const v0ReceiveCaps = "report-status side-band-64k delete-refs ofs-delta agent=forge/1.0"

func gitNotFound() Resp {
	return Resp{Status: 404, Body: []byte(`{"message":"Not Found"}`), ContentType: "application/json"}
}

func refLines(repo *Repo, caps string) [][]byte {
	var lines [][]byte
	type refEnt struct {
		name, oid string
	}
	var refs []refEnt
	for ref, oid := range repo.Refs {
		refs = append(refs, refEnt{ref, oid})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].name < refs[j].name })
	headTip := repo.DefaultTip()
	if len(refs) == 0 {
		lines = append(lines, pktLineStr(ZeroOID+" capabilities^{}\x00"+caps+"\n"))
		return lines
	}
	first := true
	if headTip != "" {
		suffix := ""
		if first {
			suffix = "\x00" + caps
		}
		lines = append(lines, pktLineStr(headTip+" HEAD"+suffix+"\n"))
		first = false
	}
	for _, r := range refs {
		suffix := ""
		if first {
			suffix = "\x00" + caps
		}
		lines = append(lines, pktLineStr(r.oid+" "+r.name+suffix+"\n"))
		first = false
		obj := repo.GetObject(r.oid)
		if obj != nil && obj.Type == ObjTag && strings.HasPrefix(r.name, "refs/tags/") {
			tag := parseTag(obj.Content)
			lines = append(lines, pktLineStr(tag.Object+" "+r.name+"^{}\n"))
		}
	}
	return lines
}

func handleInfoRefs(repo *Repo, service string, gitProtocolHeader string) Resp {
	if repo == nil {
		return gitNotFound()
	}
	contentType := "application/x-git-upload-pack-advertisement"
	if service == "git-receive-pack" {
		contentType = "application/x-git-receive-pack-advertisement"
	}
	wantsV2 := service == "git-upload-pack" && strings.Contains(gitProtocolHeader, "version=2")
	if wantsV2 {
		var chunks [][]byte
		chunks = append(chunks, pktLineStr("version 2\n"))
		for _, cap := range v2Caps {
			chunks = append(chunks, pktLineStr(cap+"\n"))
		}
		chunks = append(chunks, flushPkt)
		return Resp{Status: 200, Body: concat(chunks), ContentType: contentType}
	}
	serviceName := service
	caps := v0UploadCaps
	if service == "git-receive-pack" {
		caps = v0ReceiveCaps
	}
	chunks := [][]byte{pktLineStr("# service=" + serviceName + "\n"), flushPkt}
	chunks = append(chunks, refLines(repo, caps)...)
	chunks = append(chunks, flushPkt)
	return Resp{Status: 200, Body: concat(chunks), ContentType: contentType}
}

func concat(chunks [][]byte) []byte {
	n := 0
	for _, c := range chunks {
		n += len(c)
	}
	out := make([]byte, 0, n)
	for _, c := range chunks {
		out = append(out, c...)
	}
	return out
}

// ---------------------------------------------------------------------------
// upload-pack
// ---------------------------------------------------------------------------

type fetchArgs struct {
	wants   []string
	haves   []string
	shallow []string
	deepen  *int
}

func parseFetchArgs(lines []PktLine) fetchArgs {
	args := fetchArgs{}
	pastDelim := false
	for _, line := range lines {
		if line.Kind == "delim" {
			pastDelim = true
			continue
		}
		if line.Kind != "data" {
			continue
		}
		text := strings.TrimSuffix(string(line.Data), "\n")
		if !pastDelim {
			continue
		}
		switch {
		case strings.HasPrefix(text, "want "):
			args.wants = append(args.wants, strings.TrimSpace(text[5:]))
		case strings.HasPrefix(text, "have "):
			args.haves = append(args.haves, strings.TrimSpace(text[5:]))
		case strings.HasPrefix(text, "shallow "):
			args.shallow = append(args.shallow, strings.TrimSpace(text[8:]))
		case strings.HasPrefix(text, "deepen "):
			n := atoiSafe(strings.TrimSpace(text[7:]))
			args.deepen = &n
		}
	}
	return args
}

func atoiSafe(s string) int {
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

func lsRefsResponse(repo *Repo, lines []PktLine) []byte {
	peel := false
	var prefixes []string
	pastDelim := false
	for _, line := range lines {
		if line.Kind == "delim" {
			pastDelim = true
			continue
		}
		if line.Kind != "data" || !pastDelim {
			continue
		}
		text := strings.TrimSuffix(string(line.Data), "\n")
		if text == "peel" {
			peel = true
		} else if strings.HasPrefix(text, "ref-prefix ") {
			prefixes = append(prefixes, text[len("ref-prefix "):])
		}
	}
	matches := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var chunks [][]byte
	tip := repo.DefaultTip()
	if matches("HEAD") {
		if tip != "" {
			chunks = append(chunks, pktLineStr(tip+" HEAD symref-target:refs/heads/"+repo.DefaultBranch+"\n"))
		} else {
			chunks = append(chunks, pktLineStr("unborn HEAD symref-target:refs/heads/"+repo.DefaultBranch+"\n"))
		}
	}
	type refEnt struct {
		name, oid string
	}
	var refs []refEnt
	for ref, oid := range repo.Refs {
		refs = append(refs, refEnt{ref, oid})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].name < refs[j].name })
	for _, r := range refs {
		if !matches(r.name) {
			continue
		}
		obj := repo.GetObject(r.oid)
		if peel && obj != nil && obj.Type == ObjTag && strings.HasPrefix(r.name, "refs/tags/") {
			tag := parseTag(obj.Content)
			chunks = append(chunks, pktLineStr(r.oid+" "+r.name+" peeled:"+tag.Object+"\n"))
		} else {
			chunks = append(chunks, pktLineStr(r.oid+" "+r.name+"\n"))
		}
	}
	chunks = append(chunks, flushPkt)
	return concat(chunks)
}

func fetchResponse(repo *Repo, args fetchArgs, v2 bool) []byte {
	var chunks [][]byte
	shallowSet := make(map[string]bool, len(args.shallow))
	for _, s := range args.shallow {
		shallowSet[s] = true
	}
	exclude := reachableObjects(repo, args.haves, shallowSet)
	res := reachableFrom(repo, args.wants, exclude, args.deepen)
	sentOids := make(map[string]bool, len(res.Objects))
	for _, o := range res.Objects {
		sentOids[o.OID] = true
	}

	ackLines := func() [][]byte {
		var out [][]byte
		if len(args.haves) == 0 {
			out = append(out, pktLineStr("NAK\n"))
		} else {
			acked := false
			for _, h := range args.haves {
				if repo.GetObject(h) != nil {
					out = append(out, pktLineStr("ACK "+h+"\n"))
					acked = true
				}
			}
			if !acked {
				out = append(out, pktLineStr("NAK\n"))
			}
			out = append(out, pktLineStr("ready\n"))
		}
		return out
	}

	if v2 {
		if len(args.haves) > 0 {
			chunks = append(chunks, pktLineStr("acknowledgments\n"))
			chunks = append(chunks, ackLines()...)
			chunks = append(chunks, delimPkt)
		}
		if args.deepen != nil {
			var unshallow []string
			for _, s := range args.shallow {
				obj := repo.GetObject(s)
				if obj == nil || obj.Type != ObjCommit {
					continue
				}
				parents := parseCommit(obj.Content).Parents
				if len(parents) > 0 {
					all := true
					for _, p := range parents {
						if !sentOids[p] {
							all = false
							break
						}
					}
					if all {
						unshallow = append(unshallow, s)
					}
				}
			}
			if len(res.Boundary) > 0 || len(unshallow) > 0 {
				chunks = append(chunks, pktLineStr("shallow-info\n"))
				for _, b := range res.Boundary {
					chunks = append(chunks, pktLineStr("shallow "+b+"\n"))
				}
				for _, u := range unshallow {
					chunks = append(chunks, pktLineStr("unshallow "+u+"\n"))
				}
				chunks = append(chunks, delimPkt)
			}
		}
		if len(res.Objects) > 0 {
			chunks = append(chunks, pktLineStr("packfile\n"))
			chunks = append(chunks, sidebandPacket(1, encodePack(res.Objects)))
			chunks = append(chunks, flushPkt)
		}
	} else {
		chunks = append(chunks, ackLines()...)
		if args.deepen != nil {
			for _, b := range res.Boundary {
				chunks = append(chunks, pktLineStr("shallow "+b+"\n"))
			}
		}
		if len(res.Objects) > 0 {
			chunks = append(chunks, sidebandPacket(1, encodePack(res.Objects)))
		}
		chunks = append(chunks, flushPkt)
	}
	return concat(chunks)
}

func handleUploadPack(repo *Repo, body []byte) Resp {
	if repo == nil {
		return gitNotFound()
	}
	lines := parsePktLines(body)
	var first *PktLine
	for i := range lines {
		if lines[i].Kind == "data" {
			first = &lines[i]
			break
		}
	}
	firstText := ""
	if first != nil {
		firstText = strings.TrimSuffix(string(first.Data), "\n")
	}
	if strings.HasPrefix(firstText, "command=ls-refs") {
		out := lsRefsResponse(repo, lines)
		return Resp{Status: 200, Body: out, ContentType: "application/x-git-upload-pack-result"}
	}
	if strings.HasPrefix(firstText, "command=fetch") {
		args := parseFetchArgs(lines)
		out := fetchResponse(repo, args, true)
		return Resp{Status: 200, Body: out, ContentType: "application/x-git-upload-pack-result"}
	}
	args := fetchArgs{}
	for _, line := range lines {
		if line.Kind != "data" {
			continue
		}
		text := strings.TrimSuffix(string(line.Data), "\n")
		if nul := strings.IndexByte(text, 0); nul >= 0 {
			text = text[:nul]
		}
		switch {
		case strings.HasPrefix(text, "want "):
			args.wants = append(args.wants, strings.TrimSpace(text[5:]))
		case strings.HasPrefix(text, "have "):
			args.haves = append(args.haves, strings.TrimSpace(text[5:]))
		case strings.HasPrefix(text, "shallow "):
			args.shallow = append(args.shallow, strings.TrimSpace(text[8:]))
		case strings.HasPrefix(text, "deepen "):
			n := atoiSafe(strings.TrimSpace(text[7:]))
			args.deepen = &n
		}
	}
	out := fetchResponse(repo, args, false)
	return Resp{Status: 200, Body: out, ContentType: "application/x-git-upload-pack-result"}
}

// ---------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------

type pushCommand struct {
	oldOID, newOID, ref string
}

func handleReceivePack(repo *Repo, body []byte) Resp {
	if repo == nil {
		return gitNotFound()
	}
	lines := parsePktLines(body)
	var commands []pushCommand
	clientCaps := make(map[string]bool)
	packStart := len(body)
	pos := 0
	for _, line := range lines {
		if line.Kind == "flush" {
			packStart = pos + 4
			break
		}
		if line.Kind == "data" {
			text := strings.TrimSuffix(string(line.Data), "\n")
			if nul := strings.IndexByte(text, 0); nul >= 0 {
				if len(commands) == 0 {
					for _, cap := range strings.Split(text[nul+1:], " ") {
						if cap != "" {
							clientCaps[strings.SplitN(cap, "=", 2)[0]] = true
						}
					}
				}
				text = text[:nul]
			}
			if len(text) >= 82 && text[40] == ' ' && text[81] == ' ' {
				oldOID, newOID, ref := text[:40], text[41:81], text[82:]
				if isHex40(oldOID) && isHex40(newOID) && ref != "" {
					commands = append(commands, pushCommand{oldOID, newOID, ref})
				}
			}
		}
		pos += pktLineLen(line)
	}
	packBytes := body[packStart:]
	unpackOk := true
	if len(packBytes) > 0 {
		objects, err := decodePack(packBytes, func(oid string) *GitObject { return repo.GetObject(oid) })
		if err != nil {
			unpackOk = false
		} else {
			for oid, obj := range objects {
				if _, ok := repo.Objects[oid]; !ok {
					repo.Objects[oid] = obj
				}
			}
		}
	}
	type cmdResult struct {
		ref    string
		ok     bool
		reason string
	}
	var results []cmdResult
	for _, cmd := range commands {
		if cmd.newOID == ZeroOID {
			if cmd.oldOID != ZeroOID {
				if cur, ok := repo.Refs[cmd.ref]; !ok || cur != cmd.oldOID {
					results = append(results, cmdResult{cmd.ref, false, "failed to delete"})
					continue
				}
			}
			delete(repo.Refs, cmd.ref)
			results = append(results, cmdResult{cmd.ref, true, ""})
			continue
		}
		newObj := repo.GetObject(cmd.newOID)
		isTagRef := strings.HasPrefix(cmd.ref, "refs/tags/")
		if newObj == nil || (isTagRef && newObj.Type != ObjCommit && newObj.Type != ObjTag) || (!isTagRef && newObj.Type != ObjCommit) {
			results = append(results, cmdResult{cmd.ref, false, "object not found"})
			continue
		}
		if cmd.oldOID == ZeroOID {
			repo.Refs[cmd.ref] = cmd.newOID
			results = append(results, cmdResult{cmd.ref, true, ""})
			continue
		}
		if isTagRef {
			cur, ok := repo.Refs[cmd.ref]
			if !ok || cur != cmd.oldOID {
				results = append(results, cmdResult{cmd.ref, false, "non-fast-forward"})
				continue
			}
			repo.Refs[cmd.ref] = cmd.newOID
			results = append(results, cmdResult{cmd.ref, true, ""})
			continue
		}
		cur, ok := repo.Refs[cmd.ref]
		if !ok || cur != cmd.oldOID {
			results = append(results, cmdResult{cmd.ref, false, "non-fast-forward"})
			continue
		}
		if !isAncestor(repo, cmd.oldOID, cmd.newOID) {
			results = append(results, cmdResult{cmd.ref, false, "non-fast-forward"})
			continue
		}
		repo.Refs[cmd.ref] = cmd.newOID
		results = append(results, cmdResult{cmd.ref, true, ""})
	}
	var reportLines [][]byte
	if unpackOk {
		reportLines = append(reportLines, pktLineStr("unpack ok\n"))
	} else {
		reportLines = append(reportLines, pktLineStr("unpack failed\n"))
	}
	for _, r := range results {
		if r.ok {
			reportLines = append(reportLines, pktLineStr("ok "+r.ref+"\n"))
		} else {
			reason := r.reason
			if reason == "" {
				reason = "failed"
			}
			reportLines = append(reportLines, pktLineStr("ng "+r.ref+" "+reason+"\n"))
		}
	}
	inner := concat(append(reportLines, flushPkt))
	var out []byte
	if clientCaps["side-band-64k"] {
		out = concat([][]byte{sidebandPacket(1, inner), flushPkt})
	} else {
		out = inner
	}
	return Resp{Status: 200, Body: out, ContentType: "application/x-git-receive-pack-result"}
}
