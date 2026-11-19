package main

import (
	"encoding/base64"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ---------------------------------------------------------------------------
// Deterministic scenario identity (ported from the kit's seeded generators:
// same seed, same streams, so the viewer login matches the suite's scenario).
// ---------------------------------------------------------------------------

const u64mask uint64 = 0xffffffffffffffff

func fnv1a64(text string) uint64 {
	var hash uint64 = 0xcbf29ce484222325
	for i := 0; i < len(text); i++ {
		hash ^= uint64(text[i])
		hash = (hash * 0x100000001b3) & u64mask
	}
	return hash
}

func mix64(value uint64) uint64 {
	z := (value + 0x9e3779b97f4a7c15) & u64mask
	z = ((z ^ (z >> 30)) * 0xbf58476d1ce4e5b9) & u64mask
	z = ((z ^ (z >> 27)) * 0x94d049bb133111eb) & u64mask
	return (z ^ (z >> 31)) & u64mask
}

type seededRng struct{ state uint64 }

func (r *seededRng) nextU64() uint64 {
	x := r.state
	x = (x ^ (x >> 12)) & u64mask
	x = (x ^ (x << 25)) & u64mask
	x = (x ^ (x >> 27)) & u64mask
	r.state = x
	return (x * 0x2545f4914f6cdd1d) & u64mask
}

func (r *seededRng) nextU32() uint32 { return uint32(r.nextU64() >> 32) }

func (r *seededRng) nextInt(max int) int {
	if max == 1 {
		return 0
	}
	limit := uint32(4294967296 - (4294967296 % uint64(max)))
	for {
		v := r.nextU32()
		if v < limit {
			return int(v % uint32(max))
		}
	}
}

var firstNames = []string{
	"ada", "bram", "cora", "dmitri", "elena", "felix", "greta", "hugo", "iris", "jonas", "kira", "lars",
	"mira", "nico", "olga", "pavel", "quinn", "rosa", "sven", "tess", "ulla", "vito", "wren", "yara", "zane",
}
var lastNames = []string{
	"anders", "bauer", "costa", "dahl", "eriksen", "fischer", "garcia", "holm", "iversen", "jansen", "keller",
	"lind", "moreau", "novak", "olsen", "peters", "quist", "rossi", "sato", "turi", "ulrich", "vega", "weber",
	"xu", "yilmaz", "zimmer",
}

func viewerLogin() string {
	r := &seededRng{state: mix64(0x6a09e667f3bcc909 ^ fnv1a64("users"))}
	first := firstNames[r.nextInt(len(firstNames))]
	last := lastNames[r.nextInt(len(lastNames))]
	return first + last
}

// ---------------------------------------------------------------------------
// Deterministic clock: every timestamp derives from a counter, never the wall
// clock, so a replay produces identical bytes.
// ---------------------------------------------------------------------------

var clockSeq atomic.Int64

func nowISO() string {
	s := clockSeq.Add(1)
	return time.Unix(1735689600+s, 0).UTC().Format("2006-01-02T15:04:05Z")
}

func commitTimestamp() int64 { return 1700000000 + clockSeq.Add(1) }

// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

const zeroOID = "0000000000000000000000000000000000000000"

type Label struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Color string `json:"color"`
	Desc  string `json:"description"`
}

type Comment struct {
	ID        int64  `json:"id"`
	NodeID    string `json:"node_id"`
	Body      string `json:"body"`
	User      string `json:"user"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

type Issue struct {
	ID        int64
	Number    int
	Title     string
	Body      string
	State     string // "open" | "closed"
	Labels    []string
	CreatedAt string
	UpdatedAt string
	ClosedAt  *string
	Comments  []*Comment
	jsonCache []byte // serialized REST view, rebuilt on mutation
}

type pullFile struct {
	Filename  string `json:"filename"`
	Status    string `json:"status"`
	Additions int    `json:"additions"`
	Deletions int    `json:"deletions"`
	Changes   int    `json:"changes"`
}

type PR struct {
	ID          int64
	Number      int
	Title       string
	Body        string
	State       string // "open" | "closed"
	Merged      bool
	MergedAt    *string
	MergeCommit string
	HeadRef     string
	BaseRef     string
	HeadSha     string
	BaseSha     string
	Files       []pullFile
	CreatedAt   string
	UpdatedAt   string
}

type Repo struct {
	mu        sync.RWMutex
	id        int64
	name      string
	owner     string
	desc      string
	priv      bool
	createdAt string
	objects   map[string]*Obj
	refs      map[string]string
	labels    map[string]*Label
	issues    []*Issue
	pulls     []*PR
	nextLabel int64
	nextCmt   int64
	version   uint64
	packCache map[string][]byte
}

type Store struct {
	mu      sync.RWMutex
	repos   map[string]*Repo
	repoSeq atomic.Int64
	reqSeq  atomic.Uint64
	viewer  string
}

func newStore() *Store {
	return &Store{repos: make(map[string]*Repo), viewer: viewerLogin()}
}

func (s *Store) getRepo(owner, name string) *Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.repos[owner+"/"+name]
}

func (s *Store) createRepo(name, desc string, priv bool) (*Repo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.viewer + "/" + name
	if _, ok := s.repos[key]; ok {
		return nil, false
	}
	id := s.repoSeq.Add(1)
	r := &Repo{
		id: id, name: name, owner: s.viewer, desc: desc, priv: priv,
		createdAt: nowISO(),
		objects:   make(map[string]*Obj),
		refs:      make(map[string]string),
		labels:    make(map[string]*Label),
		packCache: make(map[string][]byte),
	}
	s.repos[key] = r
	return r, true
}

func (s *Store) deleteRepo(owner, name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := owner + "/" + name
	if _, ok := s.repos[key]; !ok {
		return false
	}
	delete(s.repos, key)
	return true
}

func (s *Store) listRepos() []*Repo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Repo, 0, len(s.repos))
	for _, r := range s.repos {
		if r.owner == s.viewer {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// ---------------------------------------------------------------------------
// Opaque global (Relay) ids: base64 of a typed key, stable across reads.
// ---------------------------------------------------------------------------

func globalID(kind, key string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(kind + ":" + key))
}

func parseGlobalID(id string) (kind, key string, ok bool) {
	b, err := base64.RawURLEncoding.DecodeString(id)
	if err != nil {
		return "", "", false
	}
	parts := strings.SplitN(string(b), ":", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func repoGlobalID(owner, name string) string { return globalID("Repository", owner+"/"+name) }
func issueGlobalID(owner, repo string, number int) string {
	return globalID("Issue", owner+"/"+repo+"/"+strconv.Itoa(number))
}
func prGlobalID(owner, repo string, number int) string {
	return globalID("PullRequest", owner+"/"+repo+"/"+strconv.Itoa(number))
}
func commentGlobalID(owner, repo string, number int, cid int64) string {
	return globalID("Comment", owner+"/"+repo+"/"+strconv.Itoa(number)+"/"+strconv.FormatInt(cid, 10))
}

// ---------------------------------------------------------------------------
// Small JSON helpers
// ---------------------------------------------------------------------------

type userView struct {
	Login string `json:"login"`
	ID    int64  `json:"id"`
	Type  string `json:"type"`
}

type labelView struct {
	ID    int64  `json:"id,omitempty"`
	Name  string `json:"name"`
	Color string `json:"color,omitempty"`
	Desc  string `json:"description,omitempty"`
}

func (s *Store) viewerUser() userView { return userView{Login: s.viewer, ID: 1000, Type: "User"} }

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte("{}")
	}
	return b
}

func jsonStr(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func jsonBool(m map[string]any, key string, def bool) bool {
	if v, ok := m[key]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return def
}

func jsonInt(m map[string]any, key string) (int, bool) {
	if v, ok := m[key]; ok {
		if f, ok := v.(float64); ok {
			return int(f), true
		}
	}
	return 0, false
}

func issueStateGQL(state string) string {
	if state == "closed" {
		return "CLOSED"
	}
	return "OPEN"
}

func prStateGQL(p *PR) string {
	if p.Merged {
		return "MERGED"
	}
	if p.State == "closed" {
		return "CLOSED"
	}
	return "OPEN"
}

// ---------------------------------------------------------------------------
// Shared store operations used by REST, GraphQL and MCP
// ---------------------------------------------------------------------------

func (r *Repo) issueJSON(s *Store, iss *Issue) []byte {
	if iss.jsonCache != nil {
		return iss.jsonCache
	}
	labels := make([]labelView, 0, len(iss.Labels))
	for _, name := range iss.Labels {
		color := "ededed"
		if l, ok := r.labels[name]; ok {
			color = l.Color
		}
		labels = append(labels, labelView{Name: name, Color: color})
	}
	v := struct {
		ID        int64       `json:"id"`
		NodeID    string      `json:"node_id"`
		Number    int         `json:"number"`
		Title     string      `json:"title"`
		Body      string      `json:"body"`
		State     string      `json:"state"`
		Labels    []labelView `json:"labels"`
		User      userView    `json:"user"`
		CreatedAt string      `json:"created_at"`
		UpdatedAt string      `json:"updated_at"`
		ClosedAt  *string     `json:"closed_at"`
		Comments  int         `json:"comments"`
	}{
		ID: iss.ID, NodeID: issueGlobalID(r.owner, r.name, iss.Number), Number: iss.Number,
		Title: iss.Title, Body: iss.Body, State: iss.State, Labels: labels,
		User:      userView{Login: s.viewer, ID: 1000, Type: "User"},
		CreatedAt: iss.CreatedAt, UpdatedAt: iss.UpdatedAt, ClosedAt: iss.ClosedAt,
		Comments: len(iss.Comments),
	}
	iss.jsonCache = mustJSON(v)
	return iss.jsonCache
}

// createIssueLocked: caller holds r.mu (write).
func (r *Repo) createIssueLocked(s *Store, title, body string, labels []string) *Issue {
	n := len(r.issues) + 1
	now := nowISO()
	iss := &Issue{
		ID: int64(10000 + n), Number: n, Title: title, Body: body, State: "open",
		Labels: append([]string(nil), labels...), CreatedAt: now, UpdatedAt: now,
	}
	r.issues = append(r.issues, iss)
	iss.jsonCache = nil
	_ = s
	return iss
}

// prJSON builds the REST view of a pull request.
func (r *Repo) prJSON(s *Store, p *PR) []byte {
	v := struct {
		ID             int64    `json:"id"`
		NodeID         string   `json:"node_id"`
		Number         int      `json:"number"`
		Title          string   `json:"title"`
		Body           string   `json:"body"`
		State          string   `json:"state"`
		Head           any      `json:"head"`
		Base           any      `json:"base"`
		Merged         bool     `json:"merged"`
		MergedAt       *string  `json:"merged_at"`
		MergeCommit    *string  `json:"merge_commit_sha"`
		Mergeable      bool     `json:"mergeable"`
		MergeableState string   `json:"mergeable_state"`
		User           userView `json:"user"`
		CreatedAt      string   `json:"created_at"`
		UpdatedAt      string   `json:"updated_at"`
	}{
		ID: p.ID, NodeID: prGlobalID(r.owner, r.name, p.Number), Number: p.Number,
		Title: p.Title, Body: p.Body, State: p.State,
		Head:   map[string]string{"ref": p.HeadRef, "sha": p.HeadSha, "label": r.owner + ":" + p.HeadRef},
		Base:   map[string]string{"ref": p.BaseRef, "sha": p.BaseSha, "label": r.owner + ":" + p.BaseRef},
		Merged: p.Merged, MergedAt: p.MergedAt,
		Mergeable: !r.conflictsLocked(p), MergeableState: mergeableState(r, p),
		User: s.viewerUser(), CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	}
	if p.MergeCommit != "" {
		v.MergeCommit = &p.MergeCommit
	}
	return mustJSON(v)
}

func mergeableState(r *Repo, p *PR) string {
	if p.State != "open" {
		return "unknown"
	}
	if r.conflictsLocked(p) {
		return "dirty"
	}
	return "clean"
}

// conflictsLocked reports whether merging p would conflict. Caller holds at least r.mu read.
func (r *Repo) conflictsLocked(p *PR) bool {
	if p.State != "open" {
		return false
	}
	baseTip := r.refs["refs/heads/"+p.BaseRef]
	headTip := r.refs["refs/heads/"+p.HeadRef]
	if baseTip == "" || headTip == "" {
		return false
	}
	mb := mergeBase(r.objects, baseTip, headTip)
	if mb == "" {
		return false
	}
	baseTree := commitTree(r.objects, baseTip)
	headTree := commitTree(r.objects, headTip)
	mbTree := commitTree(r.objects, mb)
	baseFlat := flattenTree(r.objects, baseTree)
	headFlat := flattenTree(r.objects, headTree)
	mbFlat := flattenTree(r.objects, mbTree)
	for path, he := range headFlat {
		mbE, inMb := mbFlat[path]
		if !inMb {
			continue
		}
		be, inBase := baseFlat[path]
		if !inBase {
			continue
		}
		if he.oid != mbE.oid && be.oid != mbE.oid && he.oid != be.oid {
			return true
		}
		_ = he
	}
	// deletions on both sides with different content are covered above; also
	// a file deleted on base side but changed on head side.
	for path, mbE := range mbFlat {
		_, inHead := headFlat[path]
		_, inBase := baseFlat[path]
		if !inHead && !inBase {
			continue
		}
		if !inHead && inBase {
			if be := baseFlat[path]; be.oid != mbE.oid {
				return true
			}
		}
		if inHead && !inBase {
			if he := headFlat[path]; he.oid != mbE.oid {
				return true
			}
		}
	}
	return false
}

// createPRLocked: caller holds r.mu (write). Returns nil when head/base missing.
func (r *Repo) createPRLocked(s *Store, title, body, head, base string) *PR {
	headTip := r.refs["refs/heads/"+head]
	baseTip := r.refs["refs/heads/"+base]
	if headTip == "" || baseTip == "" {
		return nil
	}
	// An open PR for the same head/base already exists: return it (idempotent).
	for _, p := range r.pulls {
		if p.State == "open" && p.HeadRef == head && p.BaseRef == base {
			return p
		}
	}
	n := len(r.pulls) + 1
	now := nowISO()
	p := &PR{
		ID: int64(20000 + n), Number: n, Title: title, Body: body, State: "open",
		HeadRef: head, BaseRef: base, HeadSha: headTip, BaseSha: baseTip,
		CreatedAt: now, UpdatedAt: now,
	}
	p.Files = r.diffPullFilesLocked(baseTip, headTip)
	r.pulls = append(r.pulls, p)
	return p
}

func (r *Repo) diffPullFilesLocked(baseTip, headTip string) []pullFile {
	baseFlat := flattenTree(r.objects, commitTree(r.objects, baseTip))
	headFlat := flattenTree(r.objects, commitTree(r.objects, headTip))
	var out []pullFile
	paths := map[string]bool{}
	for p := range baseFlat {
		paths[p] = true
	}
	for p := range headFlat {
		paths[p] = true
	}
	sorted := make([]string, 0, len(paths))
	for p := range paths {
		sorted = append(sorted, p)
	}
	sort.Strings(sorted)
	for _, p := range sorted {
		be, inBase := baseFlat[p]
		he, inHead := headFlat[p]
		var status string
		var oldData, newData []byte
		switch {
		case inBase && !inHead:
			status = "removed"
			if o := r.objects[be.oid]; o != nil {
				oldData = o.data
			}
		case !inBase && inHead:
			status = "added"
			if o := r.objects[he.oid]; o != nil {
				newData = o.data
			}
		case inBase && inHead && be.oid != he.oid:
			status = "modified"
			if o := r.objects[be.oid]; o != nil {
				oldData = o.data
			}
			if o := r.objects[he.oid]; o != nil {
				newData = o.data
			}
		default:
			continue
		}
		add, del := lineDiff(oldData, newData)
		if status != "removed" && add == 0 {
			add = 1
		}
		out = append(out, pullFile{Filename: p, Status: status, Additions: add, Deletions: del, Changes: add + del})
	}
	if out == nil {
		out = []pullFile{}
	}
	return out
}

// mergePRLocked squash-merges p onto its base branch. Caller holds r.mu (write).
// Returns (sha, ok, conflict).
func (r *Repo) mergePRLocked(p *PR) (string, bool, bool) {
	if p.State != "open" {
		return "", false, false
	}
	if r.conflictsLocked(p) {
		return "", false, true
	}
	baseTip := r.refs["refs/heads/"+p.BaseRef]
	headTip := r.refs["refs/heads/"+p.HeadRef]
	if baseTip == "" || headTip == "" {
		return "", false, false
	}
	headTree := commitTree(r.objects, headTip)
	msg := "Merge pull request #" + strconv.Itoa(p.Number) + " from " + p.HeadRef + "\n"
	sha := r.createCommitLocked(headTree, []string{baseTip}, msg)
	r.refs["refs/heads/"+p.BaseRef] = sha
	r.version++
	r.packCache = make(map[string][]byte)
	now := nowISO()
	p.State = "closed"
	p.Merged = true
	p.MergedAt = &now
	p.MergeCommit = sha
	p.UpdatedAt = now
	return sha, true, false
}

// createCommitLocked builds and stores a commit object. Caller holds r.mu (write).
func (r *Repo) createCommitLocked(tree string, parents []string, message string) string {
	ts := commitTimestamp()
	var b strings.Builder
	b.WriteString("tree " + tree + "\n")
	for _, par := range parents {
		b.WriteString("parent " + par + "\n")
	}
	who := "Forge <forge@make.fun> " + strconv.FormatInt(ts, 10) + " +0000"
	b.WriteString("author " + who + "\n")
	b.WriteString("committer " + who + "\n")
	b.WriteString("\n")
	b.WriteString(message)
	data := []byte(b.String())
	oid := hashObjectBytes("commit", data)
	if _, ok := r.objects[oid]; !ok {
		r.objects[oid] = &Obj{typ: 1, data: data, tree: tree, parents: append([]string(nil), parents...)}
	}
	return oid
}
