//go:build accuracy

package value

import (
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
			c, err := render.LoadChart(filepath.Join(dir, name+".tgz"), "", cli.New())
			if err != nil {
				t.Fatal(err)
			}

			v, err := LoadValues(c, []string{filepath.Join(dir, name+".prod.yaml")}, nil)
			if err != nil {
				t.Fatal(err)
			}

			idx, err := Trace(t.Context(), c, v, render.Options{}, ChunkSize)
			if err != nil {
				t.Fatal(err)
			}
			o := render.Options{}

			explained := map[Field]Answer{}
			var tp, fp, fn, sampled int
			var misses []string

			// Sample 40 leaves that move at least one field, as the spike did.
			r := rand.New(rand.NewPCG(1, 0))
			for _, i := range r.Perm(len(v.Leaves)) {
				if sampled == 40 {
					break
				}
				l := v.Leaves[i]

				sparse := map[string]any{}
				setSparse(sparse, idx.coalesced, l.Path, perturb(l.Value, false))
				docs, err := c.Render(t.Context(), merge(deepCopy(v.User), sparse).(map[string]any), o)
				if err != nil {
					continue // the chart rejects the perturbed value; no ground truth
				}

				// As in the spike that set the 0.95 target: only perturbations that keep the render's
				// shape count, so fields compare by doc index and path.
				after := Flatten(docs)
				if !sameShape(idx.Docs, idx.Fields, docs, after) {
					continue
				}

				moved := map[Field]bool{}
				for f, old := range idx.Fields {
					// random and hashed fields are not attributed to leaves
					if after[f] != old && !idx.Random[f] && !strings.Contains(f.Path, "checksum/") {
						moved[f] = true
					}
				}
				if len(moved) == 0 {
					continue // unused in this config
				}
				sampled++

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
			t.Logf("leaves %d, fields %d, sampled %d, tp %d, precision %.2f, recall %.2f", len(v.Leaves), len(idx.Fields), sampled, tp, precision, recall)
			if precision < 1 || recall < 0.95 {
				slices.Sort(misses)
				t.Errorf("precision %.2f, recall %.2f:\n%s", precision, recall, strings.Join(misses, "\n"))
			}
		})
	}
}

func ratio(a, b int) float64 {
	if b == 0 {
		return 1
	}
	return float64(a) / float64(b)
}
