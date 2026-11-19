package main

import (
	"bytes"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Git smart HTTP: protocol v2 + v0 upload-pack, v0 receive-pack.
// ---------------------------------------------------------------------------

func pktLine(payload []byte) []byte {
	out := make([]byte, 4, len(payload)+4)
	l := len(payload) + 4
	const hexd = "0123456789abcdef"
	out[0] = hexd[(l>>12)&0xf]
	out[1] = hexd[(l>>8)&0xf]
	out[2] = hexd[(l>>4)&0xf]
	out[3] = hexd[l&0xf]
	return append(out, payload...)
}

func pktStr(s string) []byte { return pktLine([]byte(s)) }

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

// parsePktLines decodes a buffer into data payloads and the byte offset of the
// first byte after the terminating flush (or -1 when no flush is present).
func parsePktLines(buf []byte) ([][]byte, int) {
	var out [][]byte
	off := 0
	for off+4 <= len(buf) {
		if string(buf[off:off+4]) == "0000" {
			return out, off + 4
		}
		if string(buf[off:off+4]) == "0001" || string(buf[off:off+4]) == "0002" {
			off += 4
			continue
		}
		n := 0
		for i := 0; i < 4; i++ {
			c := buf[off+i]
			var v int
			switch {
			case c >= '0' && c <= '9':
				v = int(c - '0')
			case c >= 'a' && c <= 'f':
				v = int(c-'a') + 10
			case c >= 'A' && c <= 'F':
				v = int(c-'A') + 10
			default:
				return nil, -1
			}
			n = n*16 + v
		}
		if n < 4 || off+n > len(buf) {
			return nil, -1
		}
		out = append(out, buf[off+4:off+n])
		off += n
	}
	return out, -1
}

func sortedRefNames(refs map[string]string) []string {
	names := make([]string, 0, len(refs))
	for k := range refs {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// handleGit returns true when the route was handled.
func (s *srv) handleGit(w http.ResponseWriter, r *http.Request, segs []string) bool {
	if len(segs) < 2 || !strings.HasSuffix(segs[1], ".git") {
		return false
	}
	owner := segs[0]
	repoName := segs[1][:len(segs[1])-4]
	repo := s.store.getRepo(owner, repoName)
	rest := segs[2:]
	if len(rest) == 2 && rest[0] == "info" && rest[1] == "refs" && r.Method == "GET" {
		service := r.URL.Query().Get("service")
		if repo == nil {
			s.writeRaw(w, 404, "application/json", errMsg("Not Found"))
			return true
		}
		switch service {
		case "git-upload-pack":
			if r.Header.Get("Git-Protocol") == "version=2" {
				s.serveV2Advertisement(w, repo)
			} else {
				s.serveV0Advertisement(w, repo, "git-upload-pack")
			}
			return true
		case "git-receive-pack":
			s.serveV0Advertisement(w, repo, "git-receive-pack")
			return true
		}
		s.writeRaw(w, 400, "application/json", errMsg("Unknown service"))
		return true
	}
	if repo == nil {
		s.writeRaw(w, 404, "application/json", errMsg("Not Found"))
		return true
	}
	if len(rest) == 1 && rest[0] == "git-upload-pack" && r.Method == "POST" {
		s.handleUploadPack(w, r, repo)
		return true
	}
	if len(rest) == 1 && rest[0] == "git-receive-pack" && r.Method == "POST" {
		s.handleReceivePack(w, r, repo)
		return true
	}
	return false
}

func (s *srv) serveV2Advertisement(w http.ResponseWriter, repo *Repo) {
	var buf bytes.Buffer
	buf.Write(pktStr("version 2\n"))
	buf.Write(pktStr("agent=forge/1.0\n"))
	buf.Write(pktStr("ls-refs=unborn\n"))
	buf.Write(pktStr("fetch=shallow thin-pack\n"))
	buf.Write(pktStr("object-format=sha1\n"))
	buf.Write(pktFlush)
	s.writeRaw(w, 200, "application/x-git-upload-pack-advertisement", buf.Bytes())
}

func (s *srv) serveV0Advertisement(w http.ResponseWriter, repo *Repo, service string) {
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	var caps string
	if service == "git-upload-pack" {
		caps = "side-band-64k thin-pack ofs-delta shallow multi_ack_detailed agent=forge/1.0 symref=HEAD:refs/heads/main"
	} else {
		caps = "report-status side-band-64k delete-refs ofs-delta agent=forge/1.0 symref=HEAD:refs/heads/main"
	}
	var buf bytes.Buffer
	buf.Write(pktStr("# service=" + service + "\n"))
	buf.Write(pktFlush)
	mainTip := repo.refs["refs/heads/main"]
	names := sortedRefNames(repo.refs)
	if len(names) == 0 {
		buf.Write(pktStr(zeroOID + " capabilities^{}\x00" + caps + "\n"))
		buf.Write(pktFlush)
		s.writeRaw(w, 200, "application/x-"+service+"-advertisement", buf.Bytes())
		return
	}
	// HEAD line first, carrying the capabilities.
	headOID := mainTip
	if headOID == "" {
		headOID = zeroOID
	}
	buf.Write(pktStr(headOID + " HEAD\x00" + caps + "\n"))
	for _, name := range names {
		oid := repo.refs[name]
		buf.Write(pktStr(oid + " " + name + "\n"))
		if o := repo.objects[oid]; o != nil && o.typ == 4 {
			parseTagData(o)
			if peeled := peelToCommit(repo.objects, oid); peeled != "" {
				buf.Write(pktStr(peeled + " " + name + "^{}\n"))
			}
		}
	}
	buf.Write(pktFlush)
	s.writeRaw(w, 200, "application/x-"+service+"-advertisement", buf.Bytes())
}

// ---------------------------------------------------------------------------
// upload-pack
// ---------------------------------------------------------------------------

func (s *srv) handleUploadPack(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, err := readAll(r)
	if err != nil {
		s.writeRaw(w, 400, "application/json", errMsg("Bad Request"))
		return
	}
	if r.Header.Get("Git-Protocol") == "version=2" {
		s.handleUploadPackV2(w, repo, body)
	} else {
		s.handleUploadPackV0(w, repo, body)
	}
}

func (s *srv) fetchPackAndPlan(repo *Repo, req fetchRequest) ([]byte, fetchPlan) {
	key := fetchCacheKey(req, repo.version)
	repo.mu.RLock()
	pack, ok := repo.packCache[key]
	meta, metaOK := repo.packCache[key+"#shallow"]
	repo.mu.RUnlock()
	if ok && metaOK {
		parts := bytes.SplitN(meta, []byte{0}, 2)
		var shallow, unshallow []string
		if len(parts[0]) > 0 {
			shallow = strings.Split(string(parts[0]), ",")
		}
		if len(parts) > 1 && len(parts[1]) > 0 {
			unshallow = strings.Split(string(parts[1]), ",")
		}
		return pack, fetchPlan{shallow: shallow, unshallow: unshallow}
	}
	repo.mu.RLock()
	plan := computeFetch(repo.objects, req)
	pack = writePack(plan.objs)
	repo.mu.RUnlock()
	repo.mu.Lock()
	if len(repo.packCache) > 32 {
		repo.packCache = make(map[string][]byte)
	}
	repo.packCache[key] = pack
	repo.packCache[key+"#shallow"] = packShallowMeta(plan.shallow, plan.unshallow)
	repo.mu.Unlock()
	return pack, plan
}

func packShallowMeta(shallow, unshallow []string) []byte {
	var buf bytes.Buffer
	buf.WriteString(strings.Join(shallow, ","))
	buf.WriteByte(0)
	buf.WriteString(strings.Join(unshallow, ","))
	return buf.Bytes()
}

func fetchCacheKey(req fetchRequest, version uint64) string {
	var b strings.Builder
	b.WriteString(strconv.FormatUint(version, 10))
	b.WriteByte('|')
	b.WriteString(strings.Join(req.wants, ","))
	b.WriteByte('|')
	b.WriteString(strings.Join(req.haves, ","))
	b.WriteByte('|')
	b.WriteString(strings.Join(req.shallow, ","))
	b.WriteByte('|')
	b.WriteString(strconv.Itoa(req.depth))
	return b.String()
}

func sidebandPack(pack []byte) []byte {
	var buf bytes.Buffer
	const maxData = 65515
	for len(pack) > 0 {
		n := len(pack)
		if n > maxData {
			n = maxData
		}
		chunk := pack[:n]
		pack = pack[n:]
		payload := make([]byte, 1, len(chunk)+1)
		payload[0] = 1
		payload = append(payload, chunk...)
		buf.Write(pktLine(payload))
	}
	return buf.Bytes()
}

func (s *srv) handleUploadPackV2(w http.ResponseWriter, repo *Repo, body []byte) {
	lines, _ := parsePktLines(body)
	if lines == nil {
		s.writeRaw(w, 400, "application/json", errMsg("Bad Request"))
		return
	}
	var command string
	var args []string
	inArgs := false
	for _, l := range lines {
		t := strings.TrimSuffix(string(l), "\n")
		if t == "" {
			continue
		}
		if !inArgs {
			if strings.HasPrefix(t, "command=") {
				command = t[len("command="):]
				continue
			}
			if t == "delim" {
				inArgs = true
				continue
			}
			// delim arrives as its own packet; parsePktLines skips it, so the
			// first arg line flips us into args mode.
			if strings.HasPrefix(t, "agent=") || strings.HasPrefix(t, "object-format=") {
				continue
			}
			inArgs = true
		}
		args = append(args, t)
	}
	switch command {
	case "ls-refs":
		s.handleLsRefsV2(w, repo, args)
	case "fetch":
		s.handleFetchV2(w, repo, args)
	default:
		s.writeRaw(w, 400, "application/json", errMsg("Unknown command"))
	}
}

func (s *srv) handleLsRefsV2(w http.ResponseWriter, repo *Repo, args []string) {
	var prefixes []string
	peel, symrefs, unborn := false, false, false
	for _, a := range args {
		switch {
		case a == "peel":
			peel = true
		case a == "symrefs":
			symrefs = true
		case a == "unborn":
			unborn = true
		case strings.HasPrefix(a, "ref-prefix "):
			prefixes = append(prefixes, a[len("ref-prefix "):])
		}
	}
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	keep := func(name string) bool {
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
	var buf bytes.Buffer
	mainTip := repo.refs["refs/heads/main"]
	if keep("HEAD") {
		if mainTip != "" {
			line := mainTip + " HEAD"
			if symrefs {
				line += " symref-target:refs/heads/main"
			}
			buf.Write(pktStr(line + "\n"))
		} else if unborn {
			buf.Write(pktStr("unborn HEAD symref-target:refs/heads/main\n"))
		}
	}
	for _, name := range sortedRefNames(repo.refs) {
		if !keep(name) {
			continue
		}
		oid := repo.refs[name]
		line := oid + " " + name
		if peel {
			if o := repo.objects[oid]; o != nil && o.typ == 4 {
				if peeled := peelToCommit(repo.objects, oid); peeled != "" {
					line += " peeled:" + peeled
				}
			}
		}
		buf.Write(pktStr(line + "\n"))
	}
	buf.Write(pktFlush)
	s.writeRaw(w, 200, "application/x-git-upload-pack-result", buf.Bytes())
}

func (s *srv) handleFetchV2(w http.ResponseWriter, repo *Repo, args []string) {
	req := fetchRequest{depth: -1}
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "want "):
			req.wants = append(req.wants, a[5:])
		case strings.HasPrefix(a, "have "):
			req.haves = append(req.haves, a[5:])
		case strings.HasPrefix(a, "shallow "):
			req.shallow = append(req.shallow, a[8:])
		case strings.HasPrefix(a, "deepen "):
			if n, err := strconv.Atoi(a[7:]); err == nil {
				req.depth = n
			}
		}
	}
	if len(req.wants) == 0 {
		var buf bytes.Buffer
		buf.Write(pktStr("acknowledgments\n"))
		buf.Write(pktStr("NAK\n"))
		buf.Write(pktDelim)
		buf.Write(pktStr("packfile\n"))
		buf.Write(sidebandPack(writePack(nil)))
		buf.Write(pktFlush)
		s.writeRaw(w, 200, "application/x-git-upload-pack-result", buf.Bytes())
		return
	}
	pack, plan := s.fetchPackAndPlan(repo, req)
	var buf bytes.Buffer
	buf.Write(pktStr("acknowledgments\n"))
	buf.Write(pktStr("NAK\n"))
	buf.Write(pktDelim)
	if len(plan.shallow) > 0 || len(plan.unshallow) > 0 {
		buf.Write(pktStr("shallow-info\n"))
		for _, sh := range plan.shallow {
			buf.Write(pktStr("shallow " + sh + "\n"))
		}
		for _, un := range plan.unshallow {
			buf.Write(pktStr("unshallow " + un + "\n"))
		}
		buf.Write(pktDelim)
	}
	buf.Write(pktStr("packfile\n"))
	buf.Write(sidebandPack(pack))
	buf.Write(pktFlush)
	s.writeRaw(w, 200, "application/x-git-upload-pack-result", buf.Bytes())
}

func (s *srv) handleUploadPackV0(w http.ResponseWriter, repo *Repo, body []byte) {
	lines, _ := parsePktLines(body)
	if lines == nil {
		s.writeRaw(w, 400, "application/json", errMsg("Bad Request"))
		return
	}
	// v0 fetch: wants, then (after a flush the client sent) haves/done.
	// parsePktLines drops flush markers, so the lines arrive in order.
	req := fetchRequest{depth: -1}
	for _, l := range lines {
		t := strings.TrimSuffix(string(l), "\n")
		switch {
		case strings.HasPrefix(t, "want "):
			rest := t[5:]
			fields := strings.Fields(rest)
			if len(fields) > 0 {
				req.wants = append(req.wants, fields[0])
			}
		case strings.HasPrefix(t, "have "):
			req.haves = append(req.haves, strings.TrimSpace(t[5:]))
		case strings.HasPrefix(t, "shallow "):
			req.shallow = append(req.shallow, strings.TrimSpace(t[8:]))
		case strings.HasPrefix(t, "deepen "):
			if n, err := strconv.Atoi(strings.TrimSpace(t[7:])); err == nil {
				req.depth = n
			}
		}
	}
	if len(req.wants) == 0 {
		s.writeRaw(w, 400, "application/json", errMsg("Bad Request"))
		return
	}
	pack, plan := s.fetchPackAndPlan(repo, req)
	var buf bytes.Buffer
	buf.Write(pktStr("NAK\n"))
	for _, sh := range plan.shallow {
		buf.Write(pktStr("shallow " + sh + "\n"))
	}
	for _, un := range plan.unshallow {
		buf.Write(pktStr("unshallow " + un + "\n"))
	}
	buf.Write(sidebandPack(pack))
	buf.Write(pktFlush)
	s.writeRaw(w, 200, "application/x-git-upload-pack-result", buf.Bytes())
}

// ---------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------

type pushCommand struct {
	oldOID, newOID, ref string
}

func (s *srv) handleReceivePack(w http.ResponseWriter, r *http.Request, repo *Repo) {
	body, err := readAll(r)
	if err != nil {
		s.writeRaw(w, 400, "application/json", errMsg("Bad Request"))
		return
	}
	lines, restOff := parsePktLines(body)
	if lines == nil {
		s.writeRaw(w, 400, "application/json", errMsg("Bad Request"))
		return
	}
	var cmds []pushCommand
	for i, l := range lines {
		t := strings.TrimSuffix(string(l), "\n")
		if i == 0 {
			if idx := strings.Index(t, "\x00"); idx >= 0 {
				t = t[:idx]
			}
		}
		fields := strings.Fields(t)
		if len(fields) != 3 {
			continue
		}
		cmds = append(cmds, pushCommand{oldOID: fields[0], newOID: fields[1], ref: fields[2]})
	}
	if len(cmds) == 0 {
		s.writeRaw(w, 400, "application/json", errMsg("Bad Request"))
		return
	}
	var pack []byte
	if restOff >= 0 && restOff < len(body) {
		pack = body[restOff:]
	}
	unpackOK := true
	unpackDetail := "ok"
	repo.mu.Lock()
	defer repo.mu.Unlock()
	if len(pack) > 0 {
		objs, err := parsePack(pack, func(oid string) *Obj { return repo.objects[oid] })
		if err != nil {
			unpackOK = false
			unpackDetail = err.Error()
		} else {
			for oid, o := range objs {
				if _, ok := repo.objects[oid]; !ok {
					repo.objects[oid] = o
				}
			}
		}
	}
	var report strings.Builder
	if unpackOK {
		report.WriteString("unpack ok\n")
	} else {
		report.WriteString("unpack " + unpackDetail + "\n")
	}
	for _, c := range cmds {
		ok, reason := s.applyPushLocked(repo, c, unpackOK)
		if ok {
			report.WriteString("ok " + c.ref + "\n")
		} else {
			report.WriteString("ng " + c.ref + " " + reason + "\n")
		}
	}
	// Report-status inside sideband band 1: each report pkt-line wrapped.
	var buf bytes.Buffer
	for _, line := range strings.Split(strings.TrimSuffix(report.String(), "\n"), "\n") {
		payload := append([]byte{1}, pktStr(line+"\n")...)
		buf.Write(pktLine(payload))
	}
	buf.Write(pktFlush)
	s.writeRaw(w, 200, "application/x-git-receive-pack-result", buf.Bytes())
}

// applyPushLocked validates and applies one ref update. Caller holds repo.mu.
func (s *srv) applyPushLocked(repo *Repo, c pushCommand, unpackOK bool) (bool, string) {
	if !unpackOK {
		return false, "unpack failed"
	}
	cur, exists := repo.refs[c.ref]
	if c.newOID == zeroOID {
		// delete
		if !exists || cur != c.oldOID {
			return false, "non-fast-forward"
		}
		delete(repo.refs, c.ref)
		repo.version++
		repo.packCache = make(map[string][]byte)
		return true, ""
	}
	newObj := repo.objects[c.newOID]
	if newObj == nil {
		return false, "non-fast-forward"
	}
	if c.oldOID == zeroOID {
		if exists {
			return false, "already exists"
		}
		repo.refs[c.ref] = c.newOID
		repo.version++
		repo.packCache = make(map[string][]byte)
		return true, ""
	}
	if !exists || cur != c.oldOID {
		return false, "non-fast-forward"
	}
	if !isDescendant(repo.objects, c.oldOID, c.newOID) {
		return false, "non-fast-forward"
	}
	repo.refs[c.ref] = c.newOID
	repo.version++
	repo.packCache = make(map[string][]byte)
	return true, ""
}
