package render

import (
	"os"
	"path/filepath"
	"testing"

	"helm.sh/helm/v4/pkg/cli"
)

const mini = "../testdata/charts/mini"

func load(t *testing.T, ref string) *Chart {
	t.Helper()
	c, err := LoadChart(ref, "", cli.New())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func find(docs []Doc, kind string) *Doc {
	for i := range docs {
		if docs[i].Kind == kind {
			return &docs[i]
		}
	}
	return nil
}

func TestRenderMini(t *testing.T) {
	docs, err := load(t, mini).Render(t.Context(), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 5 {
		t.Fatalf("got %d docs, want 5: %+v", len(docs), docs)
	}
	d := find(docs, "Deployment")
	if d == nil || d.Name != "rel-api" || d.Namespace != "default" || d.Source != "mini/templates/deployment.yaml" {
		t.Fatalf("deployment = %+v", d)
	}
	if d.Object["spec"].(map[string]any)["replicas"] != 1 {
		t.Fatalf("replicas = %v", d.Object["spec"])
	}
}

func TestRenderUserValues(t *testing.T) {
	docs, err := load(t, mini).Render(t.Context(), map[string]any{"ingress": map[string]any{"enabled": true}}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ing := find(docs, "Ingress")
	if ing == nil || ing.Object["spec"].(map[string]any)["ingressClassName"] != "nginx" {
		t.Fatalf("ingress = %+v", ing)
	}
}

func TestRenderHooksIncluded(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mini")
	if err := os.CopyFS(dir, os.DirFS(mini)); err != nil {
		t.Fatal(err)
	}
	hook := `apiVersion: batch/v1
kind: Job
metadata:
  name: migrate
  annotations:
    helm.sh/hook: pre-install
spec:
  template:
    spec:
      restartPolicy: Never
      containers: [{name: m, image: busybox}]
`
	if err := os.WriteFile(filepath.Join(dir, "templates", "hook.yaml"), []byte(hook), 0o644); err != nil {
		t.Fatal(err)
	}
	docs, err := load(t, dir).Render(t.Context(), nil, Options{Release: "x", Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	j := find(docs, "Job")
	if j == nil || j.Source != "mini/templates/hook.yaml" || j.Namespace != "ns" {
		t.Fatalf("hook = %+v", j)
	}
}

func TestCoalesceSubchart(t *testing.T) {
	v, err := load(t, mini).Coalesce(nil)
	if err != nil {
		t.Fatal(err)
	}
	if v["sub"].(map[string]any)["greeting"] != "hello" {
		t.Fatalf("sub = %v", v["sub"])
	}
}
