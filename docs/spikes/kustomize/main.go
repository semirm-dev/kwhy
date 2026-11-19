// Spike: can leave-one-out builds attribute rendered Kustomize fields to the kustomization entry and
// file:line that sets them?
// usage: spike [-v] <root> <overlay dir relative to root>
//
// The tree under root is copied into an in-memory filesystem. The overlay is built once; then, for
// every entry of every kustomization.yaml in its stack (a patch, an image, a replicas item, a scalar
// like namePrefix, a generator literal...), it is built again with that one entry removed. Fields that
// change were last written by that entry. Fields no entry moves come from the resource file itself.
package main

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

const originKey = "config.kubernetes.io/origin"

var (
	listKeys   = []string{"patches", "patchesStrategicMerge", "patchesJson6902", "images", "replicas", "labels", "components", "configMapGenerator", "secretGenerator"}
	mapKeys    = []string{"commonLabels", "commonAnnotations"}
	scalarKeys = []string{"namespace", "namePrefix", "nameSuffix"}
	builds     int
	orig       = map[string][]byte{} // files as on disk; kustomizations in memory get re-marshalled
	buildTime  time.Duration
)

// kfile is one kustomization.yaml in the stack, parsed so entries can be cut out and the rest written back.
type kfile struct {
	path string // in-memory path, e.g. /overlays/prod/kustomization.yaml
	doc  *yaml.Node
}

// entry is one removable piece of a kustomization.
type entry struct {
	kf     *kfile
	parent *yaml.Node // the mapping or sequence holding it
	index  int        // position in parent.Content (key index for mappings)
	isMap  bool
	line   int
	label  string     // e.g. "patches[0] path=memory.yaml"
	patch  string     // in-memory path of a patch file, if the entry is one
	target *yaml.Node // the patch's `target:` selector, if any
}

type nameSel string // [name=x] path step

type field struct {
	res  string // original resource id, stable across namePrefix and namespace changes
	path string
}

type doc struct {
	name       string
	kind       string
	ordinal    int    // among documents of this kind from the same source
	origin     string // resource file, relative to the overlay dir
	configured string // generator's kustomization, relative to the overlay dir
	obj        map[string]any
}

func main() {
	verbose := flag.Bool("v", false, "print every field")
	flag.Parse()
	root, target := flag.Arg(0), "/"+strings.Trim(flag.Arg(1), "/")

	mem := filesys.MakeFsInMemory()
	copyTree(root, mem)

	stack := loadStack(mem, target)
	// Origin annotations name the resource file of every document.
	top := stack[0]
	m := top.doc.Content[0]
	m.Content = append(m.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "buildMetadata"},
		&yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Value: types.OriginAnnotations}}})
	write(mem, top)

	base, docs, err := build(mem, target)
	if err != nil {
		fmt.Fprintln(os.Stderr, "baseline build:", err)
		os.Exit(1)
	}

	var entries []*entry
	for _, kf := range stack {
		entries = append(entries, findEntries(mem, kf)...)
	}

	affects := map[field][]*entry{}
	addedBy := map[string]*entry{}
	failed := 0
	for _, e := range entries {
		removed := cut(e)
		write(mem, e.kf)
		got, gotDocs, err := build(mem, target)
		restore(e, removed)
		write(mem, e.kf)
		if err != nil {
			failed++
			if *verbose {
				fmt.Printf("  removing %s fails the build: %v\n", e.label, firstLine(err))
			}
			continue
		}
		for f, v := range base {
			if _, ok := gotDocs[f.res]; !ok {
				addedBy[f.res] = e
				continue
			}
			if w, ok := got[f]; !ok || !reflect.DeepEqual(v, w) {
				affects[f] = append(affects[f], e)
			}
		}
	}

	var fields []field
	for f := range base {
		fields = append(fields, f)
	}
	sort.Slice(fields, func(i, j int) bool {
		if fields[i].res != fields[j].res {
			return fields[i].res < fields[j].res
		}
		return fields[i].path < fields[j].path
	})

	var byEntry, byFile, located, checkable, lineHasValue, chained int
	var misses []string
	check := func(name, fpath string, val any, file string, line int) {
		if line == 0 || !scalar(val) {
			return
		}
		checkable++
		if lineMatches(mem, "/"+file, line, val) {
			lineHasValue++
		} else {
			misses = append(misses, fmt.Sprintf("%s %s = %v but %s:%d", name, fpath, val, file, line))
		}
	}
	for _, f := range fields {
		val, d := base[f], docs[f.res]
		es := affects[f]
		baseFile, baseLine := d.resourceLine(mem, target, f.path)
		var where []string
		if len(es) == 0 {
			byFile++
			if baseLine > 0 {
				located++
			}
			where = append(where, fmt.Sprintf("%s:%d", baseFile, baseLine))
			check(d.name, f.path, val, baseFile, baseLine)
		} else {
			byEntry++
			// A component brings its own entries; when one of those moved the field, the component is
			// only the route it came in by.
			if len(es) > 1 {
				es = slices.DeleteFunc(slices.Clone(es), func(e *entry) bool { return strings.HasPrefix(e.label, "components[") })
			}
			for _, e := range es {
				file, line := e.lineFor(mem, f.path, d.obj)
				where = append(where, fmt.Sprintf("%s:%d (%s)", file, line, e.label))
				if len(es) == 1 {
					check(d.name, f.path, val, file, line)
				}
			}
			// What the winner overrides: other patch files in the stack that set the same path, then the
			// resource file. A static lookup, no builds.
			for _, o := range entries {
				if o.patch == "" || slices.Contains(es, o) || !o.targets(d) {
					continue
				}
				if l := patchLine(mem, o.patch, f.path, d.obj); l > 0 {
					where = append(where, fmt.Sprintf("overrides %s:%d", strings.TrimPrefix(o.patch, "/"), l))
					chained++
				}
			}
			if baseLine > 0 {
				where = append(where, fmt.Sprintf("overrides %s:%d", baseFile, baseLine))
			}
		}
		if *verbose {
			fmt.Printf("%-28s %-62s %v\n    ← %s\n", d.name, f.path, val, strings.Join(where, "\n      "))
		}
	}

	fmt.Printf("\nresources added by an entry (component, generator): %d\n", len(addedBy))
	fmt.Printf("kustomizations %d, entries %d (removal fails build: %d), resources %d, fields %d\n", len(stack), len(entries), failed, len(docs), len(fields))
	fmt.Printf("fields moved by an entry %d, from the resource file %d (line found %d)\n", byEntry, byFile, located)
	fmt.Printf("attributed line holds the value: %d/%d; overridden patch lines found: %d\n", lineHasValue, checkable, chained)
	fmt.Printf("builds %d, total %s, per build %s\n", builds, buildTime.Round(time.Millisecond), (buildTime / time.Duration(max(builds, 1))).Round(time.Microsecond))
	if len(misses) > 0 {
		fmt.Printf("\nlines without the value (first 15 of %d):\n", len(misses))
		for _, m := range misses[:min(15, len(misses))] {
			fmt.Println("  " + m)
		}
	}
}

func copyTree(root string, mem filesys.FileSystem) {
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		orig["/"+filepath.ToSlash(rel)] = data
		return mem.WriteFile("/"+filepath.ToSlash(rel), data)
	})
	if err != nil {
		panic(err)
	}
}

// loadStack returns the kustomization in dir and every local one it pulls in, target first.
func loadStack(mem filesys.FileSystem, dir string) []*kfile {
	var out []*kfile
	seen := map[string]bool{}
	var visit func(dir string)
	visit = func(dir string) {
		if seen[dir] {
			return
		}
		seen[dir] = true
		var kpath string
		for _, n := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
			if mem.Exists(path.Join(dir, n)) {
				kpath = path.Join(dir, n)
				break
			}
		}
		if kpath == "" {
			return
		}
		data, _ := mem.ReadFile(kpath)
		var n yaml.Node
		if err := yaml.Unmarshal(data, &n); err != nil || len(n.Content) == 0 {
			return
		}
		kf := &kfile{path: kpath, doc: &n}
		out = append(out, kf)
		for _, key := range []string{"resources", "bases", "components"} {
			if s := get(n.Content[0], key); s != nil {
				for _, it := range s.Content {
					p := path.Join(dir, it.Value)
					if mem.IsDir(p) {
						visit(p)
					}
				}
			}
		}
	}
	visit(dir)
	return out
}

func findEntries(mem filesys.FileSystem, kf *kfile) []*entry {
	var out []*entry
	m := kf.doc.Content[0]
	dir := path.Dir(kf.path)
	for i := 0; i+1 < len(m.Content); i += 2 {
		key, val := m.Content[i].Value, m.Content[i+1]
		switch {
		case slices.Contains(scalarKeys, key):
			out = append(out, &entry{kf: kf, parent: m, index: i, isMap: true, line: val.Line, label: key})

		case slices.Contains(mapKeys, key):
			for j := 0; j+1 < len(val.Content); j += 2 {
				out = append(out, &entry{kf: kf, parent: val, index: j, isMap: true, line: val.Content[j].Line, label: key + "." + val.Content[j].Value})
			}

		case slices.Contains(listKeys, key):
			for j, it := range val.Content {
				e := &entry{kf: kf, parent: val, index: j, line: it.Line, label: fmt.Sprintf("%s[%d]", key, j)}
				if it.Kind == yaml.ScalarNode { // patchesStrategicMerge: a file name; components: a dir
					e.label += " " + it.Value
					if key == "patchesStrategicMerge" {
						e.patch = path.Join(dir, it.Value)
					}
				}
				e.target = get(it, "target")
				if p := get(it, "path"); p != nil {
					e.label += " path=" + p.Value
					e.patch = path.Join(dir, p.Value)
				}
				if n := get(it, "name"); n != nil {
					e.label += " name=" + n.Value
				}
				out = append(out, e)
				if lits := get(it, "literals"); lits != nil {
					for k, l := range lits.Content {
						out = append(out, &entry{kf: kf, parent: lits, index: k, line: l.Line, label: fmt.Sprintf("%s[%d].literals %s", key, j, l.Value)})
					}
				}
			}
		}
	}
	return out
}

// cut removes the entry from its parent and returns what it removed.
func cut(e *entry) []*yaml.Node {
	n := 1
	if e.isMap {
		n = 2
	}
	removed := slices.Clone(e.parent.Content[e.index : e.index+n])
	e.parent.Content = slices.Delete(e.parent.Content, e.index, e.index+n)
	return removed
}

func restore(e *entry, removed []*yaml.Node) {
	e.parent.Content = slices.Insert(e.parent.Content, e.index, removed...)
}

func write(mem filesys.FileSystem, kf *kfile) {
	data, err := yaml.Marshal(kf.doc)
	if err != nil {
		panic(err)
	}
	if err := mem.WriteFile(kf.path, data); err != nil {
		panic(err)
	}
}

func build(mem filesys.FileSystem, target string) (map[field]any, map[string]doc, error) {
	start := time.Now()
	k := krusty.MakeKustomizer(krusty.MakeDefaultOptions())
	rm, err := k.Run(mem, target)
	builds++
	buildTime += time.Since(start)
	if err != nil {
		return nil, nil, err
	}
	return flattenAll(rm)
}

func flattenAll(rm resmap.ResMap) (map[field]any, map[string]doc, error) {
	out := map[field]any{}
	docs := map[string]doc{}
	seen := map[string]int{}
	for _, r := range rm.Resources() {
		// OrgId is just the final id after a build (kustomize drops its rename bookkeeping), so name
		// documents by kind + origin + position among those, as kwhy's Helm side does (decision #8).
		raw := r.GetAnnotations()[originKey]
		k := r.GetKind() + " " + strings.ReplaceAll(strings.TrimSpace(raw), "\n", " ")
		id := fmt.Sprintf("%s #%d", k, seen[k])
		seen[k]++
		o, _ := r.GetOrigin()
		obj, err := r.Map()
		if err != nil {
			return nil, nil, err
		}
		if md, ok := obj["metadata"].(map[string]any); ok {
			if a, ok := md["annotations"].(map[string]any); ok {
				delete(a, originKey)
				if len(a) == 0 {
					delete(md, "annotations")
				}
			}
		}
		d := doc{name: r.GetKind() + "/" + r.GetName(), kind: r.GetKind(), ordinal: seen[k] - 1, obj: obj}
		if o != nil {
			d.origin, d.configured = o.Path, o.ConfiguredIn
		}
		docs[id] = d
		flatten(obj, "", func(p string, v any) { out[field{id, p}] = v })
	}
	return out, docs, nil
}

func flatten(n any, p string, fn func(string, any)) {
	switch t := n.(type) {
	case map[string]any:
		for k, v := range t {
			if strings.ContainsAny(k, `.[]"`) || k == "" {
				flatten(v, p+"["+strconv.Quote(k)+"]", fn)
			} else {
				flatten(v, p+"."+k, fn)
			}
		}
	case []any:
		for i, v := range t {
			// Items with a name (containers, env, ports) are keyed by it, as strategic merge does;
			// by index, one inserted item would make every later one look changed.
			if m, ok := v.(map[string]any); ok {
				if n, ok := m["name"].(string); ok && !strings.ContainsAny(n, "[]") {
					flatten(v, p+"[name="+n+"]", fn)
					continue
				}
			}
			flatten(v, p+"["+strconv.Itoa(i)+"]", fn)
		}
	case nil:
	default:
		fn(p, t)
	}
}

// findLine finds the line of a field path in a YAML file holding one or more documents. List items
// are matched by their `name` (as strategic merge patches do) when the rendered item has one, else
// by index. Documents are tried in turn; the first that has the path wins.
func findLine(mem filesys.FileSystem, file, fpath string, rendered map[string]any) int {
	return findLineKind(mem, file, fpath, rendered, "")
}

// findLineKind is findLine limited to documents of one kind ("" for any), for patch files.
func findLineKind(mem filesys.FileSystem, file, fpath string, rendered map[string]any, kind string) int {
	steps := parsePath(fpath)
	for _, d := range parsed(mem, file) {
		if k := get(d, "kind"); kind != "" && k != nil && k.Value != kind {
			continue
		}
		if l := walk(d, steps, rendered); l > 0 {
			return l
		}
	}
	return 0
}

var parseCache = map[string][]*yaml.Node{}

// parsed returns the documents of a file, parsed once.
func parsed(mem filesys.FileSystem, file string) []*yaml.Node {
	if ds, ok := parseCache[file]; ok {
		return ds
	}
	var ds []*yaml.Node
	if data, err := mem.ReadFile(file); err == nil {
		dec := yaml.NewDecoder(strings.NewReader(string(data)))
		for {
			var n yaml.Node
			if dec.Decode(&n) != nil {
				break
			}
			if len(n.Content) > 0 {
				ds = append(ds, n.Content[0])
			}
		}
	}
	parseCache[file] = ds
	return ds
}

func walk(n *yaml.Node, steps []any, rendered any) int {
	if len(steps) == 0 {
		return n.Line
	}
	switch s := steps[0].(type) {
	case string:
		c := get(n, s)
		if c == nil {
			return 0
		}
		var next any
		if m, ok := rendered.(map[string]any); ok {
			next = m[s]
		}
		return walk(c, steps[1:], next)
	case nameSel:
		if n.Kind != yaml.SequenceNode {
			return 0
		}
		var item any
		if l, ok := rendered.([]any); ok {
			for _, it := range l {
				if m, ok := it.(map[string]any); ok && m["name"] == string(s) {
					item = it
				}
			}
		}
		for _, it := range n.Content {
			if nm := get(it, "name"); nm != nil && nm.Value == string(s) {
				return walk(it, steps[1:], item)
			}
		}
		return 0
	case int:
		if n.Kind != yaml.SequenceNode {
			return 0
		}
		var item any
		if l, ok := rendered.([]any); ok && s < len(l) {
			item = l[s]
		}
		if m, ok := item.(map[string]any); ok && m["name"] != nil {
			for _, it := range n.Content {
				if nm := get(it, "name"); nm != nil && nm.Value == fmt.Sprint(m["name"]) {
					return walk(it, steps[1:], item)
				}
			}
			return 0
		}
		if s < len(n.Content) {
			return walk(n.Content[s], steps[1:], item)
		}
	}
	return 0
}

func parsePath(p string) []any {
	var out []any
	for p != "" {
		switch {
		case p[0] == '.':
			end := strings.IndexAny(p[1:], ".[") + 1
			if end == 0 {
				end = len(p)
			}
			out, p = append(out, p[1:end]), p[end:]
		case strings.HasPrefix(p, `["`):
			q, _ := strconv.QuotedPrefix(p[1:])
			k, _ := strconv.Unquote(q)
			out, p = append(out, k), p[len(q)+2:]
		default:
			end := strings.IndexByte(p, ']')
			if n, ok := strings.CutPrefix(p[1:end], "name="); ok {
				out, p = append(out, nameSel(n)), p[end+1:]
				continue
			}
			i, _ := strconv.Atoi(p[1:end])
			out, p = append(out, i), p[end+1:]
		}
	}
	return out
}

func get(m *yaml.Node, key string) *yaml.Node {
	if m == nil || m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

func scalar(v any) bool {
	switch v.(type) {
	case string, int, int64, float64, bool:
		return true
	}
	return false
}

func firstLine(err error) string {
	s, _, _ := strings.Cut(err.Error(), "\n")
	return s
}

// resourceLine is where the field sits in the document's own source: its resource file, or for a
// generated ConfigMap/Secret the `KEY=` literal in the generator's kustomization.
func (d doc) resourceLine(mem filesys.FileSystem, target, fpath string) (string, int) {
	if d.origin != "" {
		file := path.Join(target, d.origin)
		return strings.TrimPrefix(file, "/"), findLineDoc(mem, file, fpath, d.obj, d.kind, d.ordinal)
	}
	if d.configured == "" {
		return "?", 0
	}
	file := path.Join(target, d.configured)
	key, ok := strings.CutPrefix(fpath, ".data.")
	if !ok {
		return strings.TrimPrefix(file, "/"), 0
	}
	data, _ := mem.ReadFile(file)
	for i, l := range strings.Split(string(data), "\n") {
		if strings.Contains(l, key+"=") {
			return strings.TrimPrefix(file, "/"), i + 1
		}
	}
	return strings.TrimPrefix(file, "/"), 0
}

// lineFor is the line inside the entry that sets the field: the field in a patch file or an inline
// patch, `count` of a replicas item, `newTag`/`digest`/`newName` of an image, a scalar's value.
func (e *entry) lineFor(mem filesys.FileSystem, fpath string, obj map[string]any) (string, int) {
	kfile := strings.TrimPrefix(e.kf.path, "/")
	if e.patch != "" {
		if l := patchLine(mem, e.patch, fpath, obj); l > 0 {
			return strings.TrimPrefix(e.patch, "/"), l
		}
		return kfile, e.line
	}
	if e.isMap {
		return kfile, e.parent.Content[e.index+1].Line
	}
	it := e.parent.Content[e.index]
	if p := get(it, "patch"); p != nil {
		if l := inlineLine(p, fpath, obj); l > 0 {
			return kfile, l
		}
	}
	for _, k := range []string{"count", "newTag", "digest", "newName"} {
		if n := get(it, k); n != nil {
			return kfile, n.Line
		}
	}
	return kfile, e.line
}

// inlineLine finds the field in an inline patch: the `value` of a JSON6902 op whose path is the
// field, or the field in a strategic merge patch. Block scalar lines start after the `|` line.
func inlineLine(p *yaml.Node, fpath string, obj map[string]any) int {
	var n yaml.Node
	if yaml.Unmarshal([]byte(p.Value), &n) != nil || len(n.Content) == 0 {
		return 0
	}
	root := n.Content[0]
	if root.Kind == yaml.SequenceNode {
		if l := opLine(root, fpath, obj); l > 0 {
			return p.Line + l
		}
		return 0
	}
	if l := walk(root, parsePath(fpath), obj); l > 0 {
		return p.Line + l
	}
	return 0
}

// pointer turns .a.b[0]["c/d"] into the JSON pointer /a/b/0/c~1d.
func pointer(fpath string, obj any) string {
	var b strings.Builder
	cur := obj
	for _, s := range parsePath(fpath) {
		k := fmt.Sprint(s)
		switch st := s.(type) {
		case nameSel:
			l, _ := cur.([]any)
			for i, it := range l {
				if m, ok := it.(map[string]any); ok && m["name"] == string(st) {
					k, cur = strconv.Itoa(i), it
				}
			}
		case string:
			m, _ := cur.(map[string]any)
			cur = m[st]
		case int:
			if l, ok := cur.([]any); ok && st < len(l) {
				cur = l[st]
			}
		}
		b.WriteString("/" + strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1"))
	}
	return b.String()
}

// lineMatches reports whether the line shows the value, or shows a part of it (a namePrefix, an image tag).
func lineMatches(mem filesys.FileSystem, file string, line int, v any) bool {
	data, ok := orig[file]
	if !ok {
		return false
	}
	lines := strings.Split(string(data), "\n")
	if line-1 >= len(lines) {
		return false
	}
	l, want := lines[line-1], fmt.Sprint(v)
	if strings.Contains(l, want) {
		return true
	}
	// a block scalar (`key: |`) starts on the next line
	if first, _, multi := strings.Cut(want, "\n"); multi && line < len(lines) {
		return strings.Contains(lines[line], strings.TrimSpace(first))
	}
	_, after, ok := strings.Cut(l, ": ")
	if !ok {
		_, after, ok = strings.Cut(l, "=")
	}
	after = strings.Trim(strings.TrimSpace(after), `"'`)
	return ok && len(after) >= 2 && strings.Contains(want, after)
}

// patchLine finds the field in a patch file, among documents of the resource's kind. A patch's
// apiVersion, kind and metadata name/namespace select its target; they set nothing.
func patchLine(mem filesys.FileSystem, file, fpath string, obj map[string]any) int {
	switch fpath {
	case ".apiVersion", ".kind", ".metadata.name", ".metadata.namespace":
		return 0
	}
	if ds := parsed(mem, file); len(ds) == 1 && ds[0].Kind == yaml.SequenceNode {
		return opLine(ds[0], fpath, obj)
	}
	kind, _ := obj["kind"].(string)
	return findLineKind(mem, file, fpath, obj, kind)
}

// opLine finds the field in a JSON6902 patch: the op whose path is the field or a parent of it, then
// the field inside that op's value.
func opLine(ops *yaml.Node, fpath string, obj map[string]any) int {
	ptr := pointer(fpath, obj)
	steps := parsePath(fpath)
	for _, op := range ops.Content {
		pp := get(op, "path")
		if pp == nil || (pp.Value != ptr && !strings.HasPrefix(ptr, pp.Value+"/")) {
			continue
		}
		v := get(op, "value")
		if v == nil {
			return pp.Line
		}
		depth := strings.Count(pp.Value, "/")
		var sub any = obj
		for _, st := range steps[:depth] { // the rendered object at the op's path, for name lookups
			switch t := st.(type) {
			case string:
				m, _ := sub.(map[string]any)
				sub = m[t]
			case int:
				if l, ok := sub.([]any); ok && t < len(l) {
					sub = l[t]
				}
			}
		}
		if l := walk(v, steps[depth:], sub); l > 0 {
			return l
		}
		return v.Line
	}
	return 0
}

// findLineDoc finds the field in the n-th document of a kind in a file that may hold several.
func findLineDoc(mem filesys.FileSystem, file, fpath string, rendered map[string]any, kind string, n int) int {
	i := 0
	for _, d := range parsed(mem, file) {
		if k := get(d, "kind"); k == nil || k.Value != kind {
			continue
		}
		if i == n {
			return walk(d, parsePath(fpath), rendered)
		}
		i++
	}
	return 0
}

// targets reports whether the entry's `target:` selector can match the document. Names compare by
// suffix-free containment, since a namePrefix may have been added after the patch ran.
func (e *entry) targets(d doc) bool {
	if e.target == nil {
		return true
	}
	if k := get(e.target, "kind"); k != nil && k.Value != d.kind {
		return false
	}
	if n := get(e.target, "name"); n != nil {
		_, name, _ := strings.Cut(d.name, "/")
		return strings.Contains(name, n.Value)
	}
	return true
}
