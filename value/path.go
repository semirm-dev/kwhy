package value

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/semirm-dev/kwhy/render"
)

var kindAliases = map[string]string{
	"deploy": "deployment", "sts": "statefulset", "ds": "daemonset",
	"svc": "service", "cm": "configmap", "ing": "ingress",
}

// Field is one scalar of a rendered document.
type Field struct {
	Doc  int    `json:"-"`    // index into the rendered docs
	Path string `json:"path"` // canonical, e.g. ".spec.template.spec.containers[0].image"
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
