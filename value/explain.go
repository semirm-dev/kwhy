package value

import (
	"context"
	"fmt"
	"slices"
	"strings"

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
)

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
