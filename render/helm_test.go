package render

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
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

// copyMini copies the mini chart to a temp dir and writes extra files into it, relative to the chart root.
func copyMini(t *testing.T, files map[string]string) string {
	t.Helper()

	dir := filepath.Join(t.TempDir(), "mini")
	if err := os.CopyFS(dir, os.DirFS(mini)); err != nil {
		t.Fatal(err)
	}

	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return dir
}

// withDep is mini's Chart.yaml declaring the sub chart as a dependency.
func withDep(extra string) map[string]string {
	return map[string]string{"Chart.yaml": `apiVersion: v2
name: mini
version: 0.1.0
appVersion: "1.2.3"
dependencies:
  - name: sub
    version: 0.1.0
` + extra}
}

func TestRenderHooksIncluded(t *testing.T) {
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
	dir := copyMini(t, map[string]string{"templates/hook.yaml": hook})

	docs, err := load(t, dir).Render(t.Context(), nil, Options{Release: "x", Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}

	j := find(docs, "Job")
	if j == nil || j.Source != "mini/templates/hook.yaml" || j.Namespace != "ns" {
		t.Fatalf("hook = %+v", j)
	}
}

func TestLoadChartOCIHasRegistryClient(t *testing.T) {
	// Nothing listens on port 1, so a connection error is expected; no network needed.
	_, err := LoadChart("oci://127.0.0.1:1/charts/x", "1.0.0", cli.New())
	if err == nil {
		t.Fatal("want an error from an unreachable registry")
	}

	if strings.Contains(err.Error(), "missing registry client") {
		t.Fatalf("oci ref used without a registry client: %v", err)
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

func TestRenderRepeatableAfterDisabledSubchart(t *testing.T) {
	c := load(t, copyMini(t, withDep("    condition: sub.enabled\n")))

	off, err := c.Render(t.Context(), map[string]any{"sub": map[string]any{"enabled": false}}, Options{})
	if err != nil {
		t.Fatal(err)
	}

	if len(off) != 4 {
		t.Fatalf("sub disabled: got %d docs, want 4", len(off))
	}

	on, err := c.Render(t.Context(), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}

	if len(on) != 5 {
		t.Fatalf("render after a disabled render: got %d docs, want 5", len(on))
	}
}

func TestCoalesceSubchartAlias(t *testing.T) {
	v, err := load(t, copyMini(t, withDep("    alias: greeter\n"))).Coalesce(nil)
	if err != nil {
		t.Fatal(err)
	}

	greeter, _ := v["greeter"].(map[string]any)
	if greeter["greeting"] != "hello" {
		t.Fatalf("aliased subchart values = %v", v)
	}
}

func TestValuesFiles(t *testing.T) {
	tests := []struct {
		name  string
		chart string
		user  map[string]any
		want  []string // "mount|name", lowest precedence first
	}{
		{"subchart then root", mini, nil, []string{"sub|mini/charts/sub/values.yaml", "|mini/values.yaml"}},
		{"alias mounts under alias", copyMini(t, withDep("    alias: greeter\n")), nil, []string{"greeter|mini/charts/greeter/values.yaml", "|mini/values.yaml"}},
		{"disabled subchart dropped", copyMini(t, withDep("    condition: sub.enabled\n")), map[string]any{"sub": map[string]any{"enabled": false}}, []string{"|mini/values.yaml"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			files, err := load(t, tt.chart).ValuesFiles(tt.user)
			if err != nil {
				t.Fatal(err)
			}

			var got []string
			for _, f := range files {
				got = append(got, strings.Join(f.Mount, ".")+"|"+f.Name)
			}

			if !slices.Equal(got, tt.want) {
				t.Fatalf("got %q, want %q", got, tt.want)
			}

			if root := files[len(files)-1]; !strings.HasPrefix(string(root.Data), "replicaCount: 1\n") {
				t.Fatalf("root data = %q", root.Data)
			}
		})
	}
}
