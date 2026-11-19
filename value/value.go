// Package value traces fields of rendered Kubernetes config back to the values line that sets them.
package value

import (
	"bytes"
	"fmt"
	"os"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/cli/values"
	"helm.sh/helm/v4/pkg/getter"
	"helm.sh/helm/v4/pkg/strvals"

	"github.com/semirm-dev/kwhy/render"
)

// Kind says how a rendered field relates to the values that produce it.
// The strings are part of the kwhy.value/v1 JSON contract.
type Kind string

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

// Source is one place that sets a values leaf.
type Source struct {
	File  string // values file as the user gave it, a chart file like "mini/values.yaml", or "--set"
	Line  int    // 1-based line; for "--set" the flag index
	Value any
}

// Leaf is one scalar of the values Helm renders with.
type Leaf struct {
	ID    int
	Path  []any    // string keys and int list indexes, e.g. {"resources", "requests", "memory"}
	Value any      // winning value
	Chain []Source // Chain[0] is the winner, then what it overrode, ending at the chart default
}

// Values are the merged values of one render, with where every leaf comes from.
type Values struct {
	User   map[string]any // what Helm receives (files + --set merged, Helm semantics)
	Leaves []Leaf         // every scalar leaf of the coalesced values, sorted by path
}

// layer is one values source, in precedence order, with the line of every leaf it sets.
type layer struct {
	file  string
	mount []any // where the tree sits in the coalesced values; set for subchart defaults
	tree  map[string]any
	lines map[string]int // PathString relative to mount → line; nil for --set
	line  int            // --set flag index
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

		layers = append(layers, layer{file: "--set", tree: tree, line: i})
	}

	return layers, subcharts, nil
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

func cutPrefix(path, prefix []any) ([]any, bool) {
	if len(path) < len(prefix) || !slices.Equal(path[:len(prefix)], prefix) {
		return nil, false
	}

	return path[len(prefix):], true
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

var keyEscaper = strings.NewReplacer(".", `\.`, "[", `\[`)

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
