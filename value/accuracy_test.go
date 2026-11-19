//go:build accuracy

package value

import (
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"helm.sh/helm/v4/pkg/cli"

	"github.com/semirm-dev/kwhy/render"
)

var publicCharts = []string{
	"podinfo", "ingress-nginx", "cert-manager", "grafana", "argo-cd",
	"postgresql", "kube-prometheus-stack", "redis", "keycloak", "external-dns",
}

// TestAccuracy checks attributions against ground truth: the fields that change when one sampled
// leaf alone is perturbed.
func TestAccuracy(t *testing.T) {
	for _, name := range publicCharts {
		t.Run(name, func(t *testing.T) {
			dir := filepath.Join("..", "testdata", "charts", "public")
			idx, v, c := trace(t, filepath.Join(dir, name+".tgz"), filepath.Join(dir, name+".prod.yaml"))
			o := render.Options{}

			byKey := map[string]int{}
			for i, d := range idx.Docs {
				byKey[d.Kind+"/"+d.Namespace+"/"+d.Name] = i
			}

			explained := map[Field]Answer{}
			var tp, fp, fn int
			var misses []string

			r := rand.New(rand.NewPCG(1, 0))
			for _, i := range r.Perm(len(v.Leaves))[:min(40, len(v.Leaves))] {
				l := v.Leaves[i]

				sparse := map[string]any{}
				setSparse(sparse, idx.coalesced, l.Path, perturb(l.Value, false))
				docs, err := c.Render(t.Context(), merge(deepCopy(v.User), sparse).(map[string]any), o)
				if err != nil {
					continue // the chart rejects the perturbed value; no ground truth
				}

				// Re-key the perturbed render by baseline doc, since doc indexes shift.
				after := map[Field]any{}
				for f, val := range Flatten(docs) {
					d := docs[f.Doc]
					if j, ok := byKey[d.Kind+"/"+d.Namespace+"/"+d.Name]; ok {
						after[Field{Doc: j, Path: f.Path}] = val
					}
				}

				moved := map[Field]bool{}
				for f, old := range idx.Fields {
					// random and hashed fields are not attributed to leaves
					if val, ok := after[f]; (!ok || val != old) && !idx.Random[f] && !strings.Contains(f.Path, "checksum/") {
						moved[f] = true
					}
				}

				predicted := map[Field]bool{}
				for _, f := range Affects(idx, l.ID) {
					predicted[f] = true
				}
				for f := range moved {
					if predicted[f] || len(idx.Hits[f]) > 0 {
						continue
					}
					a, ok := explained[f]
					if !ok {
						if a, err = Explain(t.Context(), idx, c, v, o, f); err != nil {
							t.Fatal(err)
						}
						explained[f] = a
					}
					if slices.ContainsFunc(a.Leaves, func(x Leaf) bool { return x.ID == l.ID }) {
						predicted[f] = true
					}
				}

				for f := range predicted {
					if moved[f] {
						tp++
					} else {
						fp++
						misses = append(misses, fmt.Sprintf("false: %s -> %s %s", PathString(l.Path), idx.Docs[f.Doc].Kind, f.Path))
					}
				}
				for f := range moved {
					if !predicted[f] {
						fn++
						misses = append(misses, fmt.Sprintf("missed: %s -> %s %s", PathString(l.Path), idx.Docs[f.Doc].Kind, f.Path))
					}
				}
			}

			precision, recall := ratio(tp, tp+fp), ratio(tp, tp+fn)
			t.Logf("leaves %d, fields %d, tp %d, precision %.2f, recall %.2f", len(v.Leaves), len(idx.Fields), tp, precision, recall)
			if precision < 1 || recall < 0.95 {
				slices.Sort(misses)
				t.Errorf("precision %.2f, recall %.2f:\n%s", precision, recall, strings.Join(misses, "\n"))
			}
		})
	}
}

// TestLatencyArgoCD is the spec's gate: p95 under 2s for one field on an argo-cd-sized chart.
func TestLatencyArgoCD(t *testing.T) {
	var runs []time.Duration
	for range 20 {
		start := time.Now()
		explainArgoCD(t)
		runs = append(runs, time.Since(start))
	}

	slices.Sort(runs)
	p95 := runs[18]
	t.Logf("p50 %s, p95 %s", runs[9], p95)
	if p95 > 2*time.Second {
		t.Fatalf("p95 %s over 2s", p95)
	}
}

func BenchmarkExplainArgoCD(b *testing.B) {
	for b.Loop() {
		explainArgoCD(b)
	}
}

func explainArgoCD(tb testing.TB) {
	dir := filepath.Join("..", "testdata", "charts", "public")

	c, err := render.LoadChart(filepath.Join(dir, "argo-cd.tgz"), "", cli.New())
	if err != nil {
		tb.Fatal(err)
	}

	v, err := LoadValues(c, []string{filepath.Join(dir, "argo-cd.prod.yaml")}, nil)
	if err != nil {
		tb.Fatal(err)
	}

	idx, err := Trace(tb.Context(), c, v, render.Options{})
	if err != nil {
		tb.Fatal(err)
	}

	doc, err := SelectDoc(idx.Docs, "deploy/argocd-server", "")
	if err != nil {
		tb.Fatal(err)
	}

	a, err := Explain(tb.Context(), idx, c, v, render.Options{}, Field{Doc: doc, Path: ".spec.replicas"})
	if err != nil {
		tb.Fatal(err)
	}
	if a.Kind != Direct {
		tb.Fatalf("kind %s", a.Kind)
	}
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 1
	}
	return float64(a) / float64(b)
}
