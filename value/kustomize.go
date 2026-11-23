package value

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
	"golang.org/x/sync/errgroup"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/api/resmap"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// Kustomize attribution, ported from docs/spikes/kustomize (rules 1–5 of its README). Override chains (rule 6) are
// left out on purpose: they named wrong lines in the first attempt.

const originKey = "config.kubernetes.io/origin"

var (
	kListKeys   = []string{"patches", "patchesStrategicMerge", "patchesJson6902", "images", "replicas", "labels", "components", "configMapGenerator", "secretGenerator"}
	kMapKeys    = []string{"commonLabels", "commonAnnotations"}
	kScalarKeys = []string{"namespace", "namePrefix", "nameSuffix"}
)

// Overlay is a Kustomize overlay, copied into memory with its whole local stack, attributed field by field.
type Overlay struct {
	// Docs are the built documents, in build order, keyed like Fields.
	Docs []KDoc
	// Fields are the built scalars; paths key named list items by name (`containers[name=api]`).
	Fields map[KField]any

	mem     filesys.FileSystem
	orig    map[string][]byte // files as on disk, for the line guard
	root    string            // the directory copied, on disk
	target  string            // the overlay, in memory
	entries []*kentry
	affects map[KField][]*kentry
	addedBy map[string]*kentry
	parsed  map[string][]*yaml.Node
	top     string           // the overlay's kustomization, in memory
	lines   []kline          // value lines of the stack's kustomizations and patches
	writes  map[KField][]int // lines whose own edit moves the field, by index in lines
}

// kline is one line that sets a value in a kustomization or patch; entry is its index in entries, -1 for none.
type kline struct {
	file  string
	line  int
	entry int
	str   bool
}

// KField is one built scalar: the document id (kind, origin, ordinal) and its path.
type KField struct {
	Doc  string
	Path string
}

// KDoc is one built document.
type KDoc struct {
	ID, Kind, Name, Namespace string
	Object                    map[string]any

	ordinal    int
	origin     string
	configured string
}

type kfile struct {
	path string
	doc  *yaml.Node
}

type kentry struct {
	kf     *kfile
	parent *yaml.Node
	index  int
	isMap  bool
	line   int
	label  string
	patch  string
}

type nameSel string

// LoadOverlay copies root (the repo, or any directory holding the whole stack) into memory, builds the overlay
// dir in it, and builds it again once per entry of every kustomization in its stack with that entry removed.
// Remote bases, helmCharts and plugins stop with an error naming them: kwhy stays offline and runs no code.
// cache is a directory that keeps the leave-one-out results between runs, keyed by every file copied; "" disables it.
func LoadOverlay(root, dir, cache string) (*Overlay, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(absRoot, absDir)
	if err != nil || strings.HasPrefix(rel, "..") {
		return nil, fmt.Errorf("overlay %s is outside %s", dir, root)
	}

	o := &Overlay{mem: filesys.MakeFsInMemory(), orig: map[string][]byte{}, root: absRoot, target: path.Join("/", filepath.ToSlash(rel)),
		parsed: map[string][]*yaml.Node{}}
	if err := o.copyTree(); err != nil {
		return nil, err
	}

	stack, err := o.loadStack(o.target)
	if err != nil {
		return nil, err
	}
	if len(stack) == 0 {
		return nil, fmt.Errorf("%s has no kustomization file", dir)
	}

	// Origin annotations name the resource file of every document (rule 1).
	top := stack[0].doc.Content[0]
	top.Content = append(top.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: "buildMetadata"},
		&yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{{Kind: yaml.ScalarNode, Value: types.OriginAnnotations}}})
	if err := o.write(stack[0]); err != nil {
		return nil, err
	}
	o.top = stack[0].path

	base, docs, err := o.build()
	if err != nil {
		return nil, fmt.Errorf("build overlay: %w", err)
	}
	o.Fields, o.Docs = base, docs

	for _, kf := range stack {
		o.entries = append(o.entries, findEntries(kf)...)
	}

	o.lines = o.stackLines(stack)
	o.affects, o.addedBy, o.writes = map[KField][]*kentry{}, map[string]*kentry{}, map[KField][]int{}

	// The cache holds entries by position in o.entries, which a parse of the same files gives in the same order.
	var kc kcached
	path := overlayCachePath(cache, o)
	if path != "" && read(path, &kc) == nil && kc.Entries == len(o.entries) && kc.Lines == len(o.lines) {
		o.writes = kc.Writes
		for f, is := range kc.Affects {
			for _, i := range is {
				o.affects[f] = append(o.affects[f], o.entries[i])
			}
		}
		for id, i := range kc.AddedBy {
			o.addedBy[id] = o.entries[i]
		}
		return o, nil
	}

	// Leave-one-out builds are independent, and so are the builds with stack lines edited, which find writers that
	// leave-one-out can't see: an entry that sets the value the field already had. Each worker has its own copy of
	// the files and its own parse of the stack, whose entries line up with o.entries by position. A result keeps only
	// the fields that differ from base (nil when gone): full maps of every build overwhelm the GC on large CRDs.
	type job struct {
		entry int               // remove this entry, or
		files map[string][]byte // build with these files edited
	}
	type result struct {
		diff map[KField]any
		ids  map[string]bool
		ok   bool
	}
	run := func(jobs []job) ([]result, error) {
		results := make([]result, len(jobs))
		next := make(chan int)
		g := new(errgroup.Group)
		for range min(runtime.GOMAXPROCS(0), len(jobs)) {
			g.Go(func() error {
				w := &Overlay{mem: filesys.MakeFsInMemory(), target: o.target}
				if err := copyMem(o.mem, w.mem); err != nil {
					return err
				}
				stack, err := w.loadStack(w.target)
				if err != nil {
					return err
				}
				var entries []*kentry
				for _, kf := range stack {
					entries = append(entries, findEntries(kf)...)
				}

				for i := range next {
					var got map[KField]any
					var docs []KDoc
					var berr error
					if j := jobs[i]; j.files != nil {
						saved := map[string][]byte{}
						for f, data := range j.files {
							if saved[f], err = w.mem.ReadFile(f); err != nil {
								return err
							}
							if err := w.mem.WriteFile(f, data); err != nil {
								return err
							}
						}
						got, docs, berr = w.build()
						for f, data := range saved {
							if err := w.mem.WriteFile(f, data); err != nil {
								return err
							}
						}
						if berr == nil && !sameKeys(base, got) {
							continue // the edit changes the shape: no answer
						}
					} else {
						e := entries[j.entry]
						removed := cut(e)
						if err := w.write(e.kf); err != nil {
							return err
						}
						got, docs, berr = w.build()
						restore(e, removed)
						if err := w.write(e.kf); err != nil {
							return err
						}
					}
					if berr != nil {
						continue // the build fails (a merge generator whose base is gone): no answer from this job
					}
					r := result{diff: map[KField]any{}, ids: map[string]bool{}, ok: true}
					for _, d := range docs {
						r.ids[d.ID] = true
					}
					for f, v := range base {
						if w, ok := got[f]; !ok || !reflect.DeepEqual(v, w) {
							r.diff[f] = w
						}
					}
					results[i] = r
				}
				return nil
			})
		}
		for i := range jobs {
			next <- i
		}
		close(next)
		return results, g.Wait()
	}

	// String lines are edited together, each with its own mark, so the mark in a field names its line. A set whose
	// build fails or changes shape is split: by file, then line by line. Numbers and booleans get a build each.
	edit := func(set []int) job {
		files := map[string][]byte{}
		for _, j := range set {
			l := o.lines[j]
			if _, ok := files[l.file]; !ok {
				files[l.file] = o.orig[l.file]
			}
			files[l.file], _, _ = perturbLine(files[l.file], l.line, lineMark(j))
		}
		for f, data := range files {
			files[f] = o.forBuild(f, data)
		}
		return job{entry: -1, files: files}
	}
	var jobs []job
	for i := range o.entries {
		jobs = append(jobs, job{entry: i})
	}
	var strs, others []int
	for j, l := range o.lines {
		if l.str {
			strs = append(strs, j)
		} else {
			others = append(others, j)
			jobs = append(jobs, edit([]int{j}))
		}
	}
	var pending [][]int
	if len(strs) > 0 {
		pending = [][]int{strs}
		jobs = append(jobs, edit(strs))
	}
	results, err := run(jobs)
	if err != nil {
		return nil, err
	}

	kc = kcached{Entries: len(o.entries), Affects: map[KField][]int{}, AddedBy: map[string]int{},
		Lines: len(o.lines), Writes: o.writes}
	for i, r := range results[:len(o.entries)] {
		if !r.ok {
			continue
		}
		e := o.entries[i]
		for f := range base {
			if !r.ids[f.Doc] {
				o.addedBy[f.Doc], kc.AddedBy[f.Doc] = e, i // rule 3: the document came with the entry
				continue
			}
			if _, ok := r.diff[f]; ok {
				o.affects[f], kc.Affects[f] = append(o.affects[f], e), append(kc.Affects[f], i)
			}
		}
	}
	for k, j := range others {
		r := results[len(o.entries)+k]
		if !r.ok {
			continue
		}
		// A field that ends up as it is without the line's whole entry moved because the entry stopped applying
		// (its target's name was edited), not because the line sets it.
		var without map[KField]any
		if e := o.lines[j].entry; e >= 0 && results[e].ok {
			without = results[e].diff
		}
		for f, w := range r.diff {
			if wo, ok := without[f]; !ok || !reflect.DeepEqual(w, wo) {
				o.writes[f] = append(o.writes[f], j)
			}
		}
	}

	results = results[len(o.entries)+len(others):]
	for len(pending) > 0 {
		var next [][]int
		for k, set := range pending {
			if r := results[k]; r.ok {
				for f, w := range r.diff {
					for _, j := range set {
						if s, ok := w.(string); ok && strings.Contains(s, lineMark(j)) {
							o.writes[f] = append(o.writes[f], j)
						}
					}
				}
				continue
			}
			byFile := map[string][]int{}
			for _, j := range set {
				byFile[o.lines[j].file] = append(byFile[o.lines[j].file], j)
			}
			for _, f := range slices.Sorted(maps.Keys(byFile)) {
				if len(byFile) > 1 {
					next = append(next, byFile[f])
					continue
				}
				for _, j := range byFile[f] {
					if len(set) > 1 {
						next = append(next, []int{j})
					}
				}
			}
		}
		pending = next
		jobs = nil
		for _, set := range pending {
			jobs = append(jobs, edit(set))
		}
		if results, err = run(jobs); err != nil {
			return nil, err
		}
	}
	for _, ws := range o.writes {
		slices.Sort(ws)
	}
	if path != "" {
		write(cache, path, kc)
	}

	return o, nil
}

// stackLines lists the value lines of every kustomization in stack and every patch file its entries name.
func (o *Overlay) stackLines(stack []*kfile) []kline {
	files := map[string]int{} // file -> entry of a patch file, -1 for a kustomization
	paths := map[string]map[int]bool{}
	for _, kf := range stack {
		files[kf.path] = -1
		paths[kf.path] = map[int]bool{}
		for _, key := range []string{"resources", "bases", "components", "crds", "configurations", "patchesStrategicMerge"} {
			for _, it := range kget(kf.doc.Content[0], key).contentOrNil() {
				paths[kf.path][it.Line] = true
			}
		}
	}
	for i, e := range o.entries {
		if _, ok := files[e.patch]; e.patch != "" && !ok {
			files[e.patch] = i
		}
	}

	var out []kline
	for _, file := range slices.Sorted(maps.Keys(files)) {
		lines := strings.Split(string(o.orig[file]), "\n")
		for _, n := range valueLines(o.orig[file]) {
			// shortcut: file paths and these keys pick a target or a list item and never wrote a value on the public
			// overlays, so they cost a build for nothing; a patch that sets a `name:` field falls back to leave-one-out.
			if m := lineValue.FindStringSubmatch(lines[n-1]); paths[file][n] || slices.Contains(selectorKeys,
				strings.Trim(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(m[1]), "-")), `:"'`)) {
				continue
			}
			entry := files[file]
			if entry < 0 { // the last entry of this kustomization starting at or before the line
				for i, e := range o.entries {
					if e.kf.path == file && e.line <= n && (entry < 0 || e.line >= o.entries[entry].line) {
						entry = i
					}
				}
			}
			_, _, str := perturbLine(o.orig[file], n, "")
			out = append(out, kline{file, n, entry, str})
		}
	}

	return out
}

// forBuild adds to the overlay's own kustomization the buildMetadata LoadOverlay gives it.
func (o *Overlay) forBuild(file string, data []byte) []byte {
	if file == o.top {
		return append(data, "\nbuildMetadata: [originAnnotations]\n"...)
	}
	return data
}

// lineMark is the suffix line j's string gets in the shared build; "_" ends it, so line 1's isn't part of 12's.
func lineMark(j int) string { return "p9_" + strconv.Itoa(j) + "_" }

func sameKeys(a, b map[KField]any) bool {
	if len(a) != len(b) {
		return false
	}
	for f := range a {
		if _, ok := b[f]; !ok {
			return false
		}
	}
	return true
}

var selectorKeys = []string{"apiVersion", "kind", "name", "op", "path", "group", "version", "$patch"}

// lineValue matches a line that sets one scalar: "key: value" or "- value", with an optional trailing comment.
var lineValue = regexp.MustCompile(`^(\s*(?:-\s+)?(?:["']?[\w./$-]+["']?:\s+)?)(\S.*?)\s*(#.*)?$`)

// valueLines are the 1-based lines perturbLine can change.
func valueLines(data []byte) []int {
	var ls []int
	for i := range strings.Count(string(data), "\n") + 1 {
		if _, ok, _ := perturbLine(data, i+1, "p9"); ok {
			ls = append(ls, i+1)
		}
	}
	return ls
}

// perturbLine changes the scalar value line sets, as perturb does for Helm values: a string gets a suffix, an
// integer goes up by one, a boolean flips. Inline patches and block scalars are text, so their lines count too.
// A string gets mark as its suffix; str reports a string.
func perturbLine(data []byte, line int, mark string) (out []byte, ok, str bool) {
	lines := strings.Split(string(data), "\n")
	m := lineValue.FindStringSubmatch(lines[line-1])
	if m == nil || strings.TrimSpace(m[1]) == "" {
		return nil, false, false
	}
	v := m[2]
	str = true
	switch {
	case strings.ContainsAny(v[:1], "|>{[&*!") || strings.HasSuffix(v, ":"):
		return nil, false, false
	case len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0]:
		v = v[:len(v)-1] + mark + v[len(v)-1:]
	case v == "true" || v == "false":
		v, str = strconv.FormatBool(v == "false"), false
	default:
		if n, err := strconv.Atoi(v); err == nil {
			v, str = strconv.Itoa(n+1), false
		} else {
			v += mark
		}
	}
	lines[line-1] = m[1] + v
	return []byte(strings.Join(lines, "\n")), true, str
}

// kcached is what an overlay cache file holds: which entries move each field and add each document.
type kcached struct {
	Entries int
	Affects map[KField][]int
	AddedBy map[string]int
	Lines   int
	Writes  map[KField][]int
}

// overlayCachePath is the cache file for o's files and overlay, "" when cache is "" or no key can be made.
func overlayCachePath(cache string, o *Overlay) string {
	if cache == "" {
		return ""
	}
	exe, err := binaryID()
	if err != nil {
		return ""
	}

	h := sha256.New()
	fmt.Fprintf(h, "%s\x00%s", exe, o.target)
	for _, p := range slices.Sorted(maps.Keys(o.orig)) {
		fmt.Fprintf(h, "\x00%s\x00%d\x00", p, len(o.orig[p]))
		h.Write(o.orig[p])
	}

	return filepath.Join(cache, "kustomize-"+hex.EncodeToString(h.Sum(nil))+".gob")
}

// Select picks the document sel ("deploy/api", "Deployment/prod-api", "cm/api-config") names, by kind
// (case-insensitive or alias) and exact, suffix or contained name.
func (o *Overlay) Select(sel string) (KDoc, error) {
	kind, name, ok := strings.Cut(sel, "/")
	if !ok {
		return KDoc{}, fmt.Errorf("resource %s: want <kind>/<name>", sel)
	}
	kind = strings.ToLower(kind)
	if k, ok := kindAliases[kind]; ok {
		kind = k
	}

	// Exact name, else a prefixed one (namePrefix), else one with a generator's hash suffix too.
	var exact, suffix, contains []KDoc
	for _, d := range o.Docs {
		if !strings.EqualFold(d.Kind, kind) {
			continue
		}
		switch {
		case d.Name == name:
			exact = append(exact, d)
		case strings.HasSuffix(d.Name, name):
			suffix = append(suffix, d)
		case strings.Contains(d.Name, name):
			contains = append(contains, d)
		}
	}
	found := exact
	if len(found) == 0 {
		found = suffix
	}
	if len(found) == 0 {
		found = contains
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		var all []string
		for _, d := range o.Docs {
			all = append(all, d.Kind+"/"+d.Name)
		}
		slices.Sort(all)
		return KDoc{}, fmt.Errorf("no %s in the build; resources: %s", sel, strings.Join(all, ", "))
	}

	return KDoc{}, fmt.Errorf("%s matches %d resources: use the full name", sel, len(found))
}

// FieldPath turns a jq-like path on d (`[name=x]` or `[0]` for list items) into the overlay's field path.
func (o *Overlay) FieldPath(d KDoc, expr string) (string, error) {
	canon, err := ResolvePath(d.Object, expr)
	if err != nil {
		return "", err
	}

	var p strings.Builder
	var cur any = d.Object
	for _, s := range kparsePath(canon) {
		switch t := s.(type) {
		case string:
			m, _ := cur.(map[string]any)
			cur = m[t]
			if strings.ContainsAny(t, `.[]"`) || t == "" {
				p.WriteByte('[')
				p.WriteString(strconv.Quote(t))
				p.WriteByte(']')
			} else {
				p.WriteByte('.')
				p.WriteString(t)
			}
		case int:
			l, _ := cur.([]any)
			cur = l[t]
			if m, ok := cur.(map[string]any); ok {
				if n, ok := m["name"].(string); ok && !strings.ContainsAny(n, "[]") {
					p.WriteString("[name=")
					p.WriteString(n)
					p.WriteByte(']')
					continue
				}
			}
			p.WriteByte('[')
			p.WriteString(strconv.Itoa(t))
			p.WriteByte(']')
		}
	}

	return p.String(), nil
}

// ExplainOverlay answers where field f was set: the innermost entry that moves it (rule 4), else the document's
// own file. A file:line is named only when that line shows the value (the guard); otherwise the entry's own line in
// its kustomization, which is always true, and the leaf says the field's line couldn't be confirmed.
func (o *Overlay) ExplainOverlay(f KField) (Answer, error) {
	val, ok := o.Fields[f]
	if !ok {
		return Answer{}, fmt.Errorf("unknown field %s", f.Path)
	}
	d := o.doc(f.Doc)
	a := Answer{Resource: d.Kind + "/" + d.Name, Field: Field{Path: f.Path}, Value: val, Kind: Direct}
	if d.origin != "" {
		a.Template = o.disk(strings.TrimPrefix(path.Join(o.target, d.origin), "/"))
	}

	es := innermost(o.affects[f])

	switch {
	case len(es) == 1:
		file, line := es[0].lineFor(o, f.Path, d.Object)
		label := es[0].label
		if !o.lineShows(file, line, val) {
			file, line = strings.TrimPrefix(es[0].kf.path, "/"), es[0].line
			label += " (field's line not confirmed)"
		}
		a.Leaves = []Leaf{{Entry: label, Value: val, Chain: []Source{{File: o.disk(file), Line: line, Value: val}}}}
	case len(es) > 1:
		a.Kind = Composed // several entries move it together; no order is claimed
		for _, e := range es {
			a.Leaves = append(a.Leaves, Leaf{Entry: e.label, Value: val,
				Chain: []Source{{File: o.disk(strings.TrimPrefix(e.kf.path, "/")), Line: e.line, Value: val}}})
		}
	default:
		file, line := o.resourceLine(d, f.Path)
		if line == 0 || !o.lineShows(file, line, val) {
			a.Kind = Chart // in the resource file, line not confirmed
			a.Leaves = []Leaf{{Entry: "resource " + o.disk(file), Value: val, Chain: []Source{{File: o.disk(file), Value: val, Default: true}}}}
			if e := o.addedBy[f.Doc]; e != nil {
				a.Leaves[0].Entry = "resource " + o.disk(file) + ", added by " + e.label
			}
			break
		}
		a.Leaves = []Leaf{{Entry: "resource", Value: val, Chain: []Source{{File: o.disk(file), Line: line, Value: val, Default: true}}}}
	}

	// Lines whose own edit moves the field are ground truth: keep the answer only when it names some and only those.
	ws := o.writes[f]
	if len(ws) == 0 {
		return a, nil
	}
	named := 0
	for _, l := range a.Leaves {
		src := l.Chain[0]
		if src.Line == 0 || strings.HasSuffix(l.Entry, ")") {
			continue
		}
		if !slices.ContainsFunc(ws, func(j int) bool {
			return o.disk(strings.TrimPrefix(o.lines[j].file, "/")) == src.File && o.lines[j].line == src.Line
		}) {
			named = 0
			break
		}
		named++
	}
	if named > 0 {
		return a, nil
	}
	a.Kind, a.Leaves = Direct, nil
	if len(ws) > 1 {
		a.Kind = Composed
	}
	for _, j := range ws {
		l := o.lines[j]
		file := strings.TrimPrefix(l.file, "/")
		label := file
		if l.entry >= 0 {
			label = o.entries[l.entry].label
		}
		a.Leaves = append(a.Leaves, Leaf{Entry: label, Value: val, Chain: []Source{{File: o.disk(file), Line: l.line, Value: val}}})
	}

	return a, nil
}

// innermost keeps, of entries that all move a field, the inner ones (rule 4): a component's own patch rather than
// the component, a generator's literal rather than the generator.
func innermost(es []*kentry) []*kentry {
	if len(es) < 2 {
		return es
	}

	return slices.DeleteFunc(slices.Clone(es), func(e *kentry) bool {
		if strings.HasPrefix(e.label, "components[") {
			return true
		}
		outer, _, _ := strings.Cut(e.label, " ")
		return slices.ContainsFunc(es, func(in *kentry) bool { return in != e && in.kf == e.kf && strings.HasPrefix(in.label, outer+".") })
	})
}

// ExplainAllOverlay explains every field of document id, sorted by path.
func (o *Overlay) ExplainAllOverlay(id string) ([]Answer, error) {
	var fs []KField
	for f := range o.Fields {
		if f.Doc == id {
			fs = append(fs, f)
		}
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].Path < fs[j].Path })

	var as []Answer
	for _, f := range fs {
		a, err := o.ExplainOverlay(f)
		if err != nil {
			return nil, err
		}
		as = append(as, a)
	}

	return as, nil
}

func (o *Overlay) doc(id string) KDoc {
	for _, d := range o.Docs {
		if d.ID == id {
			return d
		}
	}

	return KDoc{}
}

// disk turns an in-memory path (relative, no leading slash) into a path relative to the working directory.
func (o *Overlay) disk(p string) string {
	abs := filepath.Join(o.root, filepath.FromSlash(p))
	if wd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(wd, abs); err == nil {
			return rel
		}
	}

	return abs
}

// copyMem copies every file of one in-memory filesystem into another.
func copyMem(from, to filesys.FileSystem) error {
	return from.Walk("/", func(p string, info fs.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, err := from.ReadFile(p)
		if err != nil {
			return err
		}
		return to.WriteFile(p, data)
	})
}

func (o *Overlay) copyTree() error {
	return filepath.WalkDir(o.root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() && d.Name() == ".git" {
			return filepath.SkipDir
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(o.root, p)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		mp := "/" + filepath.ToSlash(rel)
		if o.mem.WriteFile(mp, data) != nil {
			return nil // a name the in-memory filesystem refuses ("..file.txt"); Kustomize couldn't read it either
		}
		o.orig[mp] = data
		return nil
	})
}

// loadStack returns the kustomization in dir and every local one it pulls in, dir first.
func (o *Overlay) loadStack(dir string) ([]*kfile, error) {
	var out []*kfile
	seen := map[string]bool{}
	var visit func(dir string) error
	visit = func(dir string) error {
		if seen[dir] {
			return nil
		}
		seen[dir] = true

		var kpath string
		for _, n := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
			if o.mem.Exists(path.Join(dir, n)) {
				kpath = path.Join(dir, n)
				break
			}
		}
		if kpath == "" {
			return nil
		}
		data, err := o.mem.ReadFile(kpath)
		if err != nil {
			return err
		}
		var n yaml.Node
		if err := yaml.Unmarshal(data, &n); err != nil || len(n.Content) == 0 {
			return fmt.Errorf("read %s: %v", kpath, err)
		}
		m := n.Content[0]
		for _, key := range []string{"helmCharts", "generators", "transformers", "validators"} {
			if kget(m, key).Node != nil {
				return fmt.Errorf("%s uses %s, which kwhy doesn't run", strings.TrimPrefix(kpath, "/"), key)
			}
		}
		out = append(out, &kfile{path: kpath, doc: &n})

		for _, key := range []string{"resources", "bases", "components"} {
			for _, it := range kget(m, key).contentOrNil() {
				if strings.Contains(it.Value, "://") || strings.HasPrefix(it.Value, "github.com/") {
					return fmt.Errorf("%s: remote base %s isn't supported (kwhy stays offline)", strings.TrimPrefix(kpath, "/"), it.Value)
				}
				if p := path.Join(dir, it.Value); o.mem.IsDir(p) {
					if err := visit(p); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}

	return out, visit(dir)
}

type ynode struct{ *yaml.Node }

func kget(m *yaml.Node, key string) ynode {
	if m == nil || m.Kind != yaml.MappingNode {
		return ynode{}
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return ynode{m.Content[i+1]}
		}
	}

	return ynode{}
}

func (n ynode) contentOrNil() []*yaml.Node {
	if n.Node == nil {
		return nil
	}

	return n.Content
}

func findEntries(kf *kfile) []*kentry {
	var out []*kentry
	m := kf.doc.Content[0]
	dir := path.Dir(kf.path)
	for i := 0; i+1 < len(m.Content); i += 2 {
		key, val := m.Content[i].Value, m.Content[i+1]
		switch {
		case slices.Contains(kScalarKeys, key):
			out = append(out, &kentry{kf: kf, parent: m, index: i, isMap: true, line: val.Line, label: key})
		case slices.Contains(kMapKeys, key):
			for j := 0; j+1 < len(val.Content); j += 2 {
				out = append(out, &kentry{kf: kf, parent: val, index: j, isMap: true, line: val.Content[j].Line, label: key + "." + val.Content[j].Value})
			}
		case slices.Contains(kListKeys, key):
			for j, it := range val.Content {
				e := &kentry{kf: kf, parent: val, index: j, line: it.Line, label: fmt.Sprintf("%s[%d]", key, j)}
				if it.Kind == yaml.ScalarNode {
					e.label += " " + it.Value
					if key == "patchesStrategicMerge" {
						e.patch = path.Join(dir, it.Value)
					}
				}
				if p := kget(it, "path").Node; p != nil {
					e.label += " path=" + p.Value
					e.patch = path.Join(dir, p.Value)
				}
				if n := kget(it, "name").Node; n != nil {
					e.label += " name=" + n.Value
				}
				out = append(out, e)
				for k, l := range kget(it, "literals").contentOrNil() {
					out = append(out, &kentry{kf: kf, parent: kget(it, "literals").Node, index: k, line: l.Line, label: fmt.Sprintf("%s[%d].literals %s", key, j, l.Value)})
				}
			}
		}
	}

	return out
}

func cut(e *kentry) []*yaml.Node {
	n := 1
	if e.isMap {
		n = 2
	}
	removed := slices.Clone(e.parent.Content[e.index : e.index+n])
	e.parent.Content = slices.Delete(e.parent.Content, e.index, e.index+n)

	return removed
}

func restore(e *kentry, removed []*yaml.Node) {
	e.parent.Content = slices.Insert(e.parent.Content, e.index, removed...)
}

func (o *Overlay) write(kf *kfile) error {
	data, err := yaml.Marshal(kf.doc)
	if err != nil {
		return err
	}

	return o.mem.WriteFile(kf.path, data)
}

func (o *Overlay) build() (map[KField]any, []KDoc, error) {
	rm, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(o.mem, o.target)
	if err != nil {
		return nil, nil, err
	}

	return flattenAll(rm)
}

// flattenAll keys documents by kind + origin + position among those (rule 1) and lists by name (rule 2).
func flattenAll(rm resmap.ResMap) (map[KField]any, []KDoc, error) {
	out := map[KField]any{}
	var docs []KDoc
	seen := map[string]int{}
	for _, r := range rm.Resources() {
		raw := r.GetAnnotations()[originKey]
		k := r.GetKind() + " " + strings.ReplaceAll(strings.TrimSpace(raw), "\n", " ")
		id := fmt.Sprintf("%s #%d", k, seen[k])
		seen[k]++

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

		d := KDoc{ID: id, Kind: r.GetKind(), Name: r.GetName(), Namespace: r.GetNamespace(), Object: obj, ordinal: seen[k] - 1}
		if org, _ := r.GetOrigin(); org != nil {
			d.origin, d.configured = org.Path, org.ConfiguredIn
		}
		docs = append(docs, d)
		kflatten(obj, "", func(p string, v any) { out[KField{id, p}] = v })
	}

	return out, docs, nil
}

func kflatten(n any, p string, fn func(string, any)) {
	switch t := n.(type) {
	case map[string]any:
		for k, v := range t {
			if strings.ContainsAny(k, `.[]"`) || k == "" {
				kflatten(v, p+"["+strconv.Quote(k)+"]", fn)
			} else {
				kflatten(v, p+"."+k, fn)
			}
		}
	case []any:
		for i, v := range t {
			if m, ok := v.(map[string]any); ok {
				if n, ok := m["name"].(string); ok && !strings.ContainsAny(n, "[]") {
					kflatten(v, p+"[name="+n+"]", fn)
					continue
				}
			}
			kflatten(v, p+"["+strconv.Itoa(i)+"]", fn)
		}
	case nil:
	default:
		fn(p, t)
	}
}

// LineShows reports whether line of file (relative to the working directory) shows v, as the guard checks it.
func (o *Overlay) LineShows(file string, line int, v any) bool {
	abs, err := filepath.Abs(file)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(o.root, abs)
	if err != nil {
		return false
	}

	return o.lineShows(filepath.ToSlash(rel), line, v)
}

// lineShows is the guard: the line shows the value, or the part of it the line sets (a namePrefix, an image tag).
func (o *Overlay) lineShows(file string, line int, v any) bool {
	data, ok := o.orig["/"+file]
	if !ok || line < 1 {
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

func (o *Overlay) parsedFile(file string) []*yaml.Node {
	if ds, ok := o.parsed[file]; ok {
		return ds
	}
	var ds []*yaml.Node
	if data, err := o.mem.ReadFile(file); err == nil {
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
	o.parsed[file] = ds

	return ds
}

// resourceLine is where the field sits in the document's own source: its resource file, or for a generated
// ConfigMap/Secret the KEY= literal in the generator's kustomization.
func (o *Overlay) resourceLine(d KDoc, fpath string) (string, int) {
	if d.origin != "" {
		file := path.Join(o.target, d.origin)
		return strings.TrimPrefix(file, "/"), o.findLineDoc(file, fpath, d.Object, d.Kind, d.ordinal)
	}
	if d.configured == "" {
		return strings.TrimPrefix(o.target, "/"), 0
	}

	file := path.Join(o.target, d.configured)
	key, ok := strings.CutPrefix(fpath, ".data.")
	if !ok {
		return strings.TrimPrefix(file, "/"), 0
	}
	data, _ := o.mem.ReadFile(file)
	for i, l := range strings.Split(string(data), "\n") {
		if strings.Contains(l, key+"=") {
			return strings.TrimPrefix(file, "/"), i + 1
		}
	}

	return strings.TrimPrefix(file, "/"), 0
}

func (o *Overlay) findLineDoc(file, fpath string, rendered map[string]any, kind string, n int) int {
	i := 0
	for _, d := range o.parsedFile(file) {
		if k := kget(d, "kind").Node; k == nil || k.Value != kind {
			continue
		}
		if i == n {
			return kwalk(d, kparsePath(fpath), rendered)
		}
		i++
	}

	return 0
}

// lineFor is the line inside the entry that sets the field.
func (e *kentry) lineFor(o *Overlay, fpath string, obj map[string]any) (string, int) {
	kf := strings.TrimPrefix(e.kf.path, "/")
	if e.patch != "" {
		if l := o.patchLine(e.patch, fpath, obj); l > 0 {
			return strings.TrimPrefix(e.patch, "/"), l
		}
		return kf, e.line
	}
	if e.isMap {
		return kf, e.parent.Content[e.index+1].Line
	}

	it := e.parent.Content[e.index]
	if p := kget(it, "patch").Node; p != nil {
		if l := inlineLine(p, fpath, obj); l > 0 {
			return kf, l
		}
	}
	for _, k := range []string{"count", "newTag", "digest", "newName"} {
		if n := kget(it, k).Node; n != nil {
			return kf, n.Line
		}
	}

	return kf, e.line
}

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
	if l := kwalk(root, kparsePath(fpath), obj); l > 0 {
		return p.Line + l
	}

	return 0
}

func (o *Overlay) patchLine(file, fpath string, obj map[string]any) int {
	switch fpath {
	case ".apiVersion", ".kind", ".metadata.name", ".metadata.namespace":
		return 0
	}
	if ds := o.parsedFile(file); len(ds) == 1 && ds[0].Kind == yaml.SequenceNode {
		return opLine(ds[0], fpath, obj)
	}

	kind, _ := obj["kind"].(string)
	steps := kparsePath(fpath)
	for _, d := range o.parsedFile(file) {
		if k := kget(d, "kind").Node; kind != "" && k != nil && k.Value != kind {
			continue
		}
		if l := kwalk(d, steps, obj); l > 0 {
			return l
		}
	}

	return 0
}

func opLine(ops *yaml.Node, fpath string, obj map[string]any) int {
	ptr := pointer(fpath, obj)
	steps := kparsePath(fpath)
	for _, op := range ops.Content {
		pp := kget(op, "path").Node
		if pp == nil || (pp.Value != ptr && !strings.HasPrefix(ptr, pp.Value+"/")) {
			continue
		}
		v := kget(op, "value").Node
		if v == nil {
			return pp.Line
		}
		depth := strings.Count(pp.Value, "/")
		var sub any = obj
		for _, st := range steps[:min(depth, len(steps))] {
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
		if depth <= len(steps) {
			if l := kwalk(v, steps[depth:], sub); l > 0 {
				return l
			}
		}
		return v.Line
	}

	return 0
}

func pointer(fpath string, obj any) string {
	var b strings.Builder
	cur := obj
	for _, s := range kparsePath(fpath) {
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
		b.WriteByte('/')
		b.WriteString(strings.ReplaceAll(strings.ReplaceAll(k, "~", "~0"), "/", "~1"))
	}

	return b.String()
}

func kwalk(n *yaml.Node, steps []any, rendered any) int {
	if len(steps) == 0 {
		return n.Line
	}
	switch s := steps[0].(type) {
	case string:
		c := kget(n, s).Node
		if c == nil {
			return 0
		}
		var next any
		if m, ok := rendered.(map[string]any); ok {
			next = m[s]
		}
		return kwalk(c, steps[1:], next)
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
			if nm := kget(it, "name").Node; nm != nil && nm.Value == string(s) {
				return kwalk(it, steps[1:], item)
			}
		}
	case int:
		if n.Kind != yaml.SequenceNode || s >= len(n.Content) {
			return 0
		}
		var item any
		if l, ok := rendered.([]any); ok && s < len(l) {
			item = l[s]
		}
		return kwalk(n.Content[s], steps[1:], item)
	}

	return 0
}

// kparsePath splits an overlay field path: .key, ["key"], [name=x], [n].
func kparsePath(p string) []any {
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
			if end < 0 {
				return out
			}
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
