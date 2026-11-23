//go:build accuracy

package value

import (
	"bytes"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// minRecall is Helm's 0.95 unless an overlay has a known gap.
var publicOverlays = []struct {
	repo, overlay string
	minRecall     float64
}{
	{"podinfo", "kustomize", 0.95},
	{"flux2-example", "apps/production", 0.95},
	{"flux2-example", "infrastructure/controllers", 0.95},
	{"microservices-demo", "kustomize/tests/memorystore-with-all-components", 0.95},
	{"microservices-demo", "kustomize/tests/service-mesh-istio-with-all-components", 0.95},
	{"argo-cd", "manifests/cluster-install", 0.95},
	{"argo-cd", "manifests/ha/cluster-install", 0.95},
	{"metrics-server", "manifests/overlays/release-ha", 0.95},
	// Names from namePrefix + the resource's own name line, and the references Kustomize renames with them, name
	// only the namePrefix line; values built by replacements name only some of their sources (0.87 on 2026-11-23).
	{"kubebuilder", "testdata/project-v4/config/default", 0.85},
	{"cluster-api", "core/config/default", 0.95},
	{"gatekeeper", "config/default", 0.95},
	{"kube-prometheus", ".", 0.95},
}

// TestOverlayAccuracy checks Kustomize attributions against ground truth, as TestAccuracy does for Helm: the
// fields that change when the values on one sampled source line alone are perturbed. A field counts as predicted
// for a line only when kwhy names that file:line; entry-only and file-only answers count as misses.
func TestOverlayAccuracy(t *testing.T) {
	for _, tc := range publicOverlays {
		t.Run(tc.repo+"/"+tc.overlay, func(t *testing.T) {
			root, err := filepath.Abs(filepath.Join("../testdata/kustomize/public", tc.repo))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(root); err != nil {
				t.Skip("run scripts/fetch-overlays.sh first")
			}
			t.Chdir(root)

			o, err := LoadOverlay(t.Context(), ".", tc.overlay, "")
			if err != nil {
				t.Fatal(err)
			}

			// Every named file:line, and the files the stack reads.
			named := map[string][]KField{}
			files := map[string]bool{}
			var lines, entryOnly, fileOnly int
			for _, d := range o.Docs {
				as, err := o.ExplainAll(d.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, a := range as {
					f := KField{d.ID, a.Field.Path}
					for _, l := range a.Leaves {
						files[l.Chain[0].File] = true
					}
					switch {
					case a.Kind == Chart:
						fileOnly++
					case a.Leaves[0].Chain[0].Line == 0 || strings.HasSuffix(a.Leaves[0].Entry, ")") ||
						a.Kind == Composed && len(o.writes[f]) == 0: // entries that move it together, at their own lines
						entryOnly++
					default:
						lines++
						for _, l := range a.Leaves { // a composed answer names each line that writes part of it
							named[l.Chain[0].String()] = append(named[l.Chain[0].String()], f)
						}
					}
				}
			}

			// Candidate lines, in memory paths: every value line of the stack's kustomizations and patches, and a
			// sample of the resource files' lines, where most answers are trivially right.
			type cand struct {
				mem  string
				line int
			}
			var all, sample []cand
			stack := map[string]bool{}
			for _, l := range o.lines {
				all = append(all, cand{l.file, l.line})
				stack[l.file] = true
			}
			for file := range files {
				if mem := "/" + o.rel(file); !stack[mem] {
					for _, l := range scalarLines(o.orig[mem]) {
						sample = append(sample, cand{mem, l})
					}
				}
			}
			slices.SortFunc(sample, func(a, b cand) int {
				return strings.Compare(fmt.Sprint(a.mem, a.line), fmt.Sprint(b.mem, b.line))
			})
			r := rand.New(rand.NewPCG(1, 0))
			r.Shuffle(len(sample), func(i, j int) { sample[i], sample[j] = sample[j], sample[i] })

			// movedBy perturbs one line and rebuilds; ok is false when there's no ground truth.
			movedBy := func(mem string, line int) (map[KField]bool, bool) {
				data, ok, _ := perturbLine(o.orig[mem], line, "p9")
				if !ok {
					return nil, false
				}
				data = o.forBuild(mem, data)
				inMem, err := o.mem.ReadFile(mem)
				if err != nil {
					t.Fatal(err)
				}
				if err := o.mem.WriteFile(mem, data); err != nil {
					t.Fatal(err)
				}
				got, _, err := o.build()
				if err := o.mem.WriteFile(mem, inMem); err != nil {
					t.Fatal(err)
				}
				if err != nil || !sameKeys(o.Fields, got) {
					return nil, false
				}
				moved := map[KField]bool{}
				for f, v := range o.Fields {
					if !reflect.DeepEqual(v, got[f]) {
						moved[f] = true
					}
				}
				return moved, true
			}

			var tp, fp, fn, sampled, stackSampled int
			var misses []string
			checked := map[KField]bool{}
			for i, c := range append(all, sample...) {
				if i >= len(all) && sampled-stackSampled == 50 {
					break
				}
				if i == len(all) {
					stackSampled = sampled
				}
				moved, ok := movedBy(c.mem, c.line)
				if !ok {
					continue // not a value, the build rejects it or its shape changes; no ground truth
				}
				if len(moved) == 0 {
					continue // unused
				}
				sampled++

				at := o.disk(strings.TrimPrefix(c.mem, "/")) + ":" + strconv.Itoa(c.line)
				predicted := map[KField]bool{}
				for _, f := range named[at] {
					predicted[f] = true
				}
				for f := range predicted {
					if moved[f] {
						tp++
					} else {
						fp++
						misses = append(misses, fmt.Sprintf("false: %s -> %s %s", at, f.Doc, f.Path))
					}
				}
				for f := range moved {
					if !predicted[f] {
						fn++
						a, _ := o.Explain(f)
						src := a.Leaves[0].Chain[0]
						misses = append(misses, fmt.Sprintf("missed: %s -> %s %s (kwhy: %s %s)", at, f.Doc, f.Path, src, a.Leaves[0].Entry))

						// The line kwhy names instead must move the field too, or kwhy named a wrong line.
						if src.Line == 0 || a.Kind != Direct || strings.HasSuffix(a.Leaves[0].Entry, ")") || checked[f] {
							continue
						}
						checked[f] = true
						if m, ok := movedBy("/"+o.rel(src.File), src.Line); ok && !m[f] {
							fp++
							misses = append(misses, fmt.Sprintf("false: %s -> %s %s", src, f.Doc, f.Path))
						}
					}
				}
			}

			precision, recall := ratio(tp, tp+fp), ratio(tp, tp+fn)
			t.Logf("fields %d (%d line, %d entry only, %d file only), lines %d (%d of kustomizations and patches), tp %d, precision %.2f, recall %.2f",
				len(o.Fields), lines, entryOnly, fileOnly, sampled, stackSampled, tp, precision, recall)
			if precision < 1 || recall < tc.minRecall {
				slices.Sort(misses)
				t.Errorf("precision %.2f, recall %.2f:\n%s", precision, recall, strings.Join(misses, "\n"))
			}
		})
	}
}

// rel turns a path relative to the working directory into one relative to o.root.
func (o *Overlay) rel(file string) string {
	abs, _ := filepath.Abs(file)
	rel, _ := filepath.Rel(o.root, abs)
	return filepath.ToSlash(rel)
}

// scalarLines are the lines where a one-line scalar value starts. A value folded over several lines (a long
// description, a JSON block) is named at its first line, so its other lines would test nothing kwhy claims.
func scalarLines(data []byte) []int {
	var ls []int
	dec := yaml.NewDecoder(bytes.NewReader(data))
	for {
		var n yaml.Node
		if dec.Decode(&n) != nil {
			break
		}
		var walk func(*yaml.Node)
		walk = func(n *yaml.Node) {
			switch n.Kind {
			case yaml.DocumentNode, yaml.SequenceNode:
				for _, c := range n.Content {
					walk(c)
				}
			case yaml.MappingNode:
				for i := 1; i < len(n.Content); i += 2 {
					walk(n.Content[i])
				}
			case yaml.ScalarNode:
				if n.Style&(yaml.LiteralStyle|yaml.FoldedStyle) == 0 && !strings.Contains(n.Value, "\n") &&
					!strings.Contains(n.Value, " ") || n.Tag != "!!str" {
					ls = append(ls, n.Line)
				}
			}
		}
		walk(&n)
	}
	slices.Sort(ls)
	return slices.Compact(ls)
}
