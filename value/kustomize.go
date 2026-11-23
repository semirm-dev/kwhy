package value

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
	"golang.org/x/sync/errgroup"
	"sigs.k8s.io/kustomize/api/types"
	"sigs.k8s.io/kustomize/kyaml/filesys"

	"github.com/semirm-dev/kwhy/render"
)

// Kustomize attribution, ported from docs/spikes/kustomize (rules 1–5 of its README). Override chains (rule 6) are
// left out on purpose: they named wrong lines in the first attempt.

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

// LoadOverlay copies the overlay dir and the rest of its kustomization stack from root (the repo, or any directory
// holding the whole stack) into memory, builds it, and builds it again once per entry of every kustomization in the
// stack with that entry removed. Remote bases and URLs (render.Offline), helmCharts and plugins stop with an error
// naming them: kwhy stays offline and runs no code. cache is a directory that keeps the results between runs, keyed
// by every file copied; "" disables it.
func LoadOverlay(ctx context.Context, root, dir, cache string) (*Overlay, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}

	absDir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}

	rel, err := filepath.Rel(absRoot, absDir)
	if err != nil || !filepath.IsLocal(rel) {
		return nil, fmt.Errorf("overlay %s is outside %s", dir, root)
	}

	o := &Overlay{mem: filesys.MakeFsInMemory(), orig: map[string][]byte{}, root: absRoot, target: path.Join("/", filepath.ToSlash(rel)),
		parsed: map[string][]*yaml.Node{}}

	stack, err := o.loadStack(o.target)
	if err != nil {
		return nil, err
	}
	if len(stack) == 0 {
		return nil, fmt.Errorf("%s has no kustomization file", dir)
	}

	if err := render.Offline(o.mem, o.target); err != nil {
		return nil, err
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
	file := overlayCachePath(cache, o)
	if file != "" && read(file, &kc) == nil && kc.Entries == len(o.entries) && kc.Lines == len(o.lines) {
		o.restore(kc)
		return o, nil
	}

	if kc, err = o.attribute(ctx, base); err != nil {
		return nil, err
	}

	if file != "" {
		write(cache, file, kc)
	}

	return o, nil
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
			p.WriteString(keyStep(t))
		case int:
			l, _ := cur.([]any)
			cur = l[t]
			p.WriteString(itemStep(cur, t))
		}
	}

	return p.String(), nil
}

// Explain answers where field f was set: the innermost entry that moves it (rule 4), else the document's
// own file. A file:line is named only when that line shows the value (the guard); otherwise the entry's own line in
// its kustomization, which is always true, and the leaf says the field's line couldn't be confirmed.
func (o *Overlay) Explain(f KField) (Answer, error) {
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

// ExplainAll explains every field of document id, sorted by path.
func (o *Overlay) ExplainAll(id string) ([]Answer, error) {
	var fs []KField
	for f := range o.Fields {
		if f.Doc == id {
			fs = append(fs, f)
		}
	}
	sort.Slice(fs, func(i, j int) bool { return fs[i].Path < fs[j].Path })

	var as []Answer
	for _, f := range fs {
		a, err := o.Explain(f)
		if err != nil {
			return nil, err
		}
		as = append(as, a)
	}

	return as, nil
}

// restore takes the attribution from the cache file's contents kc.
func (o *Overlay) restore(kc kcached) {
	o.writes = kc.Writes
	for f, is := range kc.Affects {
		for _, i := range is {
			o.affects[f] = append(o.affects[f], o.entries[i])
		}
	}
	for id, i := range kc.AddedBy {
		o.addedBy[id] = o.entries[i]
	}
}

// attribute finds what moves each field of base: every entry, by a build without it (rules 2 and 3), and every
// value line of the stack, by a build with it edited. It returns what the cache keeps.
func (o *Overlay) attribute(ctx context.Context, base map[KField]any) (kcached, error) {
	var jobs []buildJob
	for i := range o.entries {
		jobs = append(jobs, buildJob{entry: i})
	}

	// String lines are edited together, each with its own mark, so the mark in a field names its line. Numbers and
	// booleans get a build each.
	var strs, others []int
	for j, l := range o.lines {
		if l.str {
			strs = append(strs, j)
		} else {
			others = append(others, j)
			jobs = append(jobs, o.edit([]int{j}))
		}
	}

	var pending [][]int
	if len(strs) > 0 {
		pending = [][]int{strs}
		jobs = append(jobs, o.edit(strs))
	}

	results, err := o.runBuilds(ctx, base, jobs)
	if err != nil {
		return kcached{}, err
	}

	n, m := len(o.entries), len(others)
	kc := o.creditEntries(base, results[:n])
	o.creditLines(others, results[n:n+m], results[:n])
	if err := o.creditStrings(ctx, base, pending, results[n+m:]); err != nil {
		return kcached{}, err
	}

	for _, ws := range o.writes {
		slices.Sort(ws)
	}
	kc.Lines, kc.Writes = len(o.lines), o.writes

	return kc, nil
}

// edit is a build with every line of set edited to carry its own mark.
func (o *Overlay) edit(set []int) buildJob {
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

	return buildJob{entry: -1, files: files}
}

// creditEntries records, from the leave-one-out results (one per entry), the entries that move each field and the
// entry each document came with (rule 3).
func (o *Overlay) creditEntries(base map[KField]any, results []buildResult) kcached {
	kc := kcached{Entries: len(o.entries), Affects: map[KField][]int{}, AddedBy: map[string]int{}}
	for i, r := range results {
		if !r.ok {
			continue
		}

		e := o.entries[i]
		for f := range base {
			if !r.ids[f.Doc] {
				o.addedBy[f.Doc], kc.AddedBy[f.Doc] = e, i
				continue
			}
			if _, ok := r.diff[f]; ok {
				o.affects[f], kc.Affects[f] = append(o.affects[f], e), append(kc.Affects[f], i)
			}
		}
	}

	return kc
}

// creditLines records the number and boolean lines whose own edit moves a field: results holds one build per line,
// entries the leave-one-out builds.
func (o *Overlay) creditLines(lines []int, results, entries []buildResult) {
	for k, j := range lines {
		r := results[k]
		if !r.ok {
			continue
		}

		// A field that ends up as it is without the line's whole entry moved because the entry stopped applying
		// (its target's name was edited), not because the line sets it.
		var without map[KField]any
		if e := o.lines[j].entry; e >= 0 && entries[e].ok {
			without = entries[e].diff
		}

		for f, w := range r.diff {
			if wo, ok := without[f]; !ok || !reflect.DeepEqual(w, wo) {
				o.writes[f] = append(o.writes[f], j)
			}
		}
	}
}

// creditStrings records the string lines whose mark shows in a field. pending are sets of lines edited together and
// results their builds; a set whose build fails or changes shape is split and built again.
func (o *Overlay) creditStrings(ctx context.Context, base map[KField]any, pending [][]int, results []buildResult) error {
	for {
		var next [][]int
		for k, set := range pending {
			r := results[k]
			if !r.ok {
				next = append(next, o.split(set)...)
				continue
			}

			for f, w := range r.diff {
				for _, j := range set {
					if s, ok := w.(string); ok && strings.Contains(s, lineMark(j)) {
						o.writes[f] = append(o.writes[f], j)
					}
				}
			}
		}

		if pending = next; len(pending) == 0 {
			return nil
		}

		jobs := make([]buildJob, len(pending))
		for i, set := range pending {
			jobs[i] = o.edit(set)
		}

		var err error
		if results, err = o.runBuilds(ctx, base, jobs); err != nil {
			return err
		}
	}
}

// split splits a set of lines whose build gave no answer: by file, else line by line. A single line is dropped.
func (o *Overlay) split(set []int) [][]int {
	byFile := map[string][]int{}
	for _, j := range set {
		byFile[o.lines[j].file] = append(byFile[o.lines[j].file], j)
	}

	var out [][]int
	if len(byFile) > 1 {
		for _, f := range slices.Sorted(maps.Keys(byFile)) {
			out = append(out, byFile[f])
		}
		return out
	}

	if len(set) > 1 {
		for _, j := range set {
			out = append(out, []int{j})
		}
	}

	return out
}

// runBuilds runs jobs in parallel, until ctx is done. Leave-one-out builds are independent, and so are the builds with stack lines
// edited, which find writers that leave-one-out can't see: an entry that sets the value the field already had. Each
// worker has its own copy of the files and its own parse of the stack, whose entries line up with o.entries by position.
func (o *Overlay) runBuilds(ctx context.Context, base map[KField]any, jobs []buildJob) ([]buildResult, error) {
	results := make([]buildResult, len(jobs))
	next := make(chan int)
	g := new(errgroup.Group)
	for range min(runtime.GOMAXPROCS(0), len(jobs)) {
		g.Go(func() error {
			// A worker that fails still drains, so the sender never blocks.
			defer func() {
				for range next {
				}
			}()
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
				if err := ctx.Err(); err != nil {
					return err
				}
				if results[i], err = w.runBuild(base, entries, jobs[i]); err != nil {
					return err
				}
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

// runBuild runs one job on w's files and puts them back after.
func (o *Overlay) runBuild(base map[KField]any, entries []*kentry, j buildJob) (buildResult, error) {
	var got map[KField]any
	var docs []KDoc
	var berr error
	if j.files != nil {
		saved := map[string][]byte{}
		for f, data := range j.files {
			var err error
			if saved[f], err = o.mem.ReadFile(f); err != nil {
				return buildResult{}, err
			}
			if err := o.mem.WriteFile(f, data); err != nil {
				return buildResult{}, err
			}
		}
		got, docs, berr = o.build()
		for f, data := range saved {
			if err := o.mem.WriteFile(f, data); err != nil {
				return buildResult{}, err
			}
		}
		if berr == nil && !sameKeys(base, got) {
			return buildResult{}, nil // the edit changes the shape: no answer
		}
	} else {
		e := entries[j.entry]
		removed := cut(e)
		if err := o.write(e.kf); err != nil {
			return buildResult{}, err
		}
		got, docs, berr = o.build()
		restore(e, removed)
		if err := o.write(e.kf); err != nil {
			return buildResult{}, err
		}
	}
	if berr != nil {
		return buildResult{}, nil // the build fails (a merge generator whose base is gone): no answer from this job
	}

	r := buildResult{diff: map[KField]any{}, ids: map[string]bool{}, ok: true}
	for _, d := range docs {
		r.ids[d.ID] = true
	}
	for f, v := range base {
		if g, ok := got[f]; !ok || !reflect.DeepEqual(v, g) {
			r.diff[f] = g
		}
	}

	return r, nil
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

// kline is one line that sets a value in a kustomization or patch; entry is its index in entries, -1 for none.
type kline struct {
	file  string
	line  int
	entry int
	str   bool
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

// buildJob is one build next to the base one: without one entry, or with files edited.
type buildJob struct {
	entry int               // remove this entry, or
	files map[string][]byte // build with these files edited
}

// buildResult keeps only the fields that differ from base (nil when gone): full maps of every build overwhelm the GC
// on large CRDs. ok is false when the build gave no answer.
type buildResult struct {
	diff map[KField]any
	ids  map[string]bool
	ok   bool
}

// kcached is what an overlay cache file holds: which entries move each field and add each document.
type kcached struct {
	Entries int
	Affects map[KField][]int
	AddedBy map[string]int
	Lines   int
	Writes  map[KField][]int
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
