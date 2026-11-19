package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// REST v3 handlers. segs are the path segments after /api/v3.
// ---------------------------------------------------------------------------

type repoView struct {
	ID            int64    `json:"id"`
	NodeID        string   `json:"node_id"`
	Name          string   `json:"name"`
	FullName      string   `json:"full_name"`
	Private       bool     `json:"private"`
	Description   string   `json:"description"`
	DefaultBranch string   `json:"default_branch"`
	Owner         userView `json:"owner"`
	HTMLURL       string   `json:"html_url"`
	URL           string   `json:"url"`
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
	HasIssues     bool     `json:"has_issues"`
	OpenIssues    int      `json:"open_issues_count"`
}

func (s *srv) repoView(r *Repo) repoView {
	r.mu.RLock()
	defer r.mu.RUnlock()
	open := 0
	for _, iss := range r.issues {
		if iss.State == "open" {
			open++
		}
	}
	for _, p := range r.pulls {
		if p.State == "open" {
			open++
		}
	}
	return repoView{
		ID: r.id, NodeID: repoGlobalID(r.owner, r.name), Name: r.name,
		FullName: r.owner + "/" + r.name, Private: r.priv, Description: r.desc,
		DefaultBranch: "main", Owner: s.store.viewerUser(),
		HTMLURL:   "https://example.invalid/" + r.owner + "/" + r.name,
		URL:       "https://example.invalid/api/v3/repos/" + r.owner + "/" + r.name,
		CreatedAt: r.createdAt, UpdatedAt: r.createdAt, HasIssues: true, OpenIssues: open,
	}
}

func (s *srv) parseBody(r *http.Request) (map[string]any, []byte, bool) {
	if r.Body == nil {
		return map[string]any{}, nil, true
	}
	raw, err := readAll(r)
	if err != nil || len(raw) == 0 {
		return map[string]any{}, raw, true
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, raw, false
	}
	return m, raw, true
}

func errMsg(msg string) []byte { return mustJSON(map[string]string{"message": msg}) }

// handleREST returns true when the route was handled (including errors).
func (s *srv) handleREST(w http.ResponseWriter, r *http.Request, segs []string) bool {
	if len(segs) == 0 {
		if r.Method == "GET" {
			w.Header().Set("X-OAuth-Scopes", "repo, user")
			s.writeRaw(w, 200, "application/json", []byte("{}"))
			return true
		}
		return false
	}
	switch segs[0] {
	case "meta":
		if r.Method == "GET" {
			s.writeRaw(w, 200, "application/json", []byte(`{"installed_version":"1.0.0"}`))
			return true
		}
	case "rate_limit":
		if r.Method == "GET" {
			s.writeRaw(w, 200, "application/json", []byte(`{"rate":{"limit":5000,"remaining":5000,"reset":1893456000},"resources":{"core":{"limit":5000,"remaining":5000,"reset":1893456000}}}`))
			return true
		}
	case "user":
		return s.handleUser(w, r, segs[1:])
	case "repos":
		return s.handleRepo(w, r, segs[1:])
	}
	return false
}

func (s *srv) handleUser(w http.ResponseWriter, r *http.Request, rest []string) bool {
	if len(rest) == 0 {
		if r.Method == "GET" {
			s.writeJSON(w, 200, struct {
				Login string `json:"login"`
				ID    int64  `json:"id"`
				Type  string `json:"type"`
				Name  string `json:"name"`
				Email string `json:"email"`
			}{Login: s.store.viewer, ID: 1000, Type: "User", Name: s.store.viewer, Email: s.store.viewer + "@example.invalid"})
			return true
		}
		return false
	}
	if rest[0] == "repos" && len(rest) == 1 {
		switch r.Method {
		case "GET":
			return s.listUserRepos(w, r)
		case "POST":
			body, _, ok := s.parseBody(r)
			if !ok {
				s.writeRaw(w, 400, "application/json", errMsg("Bad Request: malformed JSON"))
				return true
			}
			name := jsonStr(body, "name")
			if name == "" {
				s.writeRaw(w, 422, "application/json", errMsg("name is required"))
				return true
			}
			repo, created := s.store.createRepo(name, jsonStr(body, "description"), jsonBool(body, "private", false))
			if !created {
				s.writeRaw(w, 422, "application/json", errMsg("Repository already exists"))
				return true
			}
			s.writeJSON(w, 201, s.repoView(repo))
			return true
		}
	}
	return false
}

func (s *srv) listUserRepos(w http.ResponseWriter, r *http.Request) bool {
	q := r.URL.Query()
	perPage := atoiDef(q.Get("per_page"), 30)
	page := atoiDef(q.Get("page"), 1)
	if perPage < 1 {
		perPage = 30
	}
	if page < 1 {
		page = 1
	}
	all := s.store.listRepos()
	total := len(all)
	start := (page - 1) * perPage
	if start > total {
		start = total
	}
	end := start + perPage
	if end > total {
		end = total
	}
	views := make([]repoView, 0, end-start)
	for _, repo := range all[start:end] {
		views = append(views, s.repoView(repo))
	}
	if end < total {
		next := "http://" + r.Host + "/api/v3/user/repos?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page+1)
		w.Header().Set("Link", "<"+next+">; rel=\"next\"")
	}
	s.writeJSON(w, 200, views)
	return true
}

func atoiDef(v string, def int) int {
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

// handleRepo: segs are [owner, repo, ...rest].
func (s *srv) handleRepo(w http.ResponseWriter, r *http.Request, segs []string) bool {
	if len(segs) < 2 {
		return false
	}
	owner, name := segs[0], segs[1]
	if owner != s.store.viewer {
		s.writeRaw(w, 404, "application/json", errMsg("Not Found"))
		return true
	}
	repo := s.store.getRepo(owner, name)
	if repo == nil {
		s.writeRaw(w, 404, "application/json", errMsg("Not Found"))
		return true
	}
	rest := segs[2:]
	if len(rest) == 0 {
		switch r.Method {
		case "GET":
			s.writeJSON(w, 200, s.repoView(repo))
			return true
		case "PATCH":
			body, _, ok := s.parseBody(r)
			if !ok {
				s.writeRaw(w, 400, "application/json", errMsg("Bad Request: malformed JSON"))
				return true
			}
			repo.mu.Lock()
			if d, ok2 := body["description"]; ok2 {
				if ds, ok3 := d.(string); ok3 {
					repo.desc = ds
				}
			}
			if p, ok2 := body["private"]; ok2 {
				if pb, ok3 := p.(bool); ok3 {
					repo.priv = pb
				}
			}
			repo.mu.Unlock()
			s.writeJSON(w, 200, s.repoView(repo))
			return true
		case "DELETE":
			s.store.deleteRepo(owner, name)
			w.WriteHeader(204)
			return true
		}
		return false
	}
	switch rest[0] {
	case "branches":
		return s.handleBranches(w, r, repo, rest[1:])
	case "commits":
		if r.Method == "GET" && len(rest) == 1 {
			return s.handleCommits(w, r, repo)
		}
	case "git":
		return s.handleGitRefs(w, r, repo, rest[1:])
	case "contents":
		return s.handleContents(w, r, repo, rest[1:])
	case "labels":
		return s.handleLabels(w, r, repo, rest[1:])
	case "issues":
		return s.handleIssues(w, r, repo, rest[1:])
	case "pulls":
		return s.handlePulls(w, r, repo, rest[1:])
	}
	return false
}

func (s *srv) handleBranches(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) bool {
	if r.Method != "GET" {
		return false
	}
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	if len(rest) == 0 {
		type branchView struct {
			Name      string `json:"name"`
			Commit    any    `json:"commit"`
			Protected bool   `json:"protected"`
		}
		var out []branchView
		for ref, sha := range repo.refs {
			if strings.HasPrefix(ref, "refs/heads/") {
				out = append(out, branchView{Name: ref[len("refs/heads/"):], Commit: map[string]string{"sha": sha}, Protected: false})
			}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		if out == nil {
			out = []branchView{}
		}
		s.writeJSON(w, 200, out)
		return true
	}
	if len(rest) == 1 {
		sha, ok := repo.refs["refs/heads/"+rest[0]]
		if !ok {
			s.writeRaw(w, 404, "application/json", errMsg("Not Found"))
			return true
		}
		s.writeJSON(w, 200, map[string]any{"name": rest[0], "commit": map[string]string{"sha": sha}, "protected": false})
		return true
	}
	return false
}

func (s *srv) handleCommits(w http.ResponseWriter, r *http.Request, repo *Repo) bool {
	sha := r.URL.Query().Get("sha")
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	var tip string
	if sha == "" {
		tip = repo.refs["refs/heads/main"]
	} else if strings.HasPrefix(sha, "refs/heads/") {
		tip = repo.refs[sha]
	} else if o := repo.objects[sha]; o != nil && o.typ == 1 {
		tip = sha
	} else {
		tip = repo.refs["refs/heads/"+sha]
	}
	type commitView struct {
		Sha     string `json:"sha"`
		Commit  any    `json:"commit"`
		Parents []any  `json:"parents"`
	}
	var out []commitView
	seen := map[string]bool{}
	cur := tip
	for cur != "" && !seen[cur] {
		seen[cur] = true
		o := repo.objects[cur]
		if o == nil || o.typ != 1 {
			break
		}
		parseCommitData(o)
		msg := ""
		if idx := strings.Index(string(o.data), "\n\n"); idx >= 0 {
			msg = strings.TrimRight(string(o.data[idx+2:]), "\n")
		}
		var parents []any
		for _, p := range o.parents {
			parents = append(parents, map[string]string{"sha": p})
		}
		out = append(out, commitView{
			Sha: cur,
			Commit: map[string]any{"message": msg,
				"author":    map[string]string{"name": "Forge", "email": "forge@make.fun"},
				"committer": map[string]string{"name": "Forge", "email": "forge@make.fun"}},
			Parents: parents,
		})
		if len(o.parents) == 0 {
			break
		}
		cur = o.parents[0]
	}
	if out == nil {
		out = []commitView{}
	}
	s.writeJSON(w, 200, out)
	return true
}

func (s *srv) handleGitRefs(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) bool {
	// rest: ["refs"] POST create, or ["refs", "heads", branch...] GET.
	if len(rest) >= 1 && rest[0] == "refs" {
		if r.Method == "POST" && len(rest) == 1 {
			body, _, ok := s.parseBody(r)
			if !ok {
				s.writeRaw(w, 400, "application/json", errMsg("Bad Request: malformed JSON"))
				return true
			}
			ref := jsonStr(body, "ref")
			sha := jsonStr(body, "sha")
			if ref == "" || sha == "" || !strings.HasPrefix(ref, "refs/") {
				s.writeRaw(w, 422, "application/json", errMsg("Invalid ref"))
				return true
			}
			repo.mu.Lock()
			defer repo.mu.Unlock()
			if _, exists := repo.refs[ref]; exists {
				s.writeRaw(w, 422, "application/json", errMsg("Reference already exists"))
				return true
			}
			if o := repo.objects[sha]; o == nil {
				s.writeRaw(w, 422, "application/json", errMsg("Object does not exist"))
				return true
			}
			repo.refs[ref] = sha
			repo.version++
			repo.packCache = make(map[string][]byte)
			s.writeJSON(w, 201, map[string]any{"ref": ref, "object": map[string]string{"sha": sha, "type": "commit"}})
			return true
		}
		if r.Method == "GET" && len(rest) >= 2 {
			ref := "refs/" + strings.Join(rest[1:], "/")
			repo.mu.RLock()
			sha, ok := repo.refs[ref]
			repo.mu.RUnlock()
			if !ok {
				s.writeRaw(w, 404, "application/json", errMsg("Not Found"))
				return true
			}
			s.writeJSON(w, 200, map[string]any{"ref": ref, "object": map[string]string{"sha": sha, "type": "commit"}})
			return true
		}
	}
	return false
}

// resolveRefTipLocked: branch name or oid -> commit oid. Caller holds r.mu read.
func resolveRefTipLocked(repo *Repo, ref string) string {
	if ref == "" {
		return repo.refs["refs/heads/main"]
	}
	if strings.HasPrefix(ref, "refs/heads/") {
		return repo.refs[ref]
	}
	if o := repo.objects[ref]; o != nil && o.typ == 1 {
		return ref
	}
	if tip := repo.refs["refs/heads/"+ref]; tip != "" {
		return tip
	}
	return ""
}

func (s *srv) handleContents(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) bool {
	if len(rest) == 0 {
		return false
	}
	path := strings.Join(rest, "/")
	refQ := r.URL.Query().Get("ref")
	switch r.Method {
	case "GET":
		repo.mu.RLock()
		tip := resolveRefTipLocked(repo, refQ)
		if tip == "" {
			repo.mu.RUnlock()
			s.writeRaw(w, 404, "application/json", errMsg("Not Found"))
			return true
		}
		flat := flattenTree(repo.objects, commitTree(repo.objects, tip))
		// exact file?
		if e, ok := flat[path]; ok {
			o := repo.objects[e.oid]
			var data []byte
			if o != nil {
				data = o.data
			}
			enc := base64.StdEncoding.EncodeToString(data)
			repo.mu.RUnlock()
			s.writeJSON(w, 200, map[string]any{
				"type": "file", "encoding": "base64", "size": len(data),
				"name": lastSegment(path), "path": path, "sha": e.oid, "content": enc,
			})
			return true
		}
		// directory?
		prefix := path + "/"
		seen := map[string]map[string]any{}
		for p, e := range flat {
			if !strings.HasPrefix(p, prefix) {
				continue
			}
			rest2 := p[len(prefix):]
			seg := strings.SplitN(rest2, "/", 2)[0]
			if _, ok := seen[seg]; ok {
				continue
			}
			if strings.Contains(rest2, "/") {
				// sub-directory: find its tree oid via the path walk
				seen[seg] = map[string]any{"type": "dir", "name": seg, "path": path + "/" + seg, "sha": dirTreeOID(repo, tip, path+"/"+seg), "size": 0}
			} else {
				seen[seg] = map[string]any{"type": "file", "name": seg, "path": p, "sha": e.oid, "size": blobSize(repo, e.oid)}
			}
		}
		if len(seen) == 0 {
			// also allow listing the repo root when path is "" (not reachable here)
			repo.mu.RUnlock()
			s.writeRaw(w, 404, "application/json", errMsg("Not Found"))
			return true
		}
		var out []map[string]any
		for _, v := range seen {
			out = append(out, v)
		}
		sort.Slice(out, func(i, j int) bool { return out[i]["name"].(string) < out[j]["name"].(string) })
		repo.mu.RUnlock()
		s.writeJSON(w, 200, out)
		return true
	case "PUT", "DELETE":
		body, _, ok := s.parseBody(r)
		if !ok {
			s.writeRaw(w, 400, "application/json", errMsg("Bad Request: malformed JSON"))
			return true
		}
		branch := jsonStr(body, "branch")
		if branch == "" {
			branch = "main"
		}
		msg := jsonStr(body, "message")
		if msg == "" {
			msg = "update"
		}
		repo.mu.Lock()
		defer repo.mu.Unlock()
		refName := "refs/heads/" + branch
		tip := repo.refs[refName]
		var flat map[string]flatEntry
		if tip != "" {
			flat = flattenTree(repo.objects, commitTree(repo.objects, tip))
		} else {
			flat = map[string]flatEntry{}
		}
		if r.Method == "DELETE" {
			e, ok := flat[path]
			if !ok {
				s.writeRaw(w, 404, "application/json", errMsg("Not Found"))
				return true
			}
			if sha := jsonStr(body, "sha"); sha != "" && sha != e.oid {
				s.writeRaw(w, 409, "application/json", errMsg("sha does not match"))
				return true
			}
			delete(flat, path)
			tree := buildTreeFromFlat(repo.objects, flat)
			var parents []string
			if tip != "" {
				parents = []string{tip}
			}
			commit := repo.createCommitLocked(tree, parents, msg+"\n")
			repo.refs[refName] = commit
			repo.version++
			repo.packCache = make(map[string][]byte)
			s.writeJSON(w, 200, map[string]any{"commit": map[string]string{"sha": commit}})
			return true
		}
		// PUT
		contentB64 := jsonStr(body, "content")
		raw := []byte(strings.NewReplacer("\n", "", "\r", "", "\t", "", " ", "").Replace(contentB64))
		data, err := base64.StdEncoding.DecodeString(string(raw))
		if err != nil {
			s.writeRaw(w, 422, "application/json", errMsg("content is not valid base64"))
			return true
		}
		blobOID := hashObjectBytes("blob", data)
		if _, ok := repo.objects[blobOID]; !ok {
			repo.objects[blobOID] = &Obj{typ: 3, data: data}
		}
		sha := jsonStr(body, "sha")
		existing, exists := flat[path]
		if exists {
			if sha == "" || sha != existing.oid {
				s.writeRaw(w, 409, "application/json", errMsg("sha does not match"))
				return true
			}
		} else if sha != "" {
			s.writeRaw(w, 409, "application/json", errMsg("sha does not match"))
			return true
		}
		flat[path] = flatEntry{mode: "100644", oid: blobOID}
		tree := buildTreeFromFlat(repo.objects, flat)
		var parents []string
		if tip != "" {
			parents = []string{tip}
		}
		commit := repo.createCommitLocked(tree, parents, msg+"\n")
		repo.refs[refName] = commit
		repo.version++
		repo.packCache = make(map[string][]byte)
		status := 201
		if exists {
			status = 200
		}
		s.writeJSON(w, status, map[string]any{
			"content": map[string]any{"sha": blobOID, "encoding": "base64", "name": lastSegment(path), "path": path, "size": len(data)},
			"commit":  map[string]string{"sha": commit},
		})
		return true
	}
	return false
}

func dirTreeOID(repo *Repo, tip, dirPath string) string {
	flat := flattenTree(repo.objects, commitTree(repo.objects, tip))
	// Walk the tree objects directly.
	tree := commitTree(repo.objects, tip)
	parts := strings.Split(dirPath, "/")
	cur := tree
	for _, seg := range parts {
		o := repo.objects[cur]
		if o == nil {
			return zeroOID
		}
		found := ""
		for _, e := range parseTree(o.data) {
			if e.name == seg {
				found = e.oid
				break
			}
		}
		if found == "" {
			return zeroOID
		}
		cur = found
	}
	_ = flat
	return cur
}

func blobSize(repo *Repo, oid string) int {
	if o := repo.objects[oid]; o != nil {
		return len(o.data)
	}
	return 0
}

func lastSegment(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

func (s *srv) handleLabels(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) bool {
	if len(rest) != 0 {
		return false
	}
	switch r.Method {
	case "GET":
		repo.mu.RLock()
		var out []labelView
		for _, l := range repo.labels {
			out = append(out, labelView{ID: l.ID, Name: l.Name, Color: l.Color, Desc: l.Desc})
		}
		repo.mu.RUnlock()
		if out == nil {
			out = []labelView{}
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		s.writeJSON(w, 200, out)
		return true
	case "POST":
		body, _, ok := s.parseBody(r)
		if !ok {
			s.writeRaw(w, 400, "application/json", errMsg("Bad Request: malformed JSON"))
			return true
		}
		name := jsonStr(body, "name")
		if name == "" {
			s.writeRaw(w, 422, "application/json", errMsg("name is required"))
			return true
		}
		color := jsonStr(body, "color")
		if color == "" {
			color = "ededed"
		}
		repo.mu.Lock()
		if l, exists := repo.labels[name]; exists {
			repo.mu.Unlock()
			s.writeJSON(w, 201, labelView{ID: l.ID, Name: l.Name, Color: l.Color, Desc: l.Desc})
			return true
		}
		repo.nextLabel++
		l := &Label{ID: repo.nextLabel, Name: name, Color: color, Desc: jsonStr(body, "description")}
		repo.labels[name] = l
		repo.mu.Unlock()
		s.writeJSON(w, 201, labelView{ID: l.ID, Name: l.Name, Color: l.Color, Desc: l.Desc})
		return true
	}
	return false
}

func (s *srv) handleIssues(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) bool {
	if len(rest) == 0 {
		switch r.Method {
		case "GET":
			q := r.URL.Query()
			state := q.Get("state")
			if state == "" {
				state = "open"
			}
			sortParam := q.Get("sort")
			dir := q.Get("direction")
			if dir == "" {
				if sortParam == "created" {
					dir = "asc"
				} else {
					dir = "desc"
				}
			}
			repo.mu.RLock()
			var list []*Issue
			for _, iss := range repo.issues {
				if state == "all" || iss.State == state {
					list = append(list, iss)
				}
			}
			repo.mu.RUnlock()
			if dir == "desc" {
				for i, j := 0, len(list)-1; i < j; i, j = i+1, j-1 {
					list[i], list[j] = list[j], list[i]
				}
			}
			views := make([]json.RawMessage, 0, len(list))
			for _, iss := range list {
				views = append(views, repo.issueJSON(s.store, iss))
			}
			s.writeJSON(w, 200, views)
			return true
		case "POST":
			body, _, ok := s.parseBody(r)
			if !ok {
				s.writeRaw(w, 400, "application/json", errMsg("Bad Request: malformed JSON"))
				return true
			}
			var labels []string
			if lv, ok2 := body["labels"]; ok2 {
				if arr, ok3 := lv.([]any); ok3 {
					for _, item := range arr {
						switch v := item.(type) {
						case string:
							labels = append(labels, v)
						case map[string]any:
							labels = append(labels, jsonStr(v, "name"))
						}
					}
				}
			}
			repo.mu.Lock()
			for _, name := range labels {
				if _, exists := repo.labels[name]; !exists {
					repo.mu.Unlock()
					s.writeRaw(w, 422, "application/json", errMsg("Label does not exist"))
					return true
				}
			}
			iss := repo.createIssueLocked(s.store, jsonStr(body, "title"), jsonStr(body, "body"), labels)
			data := repo.issueJSON(s.store, iss)
			repo.mu.Unlock()
			s.writeRaw(w, 201, "application/json", data)
			return true
		}
		return false
	}
	num, err := strconv.Atoi(rest[0])
	if err != nil || num < 1 {
		return false
	}
	if len(rest) == 2 && rest[1] == "comments" {
		switch r.Method {
		case "GET":
			repo.mu.RLock()
			var iss *Issue
			if num <= len(repo.issues) {
				iss = repo.issues[num-1]
			}
			repo.mu.RUnlock()
			if iss == nil {
				s.writeRaw(w, 404, "application/json", errMsg("Not Found"))
				return true
			}
			type commentView struct {
				ID        int64    `json:"id"`
				NodeID    string   `json:"node_id"`
				Body      string   `json:"body"`
				User      userView `json:"user"`
				CreatedAt string   `json:"created_at"`
				UpdatedAt string   `json:"updated_at"`
			}
			var out []commentView
			for _, c := range iss.Comments {
				out = append(out, commentView{ID: c.ID, NodeID: c.NodeID, Body: c.Body, User: s.store.viewerUser(), CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt})
			}
			if out == nil {
				out = []commentView{}
			}
			s.writeJSON(w, 200, out)
			return true
		case "POST":
			body, _, ok := s.parseBody(r)
			if !ok {
				s.writeRaw(w, 400, "application/json", errMsg("Bad Request: malformed JSON"))
				return true
			}
			repo.mu.Lock()
			var iss *Issue
			if num <= len(repo.issues) {
				iss = repo.issues[num-1]
			}
			if iss == nil {
				repo.mu.Unlock()
				s.writeRaw(w, 404, "application/json", errMsg("Not Found"))
				return true
			}
			repo.nextCmt++
			now := nowISO()
			c := &Comment{ID: repo.nextCmt, NodeID: commentGlobalID(repo.owner, repo.name, num, repo.nextCmt), Body: jsonStr(body, "body"), User: s.store.viewer, CreatedAt: now, UpdatedAt: now}
			iss.Comments = append(iss.Comments, c)
			repo.mu.Unlock()
			s.writeJSON(w, 201, map[string]any{"id": c.ID, "node_id": c.NodeID, "body": c.Body, "user": s.store.viewerUser(), "created_at": c.CreatedAt, "updated_at": c.UpdatedAt})
			return true
		}
		return false
	}
	if len(rest) == 1 {
		switch r.Method {
		case "GET":
			repo.mu.RLock()
			var iss *Issue
			if num <= len(repo.issues) {
				iss = repo.issues[num-1]
			}
			var data []byte
			if iss != nil {
				data = repo.issueJSON(s.store, iss)
			}
			repo.mu.RUnlock()
			if iss == nil {
				s.writeRaw(w, 404, "application/json", errMsg("Not Found"))
				return true
			}
			s.writeRaw(w, 200, "application/json", data)
			return true
		case "PATCH":
			body, _, ok := s.parseBody(r)
			if !ok {
				s.writeRaw(w, 400, "application/json", errMsg("Bad Request: malformed JSON"))
				return true
			}
			repo.mu.Lock()
			var iss *Issue
			if num <= len(repo.issues) {
				iss = repo.issues[num-1]
			}
			if iss == nil {
				repo.mu.Unlock()
				s.writeRaw(w, 404, "application/json", errMsg("Not Found"))
				return true
			}
			if t, ok2 := body["title"]; ok2 {
				if ts, ok3 := t.(string); ok3 {
					iss.Title = ts
				}
			}
			if b, ok2 := body["body"]; ok2 {
				if bs, ok3 := b.(string); ok3 {
					iss.Body = bs
				}
			}
			if st, ok2 := body["state"]; ok2 {
				if ss, ok3 := st.(string); ok3 && (ss == "open" || ss == "closed") {
					iss.State = ss
					if ss == "closed" {
						now := nowISO()
						iss.ClosedAt = &now
					} else {
						iss.ClosedAt = nil
					}
				}
			}
			iss.UpdatedAt = nowISO()
			iss.jsonCache = nil
			data := repo.issueJSON(s.store, iss)
			repo.mu.Unlock()
			s.writeRaw(w, 200, "application/json", data)
			return true
		}
	}
	return false
}

func (s *srv) handlePulls(w http.ResponseWriter, r *http.Request, repo *Repo, rest []string) bool {
	if len(rest) == 0 {
		switch r.Method {
		case "GET":
			q := r.URL.Query()
			state := q.Get("state")
			if state == "" {
				state = "open"
			}
			repo.mu.RLock()
			var views []json.RawMessage
			for _, p := range repo.pulls {
				if state == "all" || p.State == state {
					views = append(views, repo.prJSON(s.store, p))
				}
			}
			repo.mu.RUnlock()
			if views == nil {
				views = []json.RawMessage{}
			}
			s.writeJSON(w, 200, views)
			return true
		case "POST":
			body, _, ok := s.parseBody(r)
			if !ok {
				s.writeRaw(w, 400, "application/json", errMsg("Bad Request: malformed JSON"))
				return true
			}
			head := jsonStr(body, "head")
			base := jsonStr(body, "base")
			if base == "" {
				base = "main"
			}
			repo.mu.Lock()
			if _, ok := repo.refs["refs/heads/"+head]; !ok {
				repo.mu.Unlock()
				s.writeRaw(w, 422, "application/json", errMsg("Head branch does not exist"))
				return true
			}
			if _, ok := repo.refs["refs/heads/"+base]; !ok {
				repo.mu.Unlock()
				s.writeRaw(w, 422, "application/json", errMsg("Base branch does not exist"))
				return true
			}
			p := repo.createPRLocked(s.store, jsonStr(body, "title"), jsonStr(body, "body"), head, base)
			if p == nil {
				repo.mu.Unlock()
				s.writeRaw(w, 422, "application/json", errMsg("Could not create pull request"))
				return true
			}
			data := repo.prJSON(s.store, p)
			repo.mu.Unlock()
			s.writeRaw(w, 201, "application/json", data)
			return true
		}
		return false
	}
	num, err := strconv.Atoi(rest[0])
	if err != nil || num < 1 {
		return false
	}
	if len(rest) == 2 && rest[1] == "files" && r.Method == "GET" {
		repo.mu.RLock()
		var p *PR
		if num <= len(repo.pulls) {
			p = repo.pulls[num-1]
		}
		repo.mu.RUnlock()
		if p == nil {
			s.writeRaw(w, 404, "application/json", errMsg("Not Found"))
			return true
		}
		files := p.Files
		if files == nil {
			files = []pullFile{}
		}
		s.writeJSON(w, 200, files)
		return true
	}
	if len(rest) == 2 && rest[1] == "merge" && r.Method == "PUT" {
		_, _, ok := s.parseBody(r)
		if !ok {
			s.writeRaw(w, 400, "application/json", errMsg("Bad Request: malformed JSON"))
			return true
		}
		repo.mu.Lock()
		var p *PR
		if num <= len(repo.pulls) {
			p = repo.pulls[num-1]
		}
		if p == nil {
			repo.mu.Unlock()
			s.writeRaw(w, 404, "application/json", errMsg("Not Found"))
			return true
		}
		if p.State != "open" {
			repo.mu.Unlock()
			s.writeRaw(w, 405, "application/json", errMsg("Pull request is already closed"))
			return true
		}
		sha, merged, conflict := repo.mergePRLocked(p)
		repo.mu.Unlock()
		if conflict {
			s.writeRaw(w, 405, "application/json", errMsg("Merge conflict"))
			return true
		}
		if !merged {
			s.writeRaw(w, 405, "application/json", errMsg("Merge failed"))
			return true
		}
		s.writeJSON(w, 200, map[string]any{"sha": sha, "merged": true, "message": "Pull request successfully merged"})
		return true
	}
	if len(rest) == 1 && r.Method == "GET" {
		repo.mu.RLock()
		var p *PR
		if num <= len(repo.pulls) {
			p = repo.pulls[num-1]
		}
		var data []byte
		if p != nil {
			data = repo.prJSON(s.store, p)
		}
		repo.mu.RUnlock()
		if p == nil {
			s.writeRaw(w, 404, "application/json", errMsg("Not Found"))
			return true
		}
		s.writeRaw(w, 200, "application/json", data)
		return true
	}
	return false
}
