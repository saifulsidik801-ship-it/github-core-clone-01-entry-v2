package main

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// ---------------------------------------------------------------------------
// MCP: JSON-RPC 2.0 Streamable HTTP at /mcp, protocol revision 2026-07-28.
// ---------------------------------------------------------------------------

const mcpProtocolVersion = "2026-07-28"

type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

var mcpTools = []mcpToolDef{
	{
		Name:        "create_repo",
		Description: "Create a repository owned by the viewer.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"name":        map[string]any{"type": "string"},
			"description": map[string]any{"type": "string"},
			"private":     map[string]any{"type": "boolean"},
		}, "required": []string{"name"}},
	},
	{
		Name:        "get_repo",
		Description: "Get a repository by owner and name.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"owner": map[string]any{"type": "string"},
			"repo":  map[string]any{"type": "string"},
		}, "required": []string{"owner", "repo"}},
	},
	{
		Name:        "create_issue",
		Description: "Create an issue in a repository.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"owner": map[string]any{"type": "string"},
			"repo":  map[string]any{"type": "string"},
			"title": map[string]any{"type": "string"},
			"body":  map[string]any{"type": "string"},
		}, "required": []string{"owner", "repo", "title"}},
	},
	{
		Name:        "list_issues",
		Description: "List issues in a repository.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"owner": map[string]any{"type": "string"},
			"repo":  map[string]any{"type": "string"},
			"state": map[string]any{"type": "string"},
		}, "required": []string{"owner", "repo"}},
	},
	{
		Name:        "create_pull_request",
		Description: "Create a pull request between two branches.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"owner": map[string]any{"type": "string"},
			"repo":  map[string]any{"type": "string"},
			"title": map[string]any{"type": "string"},
			"head":  map[string]any{"type": "string"},
			"base":  map[string]any{"type": "string"},
		}, "required": []string{"owner", "repo", "title", "head", "base"}},
	},
	{
		Name:        "merge_pull_request",
		Description: "Merge a pull request with a squash commit.",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{
			"owner":  map[string]any{"type": "string"},
			"repo":   map[string]any{"type": "string"},
			"number": map[string]any{"type": "integer"},
		}, "required": []string{"owner", "repo", "number"}},
	},
}

var mcpToolsListJSON = func() []byte {
	var tools []any
	for _, t := range mcpTools {
		tools = append(tools, map[string]any{
			"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema,
		})
	}
	b, _ := json.Marshal(map[string]any{"tools": tools})
	return b
}()

func mcpErrorResult(id any, code int, msg string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"error":   map[string]any{"code": code, "message": msg},
	}
}

func mcpTextResult(id any, text string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result": map[string]any{"content": []any{
			map[string]any{"type": "text", "text": text},
		}, "isError": false},
	}
}

func mcpErrorText(id any, text string) map[string]any {
	return map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result": map[string]any{"content": []any{
			map[string]any{"type": "text", "text": text},
		}, "isError": true},
	}
}

func (s *srv) handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method != "POST" {
		s.writeRaw(w, 404, "application/json", errMsg("Not Found"))
		return
	}
	raw, err := readAll(r)
	if err != nil || len(raw) == 0 {
		s.writeRaw(w, 400, "application/json", mcpJSONError(nil, -32700, "Parse error"))
		return
	}
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		s.writeRaw(w, 400, "application/json", mcpJSONError(nil, -32700, "Parse error"))
		return
	}
	id := req["id"]
	method, _ := req["method"].(string)
	switch method {
	case "initialize":
		params, _ := req["params"].(map[string]any)
		ver, _ := params["protocolVersion"].(string)
		if ver == "" {
			ver = mcpProtocolVersion
		}
		s.writeJSON(w, 200, map[string]any{
			"jsonrpc": "2.0", "id": id,
			"result": map[string]any{
				"protocolVersion": ver,
				"serverInfo":      map[string]any{"name": "forge", "version": "1.0.0"},
				"capabilities":    map[string]any{"tools": map[string]any{}},
			},
		})
	case "notifications/initialized":
		w.WriteHeader(202)
	case "server/discover":
		s.writeJSON(w, 200, map[string]any{
			"jsonrpc": "2.0", "id": id,
			"result": map[string]any{
				"supportedVersions": []string{mcpProtocolVersion},
				"serverInfo":        map[string]any{"name": "forge", "version": "1.0.0"},
				"capabilities":      map[string]any{"tools": map[string]any{}},
			},
		})
	case "tools/list":
		// Deterministic byte-for-byte: precomputed list JSON embedded.
		body := append([]byte(`{"jsonrpc":"2.0","id":`), idJSON(id)...)
		body = append(body, []byte(`,"result":`)...)
		body = append(body, mcpToolsListJSON...)
		body = append(body, '}')
		s.writeRaw(w, 200, "application/json", body)
	case "tools/call":
		s.handleMCPCall(w, id, req)
	default:
		s.writeJSON(w, 200, mcpErrorResult(id, -32601, "Method not found"))
	}
}

func idJSON(id any) []byte {
	b, _ := json.Marshal(id)
	return b
}

func mcpJSONError(id any, code int, msg string) []byte {
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": msg},
	})
	return b
}

func mcpArgStr(args map[string]any, key string) (string, bool) {
	v, ok := args[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func mcpArgNum(args map[string]any, key string) (int, bool) {
	v, ok := args[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case string:
		i, err := strconv.Atoi(n)
		return i, err == nil
	}
	return 0, false
}

func (s *srv) handleMCPCall(w http.ResponseWriter, id any, req map[string]any) {
	params, _ := req["params"].(map[string]any)
	if params == nil {
		s.writeJSON(w, 200, mcpErrorResult(id, -32602, "Invalid params"))
		return
	}
	name, _ := params["name"].(string)
	args, _ := params["arguments"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	var known bool
	for _, t := range mcpTools {
		if t.Name == name {
			known = true
			break
		}
	}
	if !known {
		s.writeJSON(w, 200, mcpErrorResult(id, -32602, "Unknown tool: "+name))
		return
	}
	// Missing required args.
	missing := func(keys ...string) bool {
		for _, k := range keys {
			if _, ok := args[k]; !ok {
				return true
			}
		}
		return false
	}
	switch name {
	case "create_repo":
		if missing("name") {
			s.writeJSON(w, 200, mcpErrorText(id, "Missing required argument: name"))
			return
		}
		nm, _ := mcpArgStr(args, "name")
		desc, _ := mcpArgStr(args, "description")
		priv, _ := args["private"].(bool)
		_, created := s.store.createRepo(nm, desc, priv)
		if !created {
			s.writeJSON(w, 200, mcpErrorText(id, "Repository already exists"))
			return
		}
		s.writeJSON(w, 200, mcpTextResult(id, "Created repository "+s.store.viewer+"/"+nm))
	case "get_repo":
		owner, ok1 := mcpArgStr(args, "owner")
		repo, ok2 := mcpArgStr(args, "repo")
		if !ok1 || !ok2 {
			s.writeJSON(w, 200, mcpErrorText(id, "Missing required argument: owner or repo"))
			return
		}
		r := s.store.getRepo(owner, repo)
		if r == nil {
			s.writeJSON(w, 200, mcpErrorText(id, "Repository not found"))
			return
		}
		s.writeJSON(w, 200, mcpTextResult(id, "Repository "+r.owner+"/"+r.name))
	case "create_issue":
		owner, ok1 := mcpArgStr(args, "owner")
		repo, ok2 := mcpArgStr(args, "repo")
		title, ok3 := mcpArgStr(args, "title")
		if !ok1 || !ok2 || !ok3 {
			s.writeJSON(w, 200, mcpErrorText(id, "Missing required argument"))
			return
		}
		r := s.store.getRepo(owner, repo)
		if r == nil {
			s.writeJSON(w, 200, mcpErrorText(id, "Repository not found"))
			return
		}
		body, _ := mcpArgStr(args, "body")
		r.mu.Lock()
		iss := r.createIssueLocked(s.store, title, body, nil)
		r.mu.Unlock()
		s.writeJSON(w, 200, mcpTextResult(id, "Created issue #"+strconv.Itoa(iss.Number)+" in "+owner+"/"+repo))
	case "list_issues":
		owner, ok1 := mcpArgStr(args, "owner")
		repo, ok2 := mcpArgStr(args, "repo")
		if !ok1 || !ok2 {
			s.writeJSON(w, 200, mcpErrorText(id, "Missing required argument: owner or repo"))
			return
		}
		r := s.store.getRepo(owner, repo)
		if r == nil {
			s.writeJSON(w, 200, mcpErrorText(id, "Repository not found"))
			return
		}
		state, _ := mcpArgStr(args, "state")
		r.mu.RLock()
		var items []map[string]any
		for _, iss := range r.issues {
			if state != "" && iss.State != state {
				continue
			}
			items = append(items, map[string]any{"number": iss.Number, "title": iss.Title, "state": iss.State})
		}
		r.mu.RUnlock()
		if items == nil {
			items = []map[string]any{}
		}
		b, _ := json.Marshal(items)
		s.writeJSON(w, 200, mcpTextResult(id, string(b)))
	case "create_pull_request":
		owner, ok1 := mcpArgStr(args, "owner")
		repo, ok2 := mcpArgStr(args, "repo")
		title, ok3 := mcpArgStr(args, "title")
		head, ok4 := mcpArgStr(args, "head")
		base, ok5 := mcpArgStr(args, "base")
		if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 {
			s.writeJSON(w, 200, mcpErrorText(id, "Missing required argument"))
			return
		}
		r := s.store.getRepo(owner, repo)
		if r == nil {
			s.writeJSON(w, 200, mcpErrorText(id, "Repository not found"))
			return
		}
		r.mu.Lock()
		p := r.createPRLocked(s.store, title, "", head, base)
		r.mu.Unlock()
		if p == nil {
			s.writeJSON(w, 200, mcpErrorText(id, "Branch not found"))
			return
		}
		s.writeJSON(w, 200, mcpTextResult(id, "Created pull request #"+strconv.Itoa(p.Number)+" in "+owner+"/"+repo))
	case "merge_pull_request":
		owner, ok1 := mcpArgStr(args, "owner")
		repo, ok2 := mcpArgStr(args, "repo")
		num, ok3 := mcpArgNum(args, "number")
		if !ok1 || !ok2 || !ok3 {
			s.writeJSON(w, 200, mcpErrorText(id, "Missing required argument"))
			return
		}
		r := s.store.getRepo(owner, repo)
		if r == nil {
			s.writeJSON(w, 200, mcpErrorText(id, "Repository not found"))
			return
		}
		r.mu.Lock()
		var p *PR
		if num >= 1 && num <= len(r.pulls) {
			p = r.pulls[num-1]
		}
		if p == nil {
			r.mu.Unlock()
			s.writeJSON(w, 200, mcpErrorText(id, "Pull request not found"))
			return
		}
		_, merged, conflict := r.mergePRLocked(p)
		r.mu.Unlock()
		if conflict || !merged {
			s.writeJSON(w, 200, mcpErrorText(id, "Merge conflict"))
			return
		}
		s.writeJSON(w, 200, mcpTextResult(id, "Merged pull request #"+strconv.Itoa(num)+" in "+owner+"/"+repo))
	}
}
