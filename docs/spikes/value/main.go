// Spike: can marker-substitution trace rendered Helm fields back to values leaves?
// usage: spike <chartDir> <userValues.yaml>
package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type leaf struct {
	id   int
	path []any
	val  any
}

var chartDir, userFile, tmp string
var renders int

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func loadYAML(f string) any {
	b, err := os.ReadFile(f)
	must(err)
	var v any
	must(yaml.Unmarshal(b, &v))
	if v == nil {
		v = map[string]any{}
	}
	return v
}

func deepCopy(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := map[string]any{}
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

func merge(dst, src any) any {
	dm, ok1 := dst.(map[string]any)
	sm, ok2 := src.(map[string]any)
	if !ok1 || !ok2 {
		return deepCopy(src)
	}
	for k, v := range sm {
		dm[k] = merge(dm[k], v)
	}
	return dm
}

func truthy(v any) bool {
	switch t := v.(type) {
	case string:
		return t != ""
	case int:
		return t != 0
	case float64:
		return t != 0
	}
	return false // bools, nulls: control flow, never marked
}

func collect(v any, path []any, out *[]leaf) {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			collect(t[k], append(append([]any{}, path...), k), out)
		}
	case []any:
		for i, x := range t {
			collect(x, append(append([]any{}, path...), i), out)
		}
	default:
		if truthy(v) {
			*out = append(*out, leaf{id: len(*out), path: path, val: v})
		}
	}
}

// setSparse writes val at path into sparse override; lists are copied whole from merged (Helm replaces lists).
func setSparse(sparse map[string]any, merged any, path []any, val any) {
	var cur any = sparse
	mcur := merged
	for i, p := range path {
		last := i == len(path)-1
		switch k := p.(type) {
		case string:
			m := cur.(map[string]any)
			mm := mcur.(map[string]any)
			if last {
				m[k] = val
				return
			}
			if _, isList := mm[k].([]any); isList {
				if _, have := m[k].([]any); !have {
					m[k] = deepCopy(mm[k])
				}
			} else if _, have := m[k]; !have {
				m[k] = map[string]any{}
			}
			cur, mcur = m[k], mm[k]
		case int:
			l := cur.([]any)
			if last {
				l[k] = val
				return
			}
			cur, mcur = l[k], mcur.([]any)[k]
		}
	}
}

func render(extra map[string]any) ([]any, error) {
	renders++
	args := []string{"template", "rel", chartDir, "-n", "prod", "-f", userFile}
	if extra != nil {
		b, _ := yaml.Marshal(extra)
		f := filepath.Join(tmp, "extra.yaml")
		must(os.WriteFile(f, b, 0o644))
		args = append(args, "-f", f)
	}
	var out, errb bytes.Buffer
	cmd := exec.Command("helm", args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s", strings.TrimSpace(errb.String()))
	}
	dec := yaml.NewDecoder(&out)
	var docs []any
	for {
		var d any
		if dec.Decode(&d) != nil {
			break
		}
		if d != nil {
			docs = append(docs, d)
		}
	}
	return docs, nil
}

func fields(docs []any) map[string]any {
	out := map[string]any{}
	var walk func(v any, p string)
	walk = func(v any, p string) {
		switch t := v.(type) {
		case map[string]any:
			for k, x := range t {
				walk(x, p+"."+k)
			}
		case []any:
			for i, x := range t {
				walk(x, fmt.Sprintf("%s[%d]", p, i))
			}
		default:
			out[p] = v
		}
	}
	for i, d := range docs {
		kind, _ := d.(map[string]any)["kind"].(string)
		walk(d, fmt.Sprintf("%d:%s", i, kind))
	}
	return out
}

func sameShape(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

const numBase = 900000 // Helm prints floats >= 1e6 as 1e+06, so markers must stay below

func marker(l leaf) any {
	if _, ok := l.val.(string); ok {
		return fmt.Sprintf("zq%dqz", l.id)
	}
	return numBase + l.id
}

var strRe = regexp.MustCompile(`zq(\d+)qz|9\d{5}`)

type hit struct {
	field string
	exact bool
}

var baseFields map[string]any
var merged any

// trace marks a group of leaves; bisects on render failure or shape change.
func trace(ls []leaf, hits map[int][]hit, bad map[int]string) {
	sp := map[string]any{}
	for _, l := range ls {
		setSparse(sp, merged, l.path, marker(l))
	}
	docs, err := render(sp)
	reason := ""
	var f map[string]any
	if err != nil {
		reason = "render error"
	} else if f = fields(docs); !sameShape(f, baseFields) {
		reason = "changes structure"
	}
	if reason != "" {
		if len(ls) == 1 {
			bad[ls[0].id] = reason
			return
		}
		trace(ls[:len(ls)/2], hits, bad)
		trace(ls[len(ls)/2:], hits, bad)
		return
	}
	for k, v := range f {
		if fl, ok := v.(float64); ok && fl == float64(int(fl)) {
			v = int(fl)
		}
		switch t := v.(type) {
		case string:
			if d, err := base64.StdEncoding.DecodeString(t); err == nil {
				t = t + " " + string(d)
			}
			for _, m := range strRe.FindAllString(t, -1) {
				var id int
				if strings.HasPrefix(m, "zq") {
					id, _ = strconv.Atoi(m[2 : len(m)-2])
				} else {
					n, _ := strconv.Atoi(m)
					id = n - numBase
				}
				hits[id] = append(hits[id], hit{k, v == m})
			}
		case int:
			if t >= numBase && t < numBase+100000 {
				hits[t-numBase] = append(hits[t-numBase], hit{k, true})
			}
		}
	}
}

func perturb(v any) any {
	switch t := v.(type) {
	case string:
		return t + "p9"
	case int:
		return t + 1
	case float64:
		return t + 1
	}
	return v
}

func pathStr(p []any) string {
	var s []string
	for _, x := range p {
		s = append(s, fmt.Sprint(x))
	}
	return strings.Join(s, ".")
}

func main() {
	chartDir, userFile = os.Args[1], os.Args[2]
	tmp, _ = os.MkdirTemp("", "spike")
	defer os.RemoveAll(tmp)

	merged = merge(loadYAML(filepath.Join(chartDir, "values.yaml")), loadYAML(userFile))
	var ls []leaf
	collect(merged, nil, &ls)

	base, err := render(nil)
	must(err)
	baseFields = fields(base)
	base2, _ := render(nil)
	random := map[string]bool{}
	for k, v := range fields(base2) {
		if !reflect.DeepEqual(v, baseFields[k]) {
			random[k] = true
		}
	}

	hits, bad := map[int][]hit{}, map[int]string{}
	for i := 0; i < len(ls); i += 40 {
		trace(ls[i:min(i+40, len(ls))], hits, bad)
	}

	traced := map[string]bool{}
	for _, hs := range hits {
		for _, h := range hs {
			traced[h.field] = true
		}
	}

	// Ground truth by perturbation on a sample of leaves that affect output.
	rng := rand.New(rand.NewSource(1))
	order := rng.Perm(len(ls))
	var tp, fp, fn, sampled, shapeChanged, hashMiss int
	var misses []string
	for _, i := range order {
		if sampled >= 40 {
			break
		}
		l := ls[i]
		sp := map[string]any{}
		setSparse(sp, merged, l.path, perturb(l.val))
		docs, err := render(sp)
		if err != nil {
			continue
		}
		f := fields(docs)
		if !sameShape(f, baseFields) {
			shapeChanged++
			continue
		}
		truth := map[string]bool{}
		for k, v := range f {
			if !random[k] && !reflect.DeepEqual(v, baseFields[k]) {
				truth[k] = true
			}
		}
		if len(truth) == 0 {
			continue // leaf unused in this config
		}
		sampled++
		got := map[string]bool{}
		for _, h := range hits[l.id] {
			got[h.field] = true
		}
		for k := range got {
			if truth[k] {
				tp++
			} else {
				fp++
			}
		}
		for k := range truth {
			if !got[k] && strings.Contains(k, "checksum/") {
				hashMiss++
				continue
			}
			if !got[k] {
				fn++
				if len(misses) < 6 {
					misses = append(misses, fmt.Sprintf("%s -> %s (bad=%q)", pathStr(l.path), k, bad[l.id]))
				}
			}
		}
	}

	badCount := map[string]int{}
	var badEx []string
	for id, r := range bad {
		badCount[r]++
		if len(badEx) < 5 {
			badEx = append(badEx, fmt.Sprintf("%s (%s)", pathStr(ls[id].path), r))
		}
	}
	fmt.Printf("chart=%s leaves=%d renderedFields=%d tracedFields=%d (%.0f%%) renders=%d\n",
		filepath.Base(chartDir), len(ls), len(baseFields), len(traced), 100*float64(len(traced))/float64(len(baseFields)), renders)
	fmt.Printf("unmarkable leaves: %v  e.g. %v\n", badCount, badEx)
	fmt.Printf("perturbation check on %d used leaves: precision=%.2f recall=%.2f (tp=%d fp=%d fn=%d) shapeChangingPerturbations=%d\n",
		sampled, float64(tp)/float64(max(tp+fp, 1)), float64(tp)/float64(max(tp+fn, 1)), tp, fp, fn, shapeChanged)
	zero := 0
	for _, l := range ls {
		if len(hits[l.id]) == 0 && bad[l.id] == "" {
			zero++
		}
	}
	fmt.Printf("  random fields ignored=%d  checksum-annotation misses=%d  leaves with no marker hit (fallback candidates)=%d\n", len(random), hashMiss, zero)
	for _, m := range misses {
		fmt.Println("  miss:", m)
	}
}
