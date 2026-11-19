// Package value traces fields of rendered Kubernetes config back to the values line that sets them.
package value

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"

	"go.yaml.in/yaml/v3"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/cli/values"
	"helm.sh/helm/v4/pkg/getter"
	"helm.sh/helm/v4/pkg/strvals"

	"github.com/semirm-dev/kwhy/render"
)

const (
	Direct     Kind = "direct"     // the field equals one values leaf
	Composed   Kind = "composed"   // leaves plus template text
	Derived    Kind = "derived"    // template logic computes it from leaves
	Encoded    Kind = "encoded"    // a leaf passed through b64enc
	Hashed     Kind = "hashed"     // checksum of other content
	Chart      Kind = "chart"      // constant in the chart
	Structural Kind = "structural" // a leaf decides whether the field exists
	Random     Kind = "random"     // differs between two identical renders

	chunkSize = 40 // how many leaves one tracing render marks
)

var (
	keyEscaper = strings.NewReplacer(".", `\.`, "[", `\[`)

	kindAliases = map[string]string{
		"deploy": "deployment", "sts": "statefulset", "ds": "daemonset",
		"svc": "service", "cm": "configmap", "ing": "ingress",
	}

	markerRe = regexp.MustCompile(`zq(\d+)qz|\d+`)
)

// Values are the merged values of one render, with where every leaf comes from.
type Values struct {
	User   map[string]any // what Helm receives (files + --set merged, Helm semantics)
	Leaves []Leaf         // every scalar leaf of the coalesced values, sorted by path
}

// LoadValues merges chart defaults, values files and --set flags as Helm does, and records the
// override chain of every leaf.
func LoadValues(c *render.Chart, files, sets []string) (*Values, error) {
	opts := values.Options{ValueFiles: files, Values: sets}

	// Helm's own merge, so User never disagrees with Helm.
	user, err := opts.MergeValues(getter.All(cli.New()))
	if err != nil {
		return nil, fmt.Errorf("merge values: %w", err)
	}

	final, err := c.Coalesce(user)
	if err != nil {
		return nil, err
	}

	layers, subcharts, err := loadLayers(c, user, files, sets)
	if err != nil {
		return nil, err
	}

	v := &Values{User: user}
	walk(final, nil, func(path []any, val any) {
		if !isGlobalCopy(path, subcharts) {
			v.Leaves = append(v.Leaves, Leaf{Path: path, Value: val, Chain: chain(layers, path)})
		}
	})

	slices.SortFunc(v.Leaves, func(a, b Leaf) int { return strings.Compare(PathString(a.Path), PathString(b.Path)) })
	for i := range v.Leaves {
		v.Leaves[i].ID = i
	}

	return v, nil
}

// Lookup returns the leaf at path.
func (v *Values) Lookup(path []any) (Leaf, bool) {
	key := PathString(path)
	for _, l := range v.Leaves {
		if PathString(l.Path) == key {
			return l, true
		}
	}

	return Leaf{}, false
}

// Leaf is one scalar of the values Helm renders with.
type Leaf struct {
	ID    int      `json:"-"`
	Path  []any    `json:"path"`  // string keys and int list indexes, e.g. {"resources", "requests", "memory"}
	Value any      `json:"value"` // winning value
	Chain []Source `json:"chain"` // Chain[0] is the winner, then what it overrode, ending at the chart default
}

// Source is one place that sets a values leaf.
type Source struct {
	File  string `json:"file"` // values file as the user gave it, a chart file like "mini/values.yaml", or "--set k=v"
	Line  int    `json:"line"` // 1-based line; for --set the flag index
	Value any    `json:"value"`
}

// Field is one scalar of a rendered document.
type Field struct {
	Doc  int    `json:"-"`    // index into the rendered docs
	Path string `json:"path"` // canonical, e.g. ".spec.template.spec.containers[0].image"
}

// Index is what marker tracing learned about one render.
type Index struct {
	Docs       []render.Doc  // baseline render
	Fields     map[Field]any // baseline values
	Hits       map[Field][]Hit
	Random     map[Field]bool // differs between two baseline renders
	Unmarkable map[int]string // Leaf.ID → "render error" | "changes structure"

	coalesced map[string]any // the values Helm rendered the baseline with
}

// Trace renders with the values replaced by unique markers, chunk by chunk, and records which
// leaves show up in which fields. Only non-empty strings and non-zero numbers are marked; bools
// and empty values steer control flow and are left to Explain.
func Trace(ctx context.Context, c *render.Chart, v *Values, o render.Options) (*Index, error) {
	docs, err := c.Render(ctx, v.User, o)
	if err != nil {
		return nil, err
	}

	again, err := c.Render(ctx, v.User, o)
	if err != nil {
		return nil, err
	}

	coalesced, err := c.Coalesce(v.User)
	if err != nil {
		return nil, err
	}

	idx := &Index{Docs: docs, Fields: Flatten(docs), Hits: map[Field][]Hit{}, Random: map[Field]bool{}, Unmarkable: map[int]string{}, coalesced: coalesced}
	second := Flatten(again)
	for f, val := range idx.Fields {
		if other, ok := second[f]; !ok || other != val {
			idx.Random[f] = true
		}
	}

	base := markerBase(v.Leaves)
	var marked []Leaf
	for _, l := range v.Leaves {
		if s, ok := l.Value.(string); ok && s != "" {
			marked = append(marked, l)
		} else if n, ok := whole(l.Value); ok && n != 0 && base+len(v.Leaves) < 1e6 {
			marked = append(marked, l)
		}
	}

	t := tracer{c: c, o: o, user: v.User, idx: idx, base: base}
	// shortcut: chunks render one after another; run them in parallel if Task 7's 2s gate needs it.
	for chunk := range slices.Chunk(marked, chunkSize) {
		if err := t.mark(ctx, chunk); err != nil {
			return nil, err
		}
	}

	return idx, nil
}

// Hit says a leaf's marker shows up in a rendered field.
type Hit struct {
	Leaf    int  // Leaf.ID
	Exact   bool // the field equals the marker
	Encoded bool // found after base64 decoding
}

// Answer explains one rendered field.
type Answer struct {
	Resource string `json:"resource"` // "Deployment/rel-api"
	Field    Field  `json:"field"`
	Value    any    `json:"value"` // baseline rendered value
	Kind     Kind   `json:"kind"`
	Leaves   []Leaf `json:"leaves"`   // contributing leaves, each with its override chain
	Template string `json:"template"` // template the field's document comes from
}

// Kind says how a rendered field relates to the values that produce it.
// The strings are part of the kwhy.value/v1 JSON contract.
type Kind string

// layer is one values source, in precedence order, with the line of every leaf it sets.
type layer struct {
	file  string
	mount []any // where the tree sits in the coalesced values; set for subchart defaults
	tree  map[string]any
	lines map[string]int // PathString relative to mount → line; nil for --set
	line  int            // --set flag index
}

func fileLayer(name string, data []byte) (layer, error) {
	// Helm's reader, so values have the same types as in the coalesced values.
	tree, err := loader.LoadValues(bytes.NewReader(data))
	if err != nil {
		return layer{}, fmt.Errorf("parse %s: %w", name, err)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return layer{}, fmt.Errorf("parse %s: %w", name, err)
	}

	l := layer{file: name, tree: tree, lines: map[string]int{}}
	lines(&doc, nil, l.lines)

	return l, nil
}

type tracer struct {
	c    *render.Chart
	o    render.Options
	user map[string]any
	idx  *Index
	base int
}

// mark renders once with every leaf of chunk marked. A render error or a changed set of fields
// means a marker steered the templates: split the chunk until the leaf responsible is alone.
func (t *tracer) mark(ctx context.Context, chunk []Leaf) error {
	markers := map[int]string{}
	sparse := map[string]any{}
	for _, l := range chunk {
		var m any = t.base + l.ID
		if _, ok := l.Value.(string); ok {
			m = fmt.Sprintf("zq%dqz", l.ID)
		}
		markers[l.ID] = fmt.Sprint(m)
		setSparse(sparse, t.idx.coalesced, l.Path, m)
	}

	docs, err := t.c.Render(ctx, merge(deepCopy(t.user), sparse).(map[string]any), t.o)
	if ctx.Err() != nil {
		return ctx.Err()
	}

	fields := Flatten(docs)
	reason := ""
	if err != nil {
		reason = "render error"
	} else if !sameShape(t.idx.Docs, t.idx.Fields, docs, fields) {
		reason = "changes structure"
	}

	if reason != "" {
		if len(chunk) == 1 {
			t.idx.Unmarkable[chunk[0].ID] = reason
			return nil
		}

		if err := t.mark(ctx, chunk[:len(chunk)/2]); err != nil {
			return err
		}

		return t.mark(ctx, chunk[len(chunk)/2:])
	}

	for f, val := range fields {
		if t.idx.Random[f] {
			continue
		}

		s := fmt.Sprint(val)
		if n, ok := whole(val); ok {
			s = strconv.Itoa(n)
		}

		hits := find(s, markers, false)
		if str, ok := val.(string); ok {
			if d, err := base64.StdEncoding.DecodeString(str); err == nil {
				hits = append(hits, find(string(d), markers, true)...)
			}
		}

		for _, h := range hits {
			if !slices.Contains(t.idx.Hits[f], h) {
				t.idx.Hits[f] = append(t.idx.Hits[f], h)
			}
		}
	}

	return nil
}

type perturber struct {
	c     *render.Chart
	o     render.Options
	idx   *Index
	user  map[string]any
	field Field
}

// find bisects set down to the leaves whose perturbation moves the field. A leaf whose own
// perturbation fails to render is skipped.
func (p *perturber) find(ctx context.Context, set []Leaf) ([]found, error) {
	if len(set) == 0 {
		return nil, nil
	}

	moves, removed, failed, err := p.test(ctx, set)
	if err != nil || !moves {
		return nil, err
	}

	if len(set) == 1 {
		if failed {
			return nil, nil
		}
		return []found{{leaf: set[0], removed: removed}}, nil
	}

	left, err := p.find(ctx, set[:len(set)/2])
	if err != nil {
		return nil, err
	}

	right, err := p.find(ctx, set[len(set)/2:])
	if err != nil {
		return nil, err
	}

	return append(left, right...), nil
}

// test renders twice with every leaf of set perturbed: nudged (string+"p9", number+1) and zeroed
// ("", 0), which catches thresholds like `gt .replicas 1` that a nudge misses. Bools flip in both.
// A render error counts as moving, so bisection can isolate it.
func (p *perturber) test(ctx context.Context, set []Leaf) (moves, removed, failed bool, err error) {
	for _, zero := range []bool{false, true} {
		sparse := map[string]any{}
		for _, l := range set {
			setSparse(sparse, p.idx.coalesced, l.Path, perturb(l.Value, zero))
		}

		docs, rerr := p.c.Render(ctx, merge(deepCopy(p.user), sparse).(map[string]any), p.o)
		if ctx.Err() != nil {
			return false, false, false, ctx.Err()
		}
		if rerr != nil {
			moves, failed = true, true
			continue
		}

		val, ok := p.lookup(docs)
		if !ok {
			moves, removed = true, true
		} else if val != p.idx.Fields[p.field] {
			moves = true
		}
	}

	return moves, removed, failed, nil
}

// lookup finds the field in docs by its document's kind, name and namespace, since document
// indexes shift when documents come and go.
func (p *perturber) lookup(docs []render.Doc) (any, bool) {
	base := p.idx.Docs[p.field.Doc]
	for _, d := range docs {
		if d.Kind == base.Kind && d.Name == base.Name && d.Namespace == base.Namespace {
			val, ok := Flatten([]render.Doc{d})[Field{Path: p.field.Path}]
			return val, ok
		}
	}

	return nil, false
}

type found struct {
	leaf    Leaf
	removed bool // perturbing the leaf removes the field
}

// PathString formats a values path like --set does: "resources.requests.memory", "list[0].x", `a\.b`.
func PathString(path []any) string {
	var b strings.Builder
	for _, p := range path {
		if i, ok := p.(int); ok {
			fmt.Fprintf(&b, "[%d]", i)
			continue
		}

		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(keyEscaper.Replace(fmt.Sprint(p)))
	}

	return b.String()
}

// Flatten returns every non-null scalar of docs.
func Flatten(docs []render.Doc) map[Field]any {
	out := map[Field]any{}
	for i, d := range docs {
		walk(d.Object, nil, func(path []any, val any) {
			out[Field{Doc: i, Path: fieldPath(path)}] = val
		})
	}

	return out
}

// ResolvePath resolves a jq-like path to the canonical path of a scalar in obj. Besides .key and
// [n] it accepts ["key"] and [name=x], which picks the list item whose name is x.
func ResolvePath(obj map[string]any, expr string) (string, error) {
	var cur any = obj
	var path []any
	for rest := expr; rest != ""; {
		var step any
		switch {
		case rest[0] == '.':
			end := strings.IndexAny(rest[1:], ".[") + 1
			if end == 0 {
				end = len(rest)
			}
			step, rest = rest[1:end], rest[end:]

		case strings.HasPrefix(rest, `["`):
			q, err := strconv.QuotedPrefix(rest[1:])
			if err != nil || !strings.HasPrefix(rest[1+len(q):], "]") {
				return "", fmt.Errorf("bad path %s", expr)
			}
			step, _ = strconv.Unquote(q)
			rest = rest[len(q)+2:]

		case rest[0] == '[':
			end := strings.IndexByte(rest, ']')
			if end < 0 {
				return "", fmt.Errorf("bad path %s", expr)
			}
			sel := rest[1:end]
			rest = rest[end+1:]

			if i, err := strconv.Atoi(sel); err == nil {
				step = i
			} else if k, v, ok := strings.Cut(sel, "="); ok {
				step = findItem(cur, k, v)
			} else {
				return "", fmt.Errorf("bad path %s", expr)
			}

		default:
			return "", fmt.Errorf("bad path %s", expr)
		}

		var ok bool
		switch s := step.(type) {
		case string:
			var m map[string]any
			if m, ok = cur.(map[string]any); ok {
				cur, ok = m[s]
			}

		case int:
			var l []any
			if l, ok = cur.([]any); ok && s >= 0 && s < len(l) {
				cur = l[s]
			} else {
				ok = false
			}
		}
		if !ok {
			return "", fmt.Errorf("unknown path %s", expr)
		}

		path = append(path, step)
	}

	switch cur.(type) {
	case map[string]any, []any, nil:
		return "", fmt.Errorf("%s is not a scalar field", expr)
	}

	return fieldPath(path), nil
}

// Suggest returns up to n of valid, closest to bad first.
func Suggest(valid []string, bad string, n int) []string {
	dist := map[string]int{}
	for _, s := range valid {
		dist[s] = levenshtein(s, bad)
	}

	out := slices.Clone(valid)
	slices.SortFunc(out, func(a, b string) int {
		if c := dist[a] - dist[b]; c != 0 {
			return c
		}
		return strings.Compare(a, b)
	})

	return out[:min(n, len(out))]
}

// SelectDoc picks the document sel ("deploy/api", "Deployment/rel-api") names. Kinds match
// case-insensitively or by alias; names match exactly, or as the release-prefixed form ("api"
// matches "rel-api") when no name matches exactly. An empty namespace matches any.
func SelectDoc(docs []render.Doc, sel, namespace string) (int, error) {
	kind, name, ok := strings.Cut(sel, "/")
	if !ok {
		return 0, fmt.Errorf("resource %s: want <kind>/<name>", sel)
	}

	kind = strings.ToLower(kind)
	if k, ok := kindAliases[kind]; ok {
		kind = k
	}

	var exact, prefixed []int
	for i, d := range docs {
		if !strings.EqualFold(d.Kind, kind) || namespace != "" && d.Namespace != namespace {
			continue
		}

		if d.Name == name {
			exact = append(exact, i)
		} else if strings.HasSuffix(d.Name, "-"+name) {
			prefixed = append(prefixed, i)
		}
	}

	found := exact
	if len(found) == 0 {
		found = prefixed
	}

	switch {
	case len(found) == 1:
		return found[0], nil

	case len(found) == 0:
		var all []string
		for _, d := range docs {
			all = append(all, d.Kind+"/"+d.Name)
		}
		slices.Sort(all)
		return 0, fmt.Errorf("no %s in the render; resources: %s", sel, strings.Join(all, ", "))
	}

	var names, namespaces []string
	for _, i := range found {
		names = append(names, docs[i].Name)
		namespaces = append(namespaces, docs[i].Namespace)
	}

	if len(slices.Compact(slices.Sorted(slices.Values(names)))) == 1 {
		return 0, fmt.Errorf("%s/%s exists in namespaces %s: use --namespace", kind, name, strings.Join(namespaces, ", "))
	}

	return 0, fmt.Errorf("%s matches %s: use the full name", sel, strings.Join(names, ", "))
}

// Explain says which values leaves produce field f and how. Marker hits answer most fields; the
// rest are found by perturbing leaves in groups and watching f.
func Explain(ctx context.Context, idx *Index, c *render.Chart, v *Values, o render.Options, f Field) (Answer, error) {
	val, ok := idx.Fields[f]
	if !ok {
		return Answer{}, fmt.Errorf("unknown field %s", f.Path)
	}

	d := idx.Docs[f.Doc]
	a := Answer{Resource: d.Kind + "/" + d.Name, Field: f, Value: val, Template: d.Source}
	if idx.Random[f] {
		a.Kind = Random
		return a, nil
	}

	if strings.HasPrefix(f.Path[strings.LastIndexAny(f.Path, ".[")+1:], "checksum/") {
		a.Kind = Hashed
		return a, nil
	}

	if hits := idx.Hits[f]; len(hits) > 0 {
		a.Kind = Composed
		if len(hits) == 1 && hits[0].Exact && !hits[0].Encoded {
			a.Kind = Direct
		}

		for _, h := range hits {
			if h.Encoded {
				a.Kind = Encoded
			}
			a.Leaves = append(a.Leaves, v.Leaves[h.Leaf])
		}

		return a, nil
	}

	var candidates, bools []Leaf
	for _, l := range v.Leaves {
		if _, ok := l.Value.(bool); ok {
			bools = append(bools, l)
		} else if l.Value != "" && l.Value != 0.0 && l.Value != int64(0) { // file numbers are float64, --set ones int64
			candidates = append(candidates, l)
		}
	}

	p := perturber{c: c, o: o, idx: idx, user: v.User, field: f}
	found, err := p.find(ctx, append(candidates, bools...))
	if err != nil {
		return Answer{}, err
	}

	a.Kind = Chart
	if len(found) > 0 {
		a.Kind = Structural
	}

	for _, r := range found {
		if _, ok := r.leaf.Value.(bool); !ok || !r.removed {
			a.Kind = Derived
		}
		a.Leaves = append(a.Leaves, r.leaf)
	}

	return a, nil
}

// ExplainAll explains every field of document doc, sorted by path.
func ExplainAll(ctx context.Context, idx *Index, c *render.Chart, v *Values, o render.Options, doc int) ([]Answer, error) {
	var fields []Field
	for f := range idx.Fields {
		if f.Doc == doc {
			fields = append(fields, f)
		}
	}

	var out []Answer
	slices.SortFunc(fields, func(a, b Field) int { return strings.Compare(a.Path, b.Path) })
	for _, f := range fields {
		a, err := Explain(ctx, idx, c, v, o, f)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}

	return out, nil
}

// Affects returns the fields whose marker hits include leaf, sorted.
func Affects(idx *Index, leaf int) []Field {
	var out []Field
	for f, hits := range idx.Hits {
		if slices.ContainsFunc(hits, func(h Hit) bool { return h.Leaf == leaf }) {
			out = append(out, f)
		}
	}

	slices.SortFunc(out, func(a, b Field) int {
		if a.Doc != b.Doc {
			return a.Doc - b.Doc
		}
		return strings.Compare(a.Path, b.Path)
	})

	return out
}

// WriteHuman writes answers for a terminal. Several answers each get a header naming the field.
func WriteHuman(w io.Writer, as []Answer) error {
	var b strings.Builder
	for i, a := range as {
		if len(as) > 1 {
			if i > 0 {
				b.WriteByte('\n')
			}
			fmt.Fprintf(&b, "%s %s\n", a.Resource, a.Field.Path)
		}

		val := fmt.Sprint(a.Value)
		pad := strings.Repeat(" ", len(val)+2)
		var rows []string

		switch {
		case a.Kind == Direct:
			l := a.Leaves[0]
			fmt.Fprintf(&b, "%s  %s  %s\n", val, source(l.Chain[0]), PathString(l.Path))
			for j, s := range l.Chain[1:] {
				// shortcut: the last entry is labelled default even when it is a lower user file; Source doesn't know which files are the chart's.
				label := "├─ overrides"
				if j == len(l.Chain)-2 {
					label = "└─ default"
				}
				rows = append(rows, fmt.Sprintf("%s\t%s\t= %v", label, source(s), s.Value))
			}

		case len(a.Leaves) > 0:
			var from []string
			for _, l := range a.Leaves {
				from = append(from, fmt.Sprintf("%s (%s)", PathString(l.Path), source(l.Chain[0])))
			}
			fmt.Fprintf(&b, "%s  %s  ← %s\n", val, a.Kind, strings.Join(from, ", "))

		default:
			fmt.Fprintf(&b, "%s  %s\n", val, a.Kind)
		}

		rows = append(rows, "template\t"+a.Template)
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		for _, r := range rows {
			fmt.Fprintf(tw, "%s%s\n", pad, r)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// WriteMarkdown writes answers as a table, for pull request comments.
func WriteMarkdown(w io.Writer, as []Answer) error {
	var b strings.Builder
	b.WriteString("| Field | Value | Kind | Source | Template |\n|---|---|---|---|---|\n")
	for _, a := range as {
		var from []string
		for _, l := range a.Leaves {
			var s strings.Builder
			fmt.Fprintf(&s, "`%s` %s", PathString(l.Path), source(l.Chain[0]))
			for _, o := range l.Chain[1:] {
				s.WriteString(", overrides ")
				s.WriteString(source(o))
			}
			from = append(from, s.String())
		}

		fmt.Fprintf(&b, "| `%s %s` | `%v` | %s | %s | %s |\n", a.Resource, a.Field.Path, a.Value, a.Kind,
			strings.Join(from, "; "), a.Template)
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// WriteJSON writes answers as the kwhy.value/v1 JSON document.
func WriteJSON(w io.Writer, as []Answer) error {
	if as == nil {
		as = []Answer{}
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(struct {
		Schema  string   `json:"schema"`
		Answers []Answer `json:"answers"`
	}{"kwhy.value/v1", as})
}

// loadLayers returns every values source, lowest precedence first, and the subchart mount points.
func loadLayers(c *render.Chart, user map[string]any, files, sets []string) ([]layer, [][]any, error) {
	defaults, err := c.ValuesFiles(user)
	if err != nil {
		return nil, nil, err
	}

	var layers []layer
	var subcharts [][]any
	for _, f := range defaults {
		l, err := fileLayer(f.Name, f.Data)
		if err != nil {
			return nil, nil, err
		}

		for _, m := range f.Mount {
			l.mount = append(l.mount, m)
		}

		if l.mount != nil {
			subcharts = append(subcharts, l.mount)
		}

		layers = append(layers, l)
	}

	// shortcut: local files only; MergeValues above also accepts URLs, which then fail here.
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			return nil, nil, fmt.Errorf("read values: %w", err)
		}

		l, err := fileLayer(f, data)
		if err != nil {
			return nil, nil, err
		}

		layers = append(layers, l)
	}

	for i, s := range sets {
		tree := map[string]any{}
		if err := strvals.ParseInto(s, tree); err != nil {
			return nil, nil, fmt.Errorf("parse --set %s: %w", s, err)
		}

		layers = append(layers, layer{file: "--set " + s, tree: tree, line: i})
	}

	return layers, subcharts, nil
}

// walk calls fn for every non-null scalar under node.
func walk(node any, path []any, fn func(path []any, val any)) {
	switch n := node.(type) {
	case map[string]any:
		for k, c := range n {
			walk(c, append(slices.Clip(path), k), fn)
		}

	case []any:
		for i, c := range n {
			walk(c, append(slices.Clip(path), i), fn)
		}

	case nil:

	default:
		fn(path, n)
	}
}

// isGlobalCopy reports whether path is a subchart's copy of the parent's globals, which Helm
// makes while coalescing. The parent's global.* leaf is the one to trace.
func isGlobalCopy(path []any, subcharts [][]any) bool {
	for _, m := range subcharts {
		if rest, ok := cutPrefix(path, m); ok && len(rest) > 0 && rest[0] == "global" {
			return true
		}
	}

	return false
}

// chain lists the layers that set path, highest precedence first.
func chain(layers []layer, path []any) []Source {
	var out []Source
	for _, l := range slices.Backward(layers) {
		rel, ok := cutPrefix(path, l.mount)
		if !ok {
			continue
		}

		val, found, stop := probe(l.tree, rel)
		if found {
			line := l.line
			if l.lines != nil {
				line = l.lines[PathString(rel)]
			}
			out = append(out, Source{File: l.file, Line: line, Value: val})
		}

		if stop {
			break
		}
	}

	return out
}

// probe looks up path in one layer's tree. stop means lower layers can't reach the leaf: this
// layer deleted it (null), replaced a parent with another type, or set the list that holds it
// (Helm replaces lists whole).
func probe(tree map[string]any, path []any) (val any, found, stop bool) {
	var cur any = tree
	inList := false
	for _, p := range path {
		switch n := cur.(type) {
		case map[string]any:
			k, _ := p.(string)
			next, ok := n[k]
			if !ok {
				return nil, false, false
			}
			cur = next

		case []any:
			i, ok := p.(int)
			if !ok || i >= len(n) {
				return nil, false, true
			}
			inList = true
			cur = n[i]

		default:
			return nil, false, true
		}
	}

	switch cur.(type) {
	case map[string]any, []any, nil:
		return nil, false, true
	}

	return cur, true, inList
}

func cutPrefix(path, prefix []any) ([]any, bool) {
	if len(path) < len(prefix) || !slices.Equal(path[:len(prefix)], prefix) {
		return nil, false
	}

	return path[len(prefix):], true
}

// lines records the line of every scalar under n. Aliases resolve to the anchor's line.
func lines(n *yaml.Node, path []any, out map[string]int) {
	switch n.Kind {
	case yaml.DocumentNode:
		for _, c := range n.Content {
			lines(c, path, out)
		}

	case yaml.AliasNode:
		lines(n.Alias, path, out)

	case yaml.MappingNode:
		// Merge keys first, so explicit keys overwrite them; with `<<: [*a, *b]`, a wins over b.
		for i := 0; i < len(n.Content); i += 2 {
			if n.Content[i].Tag != "!!merge" {
				continue
			}

			src := n.Content[i+1]
			if src.Kind == yaml.SequenceNode {
				for _, s := range slices.Backward(src.Content) {
					lines(s, path, out)
				}
				continue
			}
			lines(src, path, out)
		}

		for i := 0; i < len(n.Content); i += 2 {
			if k := n.Content[i]; k.Tag != "!!merge" {
				lines(n.Content[i+1], append(slices.Clip(path), k.Value), out)
			}
		}

	case yaml.SequenceNode:
		for i, c := range n.Content {
			lines(c, append(slices.Clip(path), i), out)
		}

	case yaml.ScalarNode:
		out[PathString(path)] = n.Line
	}
}

// markerBase is the first numeric marker. Markers must stay below 1e6, where Helm starts printing
// floats as 1e+06; the base moves up past any leaf whose value could pass for a marker.
func markerBase(leaves []Leaf) int {
	base := 900000
	for slices.ContainsFunc(leaves, func(l Leaf) bool {
		n, ok := whole(l.Value)
		return ok && n >= base && n < base+len(leaves)
	}) {
		base += 1000
	}

	return base
}

func whole(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), n == float64(int(n))
	}

	return 0, false
}

// find returns the markers in s.
func find(s string, markers map[int]string, encoded bool) []Hit {
	var hits []Hit
	for _, m := range markerRe.FindAllStringSubmatch(s, -1) {
		for id, marker := range markers {
			if m[0] == marker {
				hits = append(hits, Hit{Leaf: id, Exact: !encoded && s == marker, Encoded: encoded})
			}
		}
	}

	return hits
}

// sameShape reports whether docs has the same documents and fields as the baseline.
func sameShape(baseDocs []render.Doc, baseFields map[Field]any, docs []render.Doc, fields map[Field]any) bool {
	if len(docs) != len(baseDocs) || len(fields) != len(baseFields) {
		return false
	}

	for i, d := range docs {
		if d.Kind != baseDocs[i].Kind || d.Source != baseDocs[i].Source {
			return false
		}
	}

	for f := range fields {
		if _, ok := baseFields[f]; !ok {
			return false
		}
	}

	return true
}

// setSparse writes val at path into sparse. Lists on the way are copied whole from coalesced,
// because Helm replaces lists instead of merging them.
func setSparse(sparse map[string]any, coalesced any, path []any, val any) {
	var cur, src any = sparse, coalesced
	for i, p := range path {
		last := i == len(path)-1
		switch k := p.(type) {
		case string:
			m, sm := cur.(map[string]any), src.(map[string]any)
			if last {
				m[k] = val
				return
			}

			if _, ok := m[k]; !ok {
				if l, isList := sm[k].([]any); isList {
					m[k] = deepCopy(l)
				} else {
					m[k] = map[string]any{}
				}
			}
			cur, src = m[k], sm[k]

		case int:
			l := cur.([]any)
			if last {
				l[k] = val
				return
			}
			cur, src = l[k], src.([]any)[k]
		}
	}
}

func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, x := range t {
			m[k] = deepCopy(x)
		}
		return m

	case []any:
		l := make([]any, len(t))
		for i, x := range t {
			l[i] = deepCopy(x)
		}
		return l
	}

	return v
}

// merge merges src into dst like values files do: maps merge, everything else replaces.
func merge(dst, src any) any {
	dm, ok1 := dst.(map[string]any)
	sm, ok2 := src.(map[string]any)
	if !ok1 || !ok2 {
		return src
	}

	for k, v := range sm {
		dm[k] = merge(dm[k], v)
	}

	return dm
}

func perturb(v any, zero bool) any {
	switch t := v.(type) {
	case bool:
		return !t
	case string:
		if zero {
			return ""
		}
		return t + "p9"
	case int:
		if zero {
			return 0
		}
		return t + 1
	case int64:
		if zero {
			return int64(0)
		}
		return t + 1
	case float64:
		if zero {
			return 0.0
		}
		return t + 1
	}

	return v
}

// fieldPath formats a document path: ".spec.ports[0].port", `.metadata.labels["app.kubernetes.io/name"]`.
func fieldPath(path []any) string {
	var b strings.Builder
	for _, p := range path {
		switch p := p.(type) {
		case int:
			fmt.Fprintf(&b, "[%d]", p)

		case string:
			if p == "" || strings.ContainsAny(p, `.[]"`) {
				fmt.Fprintf(&b, "[%q]", p)
			} else {
				b.WriteByte('.')
				b.WriteString(p)
			}
		}
	}

	return b.String()
}

// findItem returns the index of the item in list whose key k is v, or -1.
func findItem(list any, k, v string) int {
	l, _ := list.([]any)
	for i, item := range l {
		if m, ok := item.(map[string]any); ok && m[k] != nil && fmt.Sprint(m[k]) == v {
			return i
		}
	}

	return -1
}

func levenshtein(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}

	for i := range len(a) {
		cur := make([]int, len(b)+1)
		cur[0] = i + 1
		for j := range len(b) {
			cost := 1
			if a[i] == b[j] {
				cost = 0
			}
			cur[j+1] = min(prev[j+1]+1, cur[j]+1, prev[j]+cost)
		}
		prev = cur
	}

	return prev[len(b)]
}

func source(s Source) string {
	if strings.HasPrefix(s.File, "--set ") {
		return s.File
	}

	return fmt.Sprintf("%s:%d", s.File, s.Line)
}
