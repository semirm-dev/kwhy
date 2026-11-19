package value

import (
	"context"
	"encoding/base64"
	"fmt"
	"regexp"
	"slices"
	"strconv"

	"github.com/semirm-dev/kwhy/render"
)

const chunkSize = 40 // how many leaves one tracing render marks

var markerRe = regexp.MustCompile(`zq(\d+)qz|\d+`)

// Index is what marker tracing learned about one render.
type Index struct {
	Docs       []render.Doc  // baseline render
	Fields     map[Field]any // baseline values
	Hits       map[Field][]Hit
	Random     map[Field]bool // differs between two baseline renders
	Unmarkable map[int]string // Leaf.ID → "render error" | "changes structure"

	coalesced map[string]any // the values Helm rendered the baseline with
	breakers  map[int]bool   // Leaf.ID → perturbing it alone fails the render; nil until Explain needs it
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
