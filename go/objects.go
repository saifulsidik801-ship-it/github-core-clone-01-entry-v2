package main

import (
	"bytes"
	"compress/flate"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// Git object model
// ---------------------------------------------------------------------------

// Object types: 1 commit, 2 tree, 3 blob, 4 tag.
type Obj struct {
	typ     byte
	data    []byte
	tree    string   // commit: tree oid
	parents []string // commit: parent oids
	tagObj  string   // tag: target oid
	tagTyp  byte     // tag: target type
	parsed  bool
}

func typeName(t byte) string {
	switch t {
	case 1:
		return "commit"
	case 2:
		return "tree"
	case 3:
		return "blob"
	case 4:
		return "tag"
	}
	return ""
}

func typeCode(name string) (byte, bool) {
	switch name {
	case "commit":
		return 1, true
	case "tree":
		return 2, true
	case "blob":
		return 3, true
	case "tag":
		return 4, true
	}
	return 0, false
}

func hashObjectBytes(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ))
	h.Write([]byte{' '})
	h.Write([]byte(strconv.Itoa(len(data))))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func hashObj(t byte, data []byte) string { return hashObjectBytes(typeName(t), data) }

func isOID(s string) bool {
	if len(s) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// Commit / tree / tag parsing and building
// ---------------------------------------------------------------------------

func parseCommitData(o *Obj) {
	if o.parsed || o.typ != 1 {
		return
	}
	o.parsed = true
	data := o.data
	// headers end at blank line
	idx := bytes.Index(data, []byte("\n\n"))
	head := data
	if idx >= 0 {
		head = data[:idx]
	}
	for _, line := range strings.Split(string(head), "\n") {
		if strings.HasPrefix(line, "tree ") {
			o.tree = line[5:]
		} else if strings.HasPrefix(line, "parent ") {
			o.parents = append(o.parents, line[7:])
		}
	}
}

func parseTagData(o *Obj) {
	if o.parsed || o.typ != 4 {
		return
	}
	o.parsed = true
	for _, line := range strings.Split(string(o.data), "\n") {
		if line == "" {
			break
		}
		if strings.HasPrefix(line, "object ") {
			o.tagObj = line[7:]
		} else if strings.HasPrefix(line, "type ") {
			if c, ok := typeCode(line[5:]); ok {
				o.tagTyp = c
			}
		}
	}
}

type treeEntry struct {
	mode string
	name string
	oid  string
}

func parseTree(data []byte) []treeEntry {
	var out []treeEntry
	off := 0
	for off < len(data) {
		sp := bytes.IndexByte(data[off:], ' ')
		if sp < 0 {
			break
		}
		mode := string(data[off : off+sp])
		rest := off + sp + 1
		nul := bytes.IndexByte(data[rest:], 0)
		if nul < 0 || rest+nul+21 > len(data) {
			break
		}
		name := string(data[rest : rest+nul])
		oid := hex.EncodeToString(data[rest+nul+1 : rest+nul+21])
		out = append(out, treeEntry{mode: mode, name: name, oid: oid})
		off = rest + nul + 21
	}
	return out
}

// git tree-entry sort: directory names compare as if suffixed with '/'.
func treeSortKey(mode, name string) string {
	if mode == "40000" {
		return name + "/"
	}
	return name
}

func buildTree(entries []treeEntry) []byte {
	sorted := append([]treeEntry(nil), entries...)
	sort.Slice(sorted, func(i, j int) bool {
		return treeSortKey(sorted[i].mode, sorted[i].name) < treeSortKey(sorted[j].mode, sorted[j].name)
	})
	var buf bytes.Buffer
	for _, e := range sorted {
		buf.WriteString(e.mode)
		buf.WriteByte(' ')
		buf.WriteString(e.name)
		buf.WriteByte(0)
		raw, _ := hex.DecodeString(e.oid)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// peelToCommit resolves tags to the commit they point at.
func peelToCommit(objects map[string]*Obj, oid string) string {
	for i := 0; i < 16; i++ {
		o := objects[oid]
		if o == nil {
			return ""
		}
		if o.typ == 1 {
			return oid
		}
		if o.typ == 4 {
			parseTagData(o)
			oid = o.tagObj
			continue
		}
		return ""
	}
	return ""
}

func commitTree(objects map[string]*Obj, commitOID string) string {
	o := objects[commitOID]
	if o == nil || o.typ != 1 {
		return ""
	}
	parseCommitData(o)
	return o.tree
}

// ---------------------------------------------------------------------------
// Reachability
// ---------------------------------------------------------------------------

func reachableCommits(objects map[string]*Obj, tips []string) map[string]bool {
	seen := map[string]bool{}
	var stack []string
	for _, t := range tips {
		if c := peelToCommit(objects, t); c != "" {
			stack = append(stack, c)
		}
	}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		o := objects[oid]
		if o == nil || o.typ != 1 {
			continue
		}
		seen[oid] = true
		parseCommitData(o)
		stack = append(stack, o.parents...)
	}
	return seen
}

// reachableObjects returns every object reachable from tips (commits, trees,
// blobs, tags), following tag targets.
func reachableObjects(objects map[string]*Obj, tips []string) map[string]bool {
	seen := map[string]bool{}
	var stack []string
	stack = append(stack, tips...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		o := objects[oid]
		if o == nil {
			continue
		}
		seen[oid] = true
		switch o.typ {
		case 1:
			parseCommitData(o)
			stack = append(stack, o.tree)
			stack = append(stack, o.parents...)
		case 4:
			parseTagData(o)
			stack = append(stack, o.tagObj)
		case 2:
			for _, e := range parseTree(o.data) {
				if e.mode != "160000" {
					stack = append(stack, e.oid)
				}
			}
		}
	}
	return seen
}

// reachableObjectsStopAt is reachableObjects but never descends into the
// parents of commits in stop (their trees are still included).
func reachableObjectsStopAt(objects map[string]*Obj, tips []string, stop map[string]bool) map[string]bool {
	seen := map[string]bool{}
	var stack []string
	stack = append(stack, tips...)
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		o := objects[oid]
		if o == nil {
			continue
		}
		seen[oid] = true
		switch o.typ {
		case 1:
			parseCommitData(o)
			stack = append(stack, o.tree)
			if !stop[oid] {
				stack = append(stack, o.parents...)
			}
		case 4:
			parseTagData(o)
			stack = append(stack, o.tagObj)
		case 2:
			for _, e := range parseTree(o.data) {
				if e.mode != "160000" {
					stack = append(stack, e.oid)
				}
			}
		}
	}
	return seen
}

func isDescendant(objects map[string]*Obj, ancestor, descendant string) bool {
	if ancestor == descendant {
		return true
	}
	if ancestor == zeroOID {
		return true
	}
	seen := map[string]bool{}
	stack := []string{descendant}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		o := objects[oid]
		if o == nil || o.typ != 1 {
			continue
		}
		parseCommitData(o)
		for _, p := range o.parents {
			if p == ancestor {
				return true
			}
			stack = append(stack, p)
		}
	}
	return false
}

func mergeBase(objects map[string]*Obj, a, b string) string {
	if a == "" || b == "" {
		return ""
	}
	ancA := reachableCommits(objects, []string{a})
	if ancA[b] {
		return b
	}
	// BFS from b following parents; first node in ancA wins.
	seen := map[string]bool{}
	queue := []string{b}
	for len(queue) > 0 {
		oid := queue[0]
		queue = queue[1:]
		if seen[oid] {
			continue
		}
		seen[oid] = true
		if ancA[oid] {
			return oid
		}
		o := objects[oid]
		if o == nil || o.typ != 1 {
			continue
		}
		parseCommitData(o)
		queue = append(queue, o.parents...)
	}
	return ""
}

// ---------------------------------------------------------------------------
// Flat trees (path -> entry) for contents / diff / merge
// ---------------------------------------------------------------------------

type flatEntry struct {
	mode string
	oid  string
}

func flattenTree(objects map[string]*Obj, root string) map[string]flatEntry {
	out := map[string]flatEntry{}
	if root == "" {
		return out
	}
	var walk func(treeOID, prefix string)
	walk = func(treeOID, prefix string) {
		o := objects[treeOID]
		if o == nil || o.typ != 2 {
			return
		}
		for _, e := range parseTree(o.data) {
			path := prefix + e.name
			if e.mode == "40000" {
				walk(e.oid, path+"/")
			} else if e.mode != "160000" {
				out[path] = flatEntry{mode: e.mode, oid: e.oid}
			}
		}
	}
	walk(root, "")
	return out
}

// buildTreeFromFlat stores every tree object it creates and returns the root.
func buildTreeFromFlat(objects map[string]*Obj, flat map[string]flatEntry) string {
	type node struct {
		files map[string]flatEntry
		dirs  map[string]*node
	}
	mk := func() *node { return &node{files: map[string]flatEntry{}, dirs: map[string]*node{}} }
	root := mk()
	for path, e := range flat {
		parts := strings.Split(path, "/")
		n := root
		for _, seg := range parts[:len(parts)-1] {
			if _, ok := n.dirs[seg]; !ok {
				n.dirs[seg] = mk()
			}
			n = n.dirs[seg]
		}
		n.files[parts[len(parts)-1]] = e
	}
	var build func(n *node) string
	build = func(n *node) string {
		var entries []treeEntry
		for name, e := range n.files {
			entries = append(entries, treeEntry{mode: e.mode, name: name, oid: e.oid})
		}
		for name, child := range n.dirs {
			entries = append(entries, treeEntry{mode: "40000", name: name, oid: build(child)})
		}
		data := buildTree(entries)
		oid := hashObj(2, data)
		if _, ok := objects[oid]; !ok {
			objects[oid] = &Obj{typ: 2, data: data}
		}
		return oid
	}
	return build(root)
}

func lineDiff(oldData, newData []byte) (add, del int) {
	countLines := func(b []byte) int {
		if len(b) == 0 {
			return 0
		}
		n := bytes.Count(b, []byte{'\n'})
		if b[len(b)-1] != '\n' {
			n++
		}
		return n
	}
	// Cheap approximation: lines present in new but not a shared prefix/suffix.
	// For the checks, additions >= 1 on changed files is what matters.
	oldLines := bytes.Split(oldData, []byte{'\n'})
	newLines := bytes.Split(newData, []byte{'\n'})
	oldSet := map[string]int{}
	for _, l := range oldLines {
		oldSet[string(l)]++
	}
	newSet := map[string]int{}
	for _, l := range newLines {
		newSet[string(l)]++
	}
	for l, c := range newSet {
		if d := c - oldSet[l]; d > 0 {
			add += d
		}
	}
	for l, c := range oldSet {
		if d := c - newSet[l]; d > 0 {
			del += d
		}
	}
	_ = countLines
	return add, del
}

// ---------------------------------------------------------------------------
// Packfiles
// ---------------------------------------------------------------------------

func packObjectHeader(t byte, size int) []byte {
	code := t
	var out []byte
	first := byte((code << 4) | (byte(size) & 0x0f))
	size >>= 4
	for size > 0 {
		out = append(out, first|0x80)
		first = byte(size & 0x7f)
		size >>= 7
	}
	out = append(out, first)
	return out
}

func writePack(objs []*Obj) []byte {
	var buf bytes.Buffer
	hdr := make([]byte, 12)
	copy(hdr, "PACK")
	hdr[4] = 0
	hdr[5] = 0
	hdr[6] = 0
	hdr[7] = 2
	n := len(objs)
	hdr[8] = byte(n >> 24)
	hdr[9] = byte(n >> 16)
	hdr[10] = byte(n >> 8)
	hdr[11] = byte(n)
	buf.Write(hdr)
	for _, o := range objs {
		buf.Write(packObjectHeader(o.typ, len(o.data)))
		w, _ := zlib.NewWriterLevel(&buf, zlib.BestSpeed)
		w.Write(o.data)
		w.Close()
	}
	body := buf.Bytes()
	sum := sha1.Sum(body)
	return append(body, sum[:]...)
}

// inflateAt inflates one zlib stream starting at off (after the 2-byte zlib
// header is skipped internally) and returns the data and the offset just past
// the stream including its 4-byte Adler-32 trailer.
func inflateAt(raw []byte, off int) ([]byte, int, error) {
	if off+2 > len(raw) {
		return nil, 0, errors.New("pack truncated")
	}
	br := bytes.NewReader(raw[off+2:])
	fr := flate.NewReader(br)
	out, err := io.ReadAll(fr)
	fr.Close()
	if err != nil {
		return nil, 0, err
	}
	consumed := (len(raw) - off - 2) - br.Len()
	return out, off + 2 + consumed + 4, nil
}

func readPackVarint(buf []byte, off int) (size int, typ byte, next int, err error) {
	if off >= len(buf) {
		return 0, 0, 0, errors.New("pack truncated")
	}
	b := buf[off]
	typ = (b >> 4) & 0x07
	size = int(b & 0x0f)
	shift := uint(4)
	off++
	for b&0x80 != 0 {
		if off >= len(buf) {
			return 0, 0, 0, errors.New("pack truncated")
		}
		b = buf[off]
		off++
		size += int(b&0x7f) << shift
		shift += 7
	}
	return size, typ, off, nil
}

func readOfsBase(buf []byte, off int) (baseOff int, next int, err error) {
	if off >= len(buf) {
		return 0, 0, errors.New("pack truncated")
	}
	b := buf[off]
	off++
	value := int(b & 0x7f)
	for b&0x80 != 0 {
		if off >= len(buf) {
			return 0, 0, errors.New("pack truncated")
		}
		b = buf[off]
		off++
		value = ((value + 1) << 7) + int(b&0x7f)
	}
	return value, off, nil
}

func applyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	readSize := func() (int, error) {
		v := 0
		shift := uint(0)
		for {
			if off >= len(delta) {
				return 0, errors.New("delta truncated")
			}
			b := delta[off]
			off++
			v += int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				break
			}
		}
		return v, nil
	}
	srcSize, err := readSize()
	if err != nil {
		return nil, err
	}
	tgtSize, err := readSize()
	if err != nil {
		return nil, err
	}
	if srcSize != len(base) {
		return nil, errors.New("delta base size mismatch")
	}
	out := make([]byte, 0, tgtSize)
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			cpOff, cpSize := 0, 0
			for bit := 0; bit < 4; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta truncated")
					}
					cpOff |= int(delta[off]) << (uint(bit) * 8)
					off++
				}
			}
			for bit := 4; bit < 7; bit++ {
				if op&(1<<uint(bit)) != 0 {
					if off >= len(delta) {
						return nil, errors.New("delta truncated")
					}
					cpSize |= int(delta[off]) << (uint(bit-4) * 8)
					off++
				}
			}
			if cpSize == 0 {
				cpSize = 0x10000
			}
			if cpOff+cpSize > len(base) {
				return nil, errors.New("delta copy out of range")
			}
			out = append(out, base[cpOff:cpOff+cpSize]...)
		} else if op != 0 {
			lit := int(op & 0x7f)
			if off+lit > len(delta) {
				return nil, errors.New("delta truncated")
			}
			out = append(out, delta[off:off+lit]...)
			off += lit
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	if len(out) != tgtSize {
		return nil, errors.New("delta size mismatch")
	}
	return out, nil
}

type rawPackEntry struct {
	offset   int
	typ      byte // 1..4 plain, 6 ofs-delta, 7 ref-delta
	size     int
	data     []byte // inflated delta bytes or object bytes
	baseOff  int    // ofs-delta: absolute offset of base
	baseOID  string // ref-delta: base oid
	resolved []byte
	resTyp   byte
	resOID   string
	done     bool
}

// parsePack reads a packfile into objects (oid -> Obj). Deltas resolve
// against the pack itself and, for ref-deltas, against lookup.
func parsePack(pack []byte, lookup func(string) *Obj) (map[string]*Obj, error) {
	if len(pack) < 32 || string(pack[:4]) != "PACK" {
		return nil, errors.New("bad pack header")
	}
	ver := int(pack[4])<<24 | int(pack[5])<<16 | int(pack[6])<<8 | int(pack[7])
	if ver != 2 && ver != 3 {
		return nil, errors.New("bad pack version")
	}
	count := int(pack[8])<<24 | int(pack[9])<<16 | int(pack[10])<<8 | int(pack[11])
	trailerAt := len(pack) - 20
	sum := sha1.Sum(pack[:trailerAt])
	if !bytes.Equal(sum[:], pack[trailerAt:]) {
		return nil, errors.New("pack checksum mismatch")
	}
	var raw []*rawPackEntry
	byOff := map[int]*rawPackEntry{}
	off := 12
	for i := 0; i < count; i++ {
		entryOff := off
		size, typ, next, err := readPackVarint(pack, off)
		if err != nil {
			return nil, err
		}
		off = next
		e := &rawPackEntry{offset: entryOff, typ: typ, size: size}
		if typ == 6 {
			v, n, err := readOfsBase(pack, off)
			if err != nil {
				return nil, err
			}
			off = n
			e.baseOff = entryOff - v
		} else if typ == 7 {
			if off+20 > trailerAt {
				return nil, errors.New("pack truncated")
			}
			e.baseOID = hex.EncodeToString(pack[off : off+20])
			off += 20
		} else if typ < 1 || typ > 4 {
			return nil, errors.New("bad pack entry type")
		}
		data, n, err := inflateAt(pack, off)
		if err != nil {
			return nil, err
		}
		if n > trailerAt {
			return nil, errors.New("pack entry overruns trailer")
		}
		off = n
		if len(data) != size {
			return nil, errors.New("pack entry size mismatch")
		}
		e.data = data
		raw = append(raw, e)
		byOff[entryOff] = e
	}
	out := map[string]*Obj{}
	byOID := map[string]*rawPackEntry{}
	var resolve func(e *rawPackEntry) error
	resolving := map[int]bool{}
	resolve = func(e *rawPackEntry) error {
		if e.done {
			return nil
		}
		if resolving[e.offset] {
			return errors.New("delta cycle")
		}
		resolving[e.offset] = true
		defer delete(resolving, e.offset)
		switch e.typ {
		case 6:
			base := byOff[e.baseOff]
			if base == nil {
				return errors.New("ofs-delta base missing")
			}
			if err := resolve(base); err != nil {
				return err
			}
			data, err := applyDelta(base.resolved, e.data)
			if err != nil {
				return err
			}
			e.resolved = data
			e.resTyp = base.resTyp
		case 7:
			if b := byOID[e.baseOID]; b != nil {
				if err := resolve(b); err != nil {
					return err
				}
				data, err := applyDelta(b.resolved, e.data)
				if err != nil {
					return err
				}
				e.resolved = data
				e.resTyp = b.resTyp
			} else if lookup != nil {
				if o := lookup(e.baseOID); o != nil {
					data, err := applyDelta(o.data, e.data)
					if err != nil {
						return err
					}
					e.resolved = data
					e.resTyp = o.typ
				} else {
					return errors.New("ref-delta base missing")
				}
			} else {
				return errors.New("ref-delta base missing")
			}
		default:
			e.resolved = e.data
			e.resTyp = e.typ
		}
		e.resOID = hashObj(e.resTyp, e.resolved)
		e.done = true
		byOID[e.resOID] = e
		return nil
	}
	for _, e := range raw {
		if err := resolve(e); err != nil {
			return nil, err
		}
		if _, ok := out[e.resOID]; !ok {
			out[e.resOID] = &Obj{typ: e.resTyp, data: e.resolved}
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Fetch planning (what a clone/fetch delivers)
// ---------------------------------------------------------------------------

type fetchRequest struct {
	wants   []string
	haves   []string
	shallow []string // boundaries the client already holds
	depth   int      // -1 = no depth limit
}

type fetchPlan struct {
	objs      []*Obj
	shallow   []string
	unshallow []string
}

func computeFetch(objects map[string]*Obj, req fetchRequest) fetchPlan {
	shallowSet := map[string]bool{}
	for _, s := range req.shallow {
		shallowSet[s] = true
	}
	// Walk commits from wants with a generation limit.
	gen := map[string]int{}
	var queue []string
	var tagObjs []string
	for _, w := range req.wants {
		o := objects[w]
		if o == nil {
			continue
		}
		if o.typ == 4 {
			tagObjs = append(tagObjs, w)
			if c := peelToCommit(objects, w); c != "" {
				if _, ok := gen[c]; !ok {
					gen[c] = 0
					queue = append(queue, c)
				}
			}
		} else if o.typ == 1 {
			if _, ok := gen[w]; !ok {
				gen[w] = 0
				queue = append(queue, w)
			}
		}
	}
	// BFS by generation.
	for head := 0; head < len(queue); head++ {
		oid := queue[head]
		g := gen[oid]
		if req.depth >= 0 && g >= req.depth-1 {
			continue
		}
		o := objects[oid]
		if o == nil || o.typ != 1 {
			continue
		}
		parseCommitData(o)
		for _, p := range o.parents {
			if _, ok := gen[p]; !ok {
				gen[p] = g + 1
				queue = append(queue, p)
			}
		}
	}
	included := map[string]bool{}
	var includeTree func(treeOID string)
	includeTree = func(treeOID string) {
		o := objects[treeOID]
		if o == nil || o.typ != 2 || included[treeOID] {
			return
		}
		included[treeOID] = true
		for _, e := range parseTree(o.data) {
			if e.mode == "160000" {
				continue
			}
			if e.mode == "40000" {
				includeTree(e.oid)
			} else {
				included[e.oid] = true
			}
		}
	}
	for oid := range gen {
		included[oid] = true
		o := objects[oid]
		if o != nil && o.typ == 1 {
			parseCommitData(o)
			includeTree(o.tree)
		}
	}
	for _, t := range tagObjs {
		included[t] = true
	}
	// Exclusion: everything reachable from haves, stopping at shallow
	// boundaries (their parents are not held by the client).
	excluded := reachableObjectsStopAt(objects, req.haves, shallowSet)
	deliver := map[string]bool{}
	for oid := range included {
		if !excluded[oid] {
			deliver[oid] = true
		}
	}
	// Shallow boundary out: deepest included commits (gen == depth-1).
	var shallowOut []string
	if req.depth >= 0 {
		for oid, g := range gen {
			if g == req.depth-1 && deliver[oid] {
				shallowOut = append(shallowOut, oid)
			}
		}
		sort.Strings(shallowOut)
	}
	// Unshallow: previous boundaries whose parent is now delivered.
	deliveredCommits := map[string]bool{}
	for oid := range deliver {
		if o := objects[oid]; o != nil && o.typ == 1 {
			deliveredCommits[oid] = true
		}
	}
	var unshallowOut []string
	for _, s := range req.shallow {
		o := objects[s]
		if o == nil || o.typ != 1 {
			continue
		}
		parseCommitData(o)
		for _, p := range o.parents {
			if deliveredCommits[p] {
				unshallowOut = append(unshallowOut, s)
				break
			}
		}
	}
	sort.Strings(unshallowOut)
	oids := make([]string, 0, len(deliver))
	for oid := range deliver {
		oids = append(oids, oid)
	}
	sort.Strings(oids)
	objs := make([]*Obj, 0, len(oids))
	for _, oid := range oids {
		if o := objects[oid]; o != nil {
			objs = append(objs, o)
		}
	}
	return fetchPlan{objs: objs, shallow: shallowOut, unshallow: unshallowOut}
}
