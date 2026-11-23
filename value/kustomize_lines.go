package value

import (
	"fmt"
	"maps"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

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

// names says whether Explain may name line of file for field f: the line shows the value, and with Confirm, its own
// edit moves the field. A line that can't be edited, or whose edited build gives no answer, has only the guard.
func (o *Overlay) names(file string, line int, f KField) bool {
	if !o.lineShows(file, line, o.Fields[f]) {
		return false
	}
	if !o.Confirm && !o.replaces {
		return true
	}

	mp := "/" + file
	data, ok, _ := perturbLine(o.orig[mp], line, "p9_confirm_")
	if !ok {
		return true
	}
	r, err := o.runBuild(o.Fields, nil, buildJob{entry: -1, files: map[string][]byte{mp: o.forBuild(mp, data)}})
	if err != nil || !r.ok {
		return true
	}
	_, moved := r.diff[f]

	return moved
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
