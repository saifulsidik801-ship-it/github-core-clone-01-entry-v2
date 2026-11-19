package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// forge — a fast in-memory GitHub-core clone (REST v3 + GraphQL v4 + MCP +
// git smart HTTP). stdlib only. Single process, one hand-rolled router.
// ---------------------------------------------------------------------------

type srv struct {
	store *Store
	token string // MAKEFUN_SHOP_TOKEN; empty = any non-empty Bearer accepted
}

func readAll(r *http.Request) ([]byte, error) {
	const maxBody = 256 << 20 // 256 MiB — packs are big
	return io.ReadAll(io.LimitReader(r.Body, maxBody+1))
}

func (s *srv) writeRaw(w http.ResponseWriter, status int, contentType string, body []byte) {
	id := s.store.reqSeq.Add(1)
	w.Header().Set("X-GitHub-Request-Id", strconv.FormatUint(id, 10))
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (s *srv) writeJSON(w http.ResponseWriter, status int, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		s.writeRaw(w, 500, "application/json", errMsg("Internal Server Error"))
		return
	}
	s.writeRaw(w, status, "application/json", b)
}

func (s *srv) unauthorized(w http.ResponseWriter) {
	s.writeRaw(w, 401, "application/json", errMsg("Bad credentials"))
}

// authorized checks the Bearer token for API routes. Empty token env means
// any non-empty Bearer passes; a configured token must match exactly.
func (s *srv) authorized(r *http.Request) bool {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return false
	}
	tok := h[len("Bearer "):]
	if tok == "" {
		return false
	}
	if s.token != "" && tok != s.token {
		return false
	}
	return true
}

func (s *srv) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	segs := splitPath(path)

	// /healthz — unauthenticated boot probe.
	if path == "/healthz" && r.Method == "GET" {
		s.writeRaw(w, 200, "application/json", []byte(`{"ok":true}`))
		return
	}
	if path == "/api/graphql" || path == "/graphql" {
		if !s.authorized(r) {
			s.unauthorized(w)
			return
		}
		s.handleGraphQL(w, r)
		return
	}
	if path == "/mcp" {
		if !s.authorized(r) {
			s.unauthorized(w)
			return
		}
		s.handleMCP(w, r)
		return
	}
	if len(segs) >= 2 && segs[0] == "api" && segs[1] == "v3" {
		if !s.authorized(r) {
			s.unauthorized(w)
			return
		}
		if s.handleREST(w, r, segs[2:]) {
			return
		}
		s.writeRaw(w, 404, "application/json", errMsg("Not Found"))
		return
	}
	// Git smart HTTP: /{owner}/{repo}.git/...
	if len(segs) >= 2 && strings.HasSuffix(segs[1], ".git") {
		if s.handleGit(w, r, segs) {
			return
		}
		s.writeRaw(w, 404, "application/json", errMsg("Not Found"))
		return
	}
	s.writeRaw(w, 404, "application/json", errMsg("Not Found"))
}

func splitPath(p string) []string {
	p = strings.Trim(p, "/")
	if p == "" {
		return nil
	}
	return strings.Split(p, "/")
}

func main() {
	token := os.Getenv("MAKEFUN_SHOP_TOKEN")
	server := &srv{store: newStore(), token: token}

	port := os.Getenv("PORT")
	addr := ":0"
	if port != "" {
		addr = ":" + port
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	if port == "" {
		fmt.Printf("PORT %d\n", ln.Addr().(*net.TCPAddr).Port)
		_ = os.Stdout.Sync()
	}
	httpServer := &http.Server{Handler: server}
	if err := httpServer.Serve(ln); err != nil {
		fmt.Fprintln(os.Stderr, "serve:", err)
		os.Exit(1)
	}
}
