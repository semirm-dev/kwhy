//go:build accuracy

package value_test

import (
	"path/filepath"
	"slices"
	"testing"
	"time"

	"helm.sh/helm/v4/pkg/cli"

	"github.com/semirm-dev/kwhy/render"
	"github.com/semirm-dev/kwhy/value"
)

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

	v, err := value.LoadValues(c, []string{filepath.Join(dir, "argo-cd.prod.yaml")}, nil)
	if err != nil {
		tb.Fatal(err)
	}

	idx, err := value.Trace(tb.Context(), c, v, render.Options{}, value.ChunkSize)
	if err != nil {
		tb.Fatal(err)
	}

	doc, err := value.SelectDoc(idx.Docs, "deploy/argocd-server", "")
	if err != nil {
		tb.Fatal(err)
	}

	a, err := value.Explain(tb.Context(), idx, c, v, render.Options{}, value.Field{Doc: doc, Path: ".spec.replicas"})
	if err != nil {
		tb.Fatal(err)
	}
	if a.Kind != value.Direct {
		tb.Fatalf("kind %s", a.Kind)
	}
}
