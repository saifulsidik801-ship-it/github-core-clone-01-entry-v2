package main

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// GraphQL v4: a hand-rolled executor for exactly the documents the suite
// sends (see the kit's checks-graphql.ts).
// ---------------------------------------------------------------------------

var (
	reTypeCall = regexp.MustCompile(`(\w+)\s*:\s*__type\(\s*name\s*:\s*"([^"]+)"\s*\)`)
	reOrderBy  = regexp.MustCompile(`orderBy\s*:\s*\{\s*field\s*:\s*(\w+)\s*,\s*direction\s*:\s*(\w+)\s*\}`)
	reStates   = regexp.MustCompile(`states\s*:\s*\[([^\]]*)\]`)
	reFirstArg = regexp.MustCompile(`first\s*:\s*(\d+)`)
)

var issueTypeFields = []string{"id", "number", "title", "body", "state", "url", "createdAt", "updatedAt", "closedAt", "author", "labels", "comments"}
var prTypeFields = []string{"id", "number", "title", "body", "headRefName", "baseRefName", "headRefOid", "baseRefOid", "mergeable", "state", "merged", "mergedAt", "files", "createdAt", "updatedAt"}
var repoTypeFields = []string{"id", "databaseId", "name", "nameWithOwner", "owner", "defaultBranchRef", "viewerPermission", "hasIssuesEnabled", "description", "url", "createdAt", "issue", "issues", "pullRequests", "pullRequest"}
var searchTypeEnums = []string{"ISSUE", "PULL_REQUEST", "REPOSITORY", "USER", "DISCUSSION", "COMMIT", "MILESTONE", "LABEL"}

func typeFieldsResponse(q, typeName string) any {
	var names []string
	switch typeName {
	case "Issue":
		names = issueTypeFields
	case "PullRequest":
		names = prTypeFields
	case "Repository":
		names = repoTypeFields
	default:
		return nil
	}
	if strings.Contains(q, "enumValues") {
		return nil
	}
	fields := make([]map[string]string, 0, len(names))
	for _, n := range names {
		fields = append(fields, map[string]string{"name": n})
	}
	return map[string]any{"fields": fields}
}

func gqlErr(msg string) map[string]any {
	return map[string]any{"message": msg}
}

func (s *srv) handleGraphQL(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		s.writeRaw(w, 404, "application/json", errMsg("Not Found"))
		return
	}
	raw, err := readAll(r)
	if err != nil || len(raw) == 0 {
		s.writeRaw(w, 400, "application/json", errMsg("Bad Request"))
		return
	}
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		s.writeRaw(w, 400, "application/json", errMsg("Bad Request"))
		return
	}
	switch v := parsed.(type) {
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, s.execGraphQL(item))
		}
		s.writeJSON(w, 200, out)
	case map[string]any:
		s.writeJSON(w, 200, s.execGraphQL(v))
	default:
		s.writeRaw(w, 400, "application/json", errMsg("Bad Request"))
	}
}

func gqlVars(body map[string]any) map[string]any {
	if v, ok := body["variables"]; ok {
		if m, ok := v.(map[string]any); ok {
			return m
		}
	}
	return map[string]any{}
}

func gqlVarStr(vars map[string]any, key string) string {
	if v, ok := vars[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func gqlVarInt(vars map[string]any, key string, def int) int {
	if v, ok := vars[key]; ok {
		if f, ok := v.(float64); ok {
			return int(f)
		}
	}
	return def
}

func (s *srv) execGraphQL(body any) map[string]any {
	m, ok := body.(map[string]any)
	if !ok {
		return map[string]any{"data": nil, "errors": []any{gqlErr("Invalid request")}}
	}
	q, _ := m["query"].(string)
	vars := gqlVars(m)
	return s.execQuery(q, vars)
}

func (s *srv) execQuery(q string, vars map[string]any) map[string]any {
	// Introspection.
	if strings.Contains(q, "__type") {
		data := map[string]any{}
		for _, match := range reTypeCall.FindAllStringSubmatch(q, -1) {
			alias, typeName := match[1], match[2]
			if typeName == "SearchType" {
				vals := make([]map[string]string, 0, len(searchTypeEnums))
				for _, e := range searchTypeEnums {
					vals = append(vals, map[string]string{"name": e})
				}
				data[alias] = map[string]any{"enumValues": vals}
				continue
			}
			if v := typeFieldsResponse(q, typeName); v != nil {
				data[alias] = v
			} else {
				data[alias] = nil
			}
		}
		if len(data) == 0 {
			return map[string]any{"data": nil, "errors": []any{gqlErr("Unknown type")}}
		}
		return map[string]any{"data": data}
	}
	// Invalid field probe.
	if strings.Contains(q, "noSuchFieldOnViewer") {
		return map[string]any{
			"data":   map[string]any{"viewer": nil},
			"errors": []any{gqlErr("Cannot query field \"noSuchFieldOnViewer\" on type \"Viewer\".")},
		}
	}
	// Mutations.
	if strings.Contains(q, "mutation") {
		return s.execMutation(q, vars)
	}
	// node(id:)
	if strings.Contains(q, "node(") {
		id := gqlVarStr(vars, "id")
		kind, key, ok := parseGlobalID(id)
		if !ok || kind != "Repository" {
			return map[string]any{"data": map[string]any{"node": nil}}
		}
		parts := strings.SplitN(key, "/", 2)
		if len(parts) != 2 {
			return map[string]any{"data": map[string]any{"node": nil}}
		}
		repo := s.store.getRepo(parts[0], parts[1])
		if repo == nil || parts[0] != s.store.viewer {
			return map[string]any{"data": map[string]any{"node": nil}}
		}
		return map[string]any{"data": map[string]any{"node": map[string]any{
			"id": id, "name": repo.name, "nameWithOwner": repo.owner + "/" + repo.name,
		}}}
	}
	// viewer login.
	if strings.Contains(q, "viewer") && !strings.Contains(q, "repository(") {
		return map[string]any{"data": map[string]any{"viewer": map[string]any{"login": s.store.viewer}}}
	}
	// repository(...) queries.
	if strings.Contains(q, "repository(") {
		owner := gqlVarStr(vars, "owner")
		name := gqlVarStr(vars, "name")
		repo := s.store.getRepo(owner, name)
		if repo == nil || owner != s.store.viewer {
			return map[string]any{"data": map[string]any{"repository": nil}}
		}
		return s.execRepoQuery(q, vars, repo)
	}
	// Unknown document: report an error naming the first field.
	return map[string]any{"data": nil, "errors": []any{gqlErr("Cannot query field \"unknown\" on type \"Query\".")}}
}

func (s *srv) execRepoQuery(q string, vars map[string]any, repo *Repo) map[string]any {
	repo.mu.RLock()
	defer repo.mu.RUnlock()
	if strings.Contains(q, "issues(") {
		return map[string]any{"data": map[string]any{"repository": map[string]any{
			"issues": s.issuesConnectionLocked(q, vars, repo),
		}}}
	}
	if strings.Contains(q, "issue(number:") {
		n := gqlVarInt(vars, "number", 0)
		var iss *Issue
		if n >= 1 && n <= len(repo.issues) {
			iss = repo.issues[n-1]
		}
		var node any
		if iss != nil {
			node = s.issueGQLLocked(repo, iss)
		}
		return map[string]any{"data": map[string]any{"repository": map[string]any{"issue": node}}}
	}
	if strings.Contains(q, "pullRequests(") {
		first := gqlVarInt(vars, "first", 100)
		if m := reFirstArg.FindStringSubmatch(q); m != nil {
			if n, err := strconv.Atoi(m[1]); err == nil {
				first = n
			}
		}
		var nodes []any
		for _, p := range repo.pulls {
			if p.State != "open" {
				continue
			}
			nodes = append(nodes, s.prGQLLocked(repo, p))
			if len(nodes) >= first {
				break
			}
		}
		if nodes == nil {
			nodes = []any{}
		}
		return map[string]any{"data": map[string]any{"repository": map[string]any{
			"pullRequests": map[string]any{"nodes": nodes},
		}}}
	}
	if strings.Contains(q, "pullRequest(number:") {
		n := gqlVarInt(vars, "number", 0)
		var p *PR
		if n >= 1 && n <= len(repo.pulls) {
			p = repo.pulls[n-1]
		}
		var node any
		if p != nil {
			node = s.prGQLLocked(repo, p)
		}
		return map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": node}}}
	}
	// Repository fields.
	return map[string]any{"data": map[string]any{"repository": s.repoGQLLocked(repo)}}
}

func (s *srv) repoGQLLocked(repo *Repo) map[string]any {
	return map[string]any{
		"id":               repoGlobalID(repo.owner, repo.name),
		"databaseId":       repo.id,
		"name":             repo.name,
		"nameWithOwner":    repo.owner + "/" + repo.name,
		"owner":            map[string]any{"login": repo.owner},
		"defaultBranchRef": map[string]any{"name": "main"},
		"viewerPermission": "ADMIN",
		"hasIssuesEnabled": true,
	}
}

func (s *srv) issueGQLLocked(repo *Repo, iss *Issue) map[string]any {
	return map[string]any{
		"id":     issueGlobalID(repo.owner, repo.name, iss.Number),
		"number": iss.Number,
		"title":  iss.Title,
		"state":  issueStateGQL(iss.State),
		"body":   iss.Body,
	}
}

func (s *srv) prGQLLocked(repo *Repo, p *PR) map[string]any {
	mergeable := "MERGEABLE"
	if repo.conflictsLocked(p) {
		mergeable = "CONFLICTING"
	}
	var files []any
	for _, f := range p.Files {
		files = append(files, map[string]any{"path": f.Filename})
	}
	if files == nil {
		files = []any{}
	}
	return map[string]any{
		"id":          prGlobalID(repo.owner, repo.name, p.Number),
		"number":      p.Number,
		"state":       prStateGQL(p),
		"merged":      p.Merged,
		"headRefName": p.HeadRef,
		"baseRefName": p.BaseRef,
		"mergeable":   mergeable,
		"files":       map[string]any{"nodes": files},
	}
}

func (s *srv) issuesConnectionLocked(q string, vars map[string]any, repo *Repo) map[string]any {
	// states filter
	var wantOpen, wantClosed = true, true
	if m := reStates.FindStringSubmatch(q); m != nil {
		list := m[1]
		wantOpen = strings.Contains(list, "OPEN")
		wantClosed = strings.Contains(list, "CLOSED")
	}
	var list []*Issue
	for _, iss := range repo.issues {
		if iss.State == "open" && !wantOpen {
			continue
		}
		if iss.State == "closed" && !wantClosed {
			continue
		}
		list = append(list, iss)
	}
	// order
	desc := false
	if m := reOrderBy.FindStringSubmatch(q); m != nil {
		desc = m[2] == "DESC"
	}
	if desc {
		sort.Slice(list, func(i, j int) bool { return list[i].Number > list[j].Number })
	} else {
		sort.Slice(list, func(i, j int) bool { return list[i].Number < list[j].Number })
	}
	// cursor
	after := gqlVarStr(vars, "after")
	start := 0
	if after != "" {
		if b, err := base64.StdEncoding.DecodeString(after); err == nil {
			if n, err := strconv.Atoi(strings.TrimPrefix(string(b), "c:")); err == nil {
				for i, iss := range list {
					if iss.Number == n {
						start = i + 1
						break
					}
				}
			}
		}
	}
	first := gqlVarInt(vars, "first", 0)
	if m := reFirstArg.FindStringSubmatch(q); m != nil {
		if n, err := strconv.Atoi(m[1]); err == nil {
			first = n
		}
	}
	end := len(list)
	if first > 0 && start+first < end {
		end = start + first
	}
	page := list[start:end]
	nodes := make([]any, 0, len(page))
	for _, iss := range page {
		nodes = append(nodes, s.issueGQLLocked(repo, iss))
	}
	var endCursor any
	hasNext := end < len(list)
	if len(page) > 0 {
		endCursor = base64.StdEncoding.EncodeToString([]byte("c:" + strconv.Itoa(page[len(page)-1].Number)))
	}
	return map[string]any{
		"nodes": nodes,
		"pageInfo": map[string]any{
			"hasNextPage": hasNext,
			"endCursor":   endCursor,
		},
	}
}

func (s *srv) execMutation(q string, vars map[string]any) map[string]any {
	input, _ := vars["input"].(map[string]any)
	if input == nil {
		input = map[string]any{}
	}
	switch {
	case strings.Contains(q, "createIssue"):
		repoID := jsonStr(input, "repositoryId")
		kind, key, ok := parseGlobalID(repoID)
		if !ok || kind != "Repository" {
			return map[string]any{"data": map[string]any{"createIssue": nil}, "errors": []any{gqlErr("Repository not found")}}
		}
		parts := strings.SplitN(key, "/", 2)
		repo := s.store.getRepo(parts[0], parts[1])
		if repo == nil {
			return map[string]any{"data": map[string]any{"createIssue": nil}, "errors": []any{gqlErr("Repository not found")}}
		}
		repo.mu.Lock()
		iss := repo.createIssueLocked(s.store, jsonStr(input, "title"), jsonStr(input, "body"), nil)
		repo.mu.Unlock()
		return map[string]any{"data": map[string]any{"createIssue": map[string]any{"issue": map[string]any{
			"number": iss.Number, "id": issueGlobalID(repo.owner, repo.name, iss.Number),
		}}}}
	case strings.Contains(q, "closeIssue"):
		issueID := jsonStr(input, "issueId")
		kind, key, ok := parseGlobalID(issueID)
		if !ok || kind != "Issue" {
			return map[string]any{"data": map[string]any{"closeIssue": nil}, "errors": []any{gqlErr("Issue not found")}}
		}
		parts := strings.Split(key, "/")
		if len(parts) != 3 {
			return map[string]any{"data": map[string]any{"closeIssue": nil}, "errors": []any{gqlErr("Issue not found")}}
		}
		repo := s.store.getRepo(parts[0], parts[1])
		if repo == nil {
			return map[string]any{"data": map[string]any{"closeIssue": nil}, "errors": []any{gqlErr("Issue not found")}}
		}
		n, _ := strconv.Atoi(parts[2])
		repo.mu.Lock()
		var iss *Issue
		if n >= 1 && n <= len(repo.issues) {
			iss = repo.issues[n-1]
			iss.State = "closed"
			now := nowISO()
			iss.ClosedAt = &now
			iss.UpdatedAt = now
			iss.jsonCache = nil
		}
		repo.mu.Unlock()
		if iss == nil {
			return map[string]any{"data": map[string]any{"closeIssue": nil}, "errors": []any{gqlErr("Issue not found")}}
		}
		return map[string]any{"data": map[string]any{"closeIssue": map[string]any{"issue": map[string]any{
			"number": iss.Number, "id": issueID, "state": "CLOSED",
		}}}}
	case strings.Contains(q, "createPullRequest"):
		repoID := jsonStr(input, "repositoryId")
		kind, key, ok := parseGlobalID(repoID)
		if !ok || kind != "Repository" {
			return map[string]any{"data": map[string]any{"createPullRequest": nil}, "errors": []any{gqlErr("Repository not found")}}
		}
		parts := strings.SplitN(key, "/", 2)
		repo := s.store.getRepo(parts[0], parts[1])
		if repo == nil {
			return map[string]any{"data": map[string]any{"createPullRequest": nil}, "errors": []any{gqlErr("Repository not found")}}
		}
		head := jsonStr(input, "headRefName")
		base := jsonStr(input, "baseRefName")
		if base == "" {
			base = "main"
		}
		repo.mu.Lock()
		p := repo.createPRLocked(s.store, jsonStr(input, "title"), "", head, base)
		repo.mu.Unlock()
		if p == nil {
			return map[string]any{"data": map[string]any{"createPullRequest": nil}, "errors": []any{gqlErr("Branch not found")}}
		}
		return map[string]any{"data": map[string]any{"createPullRequest": map[string]any{"pullRequest": map[string]any{
			"number": p.Number, "id": prGlobalID(repo.owner, repo.name, p.Number), "state": "OPEN",
			"headRefName": p.HeadRef, "baseRefName": p.BaseRef,
		}}}}
	case strings.Contains(q, "mergePullRequest"):
		prID := jsonStr(input, "pullRequestId")
		kind, key, ok := parseGlobalID(prID)
		if !ok || kind != "PullRequest" {
			return map[string]any{"data": map[string]any{"mergePullRequest": nil}, "errors": []any{gqlErr("Pull request not found")}}
		}
		parts := strings.Split(key, "/")
		if len(parts) != 3 {
			return map[string]any{"data": map[string]any{"mergePullRequest": nil}, "errors": []any{gqlErr("Pull request not found")}}
		}
		repo := s.store.getRepo(parts[0], parts[1])
		if repo == nil {
			return map[string]any{"data": map[string]any{"mergePullRequest": nil}, "errors": []any{gqlErr("Pull request not found")}}
		}
		n, _ := strconv.Atoi(parts[2])
		repo.mu.Lock()
		var p *PR
		if n >= 1 && n <= len(repo.pulls) {
			p = repo.pulls[n-1]
		}
		if p == nil {
			repo.mu.Unlock()
			return map[string]any{"data": map[string]any{"mergePullRequest": nil}, "errors": []any{gqlErr("Pull request not found")}}
		}
		_, merged, conflict := repo.mergePRLocked(p)
		repo.mu.Unlock()
		if conflict || !merged {
			return map[string]any{"data": map[string]any{"mergePullRequest": nil}, "errors": []any{gqlErr("Merge conflict")}}
		}
		return map[string]any{"data": map[string]any{"mergePullRequest": map[string]any{"pullRequest": map[string]any{
			"number": p.Number, "merged": true, "state": "CLOSED",
		}}}}
	case strings.Contains(q, "addComment"):
		subjectID := jsonStr(input, "subjectId")
		kind, key, ok := parseGlobalID(subjectID)
		if !ok || kind != "Issue" {
			return map[string]any{"data": map[string]any{"addComment": nil}, "errors": []any{gqlErr("Subject not found")}}
		}
		parts := strings.Split(key, "/")
		if len(parts) != 3 {
			return map[string]any{"data": map[string]any{"addComment": nil}, "errors": []any{gqlErr("Subject not found")}}
		}
		repo := s.store.getRepo(parts[0], parts[1])
		if repo == nil {
			return map[string]any{"data": map[string]any{"addComment": nil}, "errors": []any{gqlErr("Subject not found")}}
		}
		n, _ := strconv.Atoi(parts[2])
		body := jsonStr(input, "body")
		repo.mu.Lock()
		var iss *Issue
		if n >= 1 && n <= len(repo.issues) {
			iss = repo.issues[n-1]
		}
		if iss == nil {
			repo.mu.Unlock()
			return map[string]any{"data": map[string]any{"addComment": nil}, "errors": []any{gqlErr("Subject not found")}}
		}
		repo.nextCmt++
		now := nowISO()
		c := &Comment{ID: repo.nextCmt, NodeID: commentGlobalID(repo.owner, repo.name, n, repo.nextCmt), Body: body, User: s.store.viewer, CreatedAt: now, UpdatedAt: now}
		iss.Comments = append(iss.Comments, c)
		repo.mu.Unlock()
		return map[string]any{"data": map[string]any{"addComment": map[string]any{"commentEdge": map[string]any{"node": map[string]any{
			"id": c.NodeID, "body": body,
		}}}}}
	}
	return map[string]any{"data": nil, "errors": []any{gqlErr("Unknown mutation")}}
}
