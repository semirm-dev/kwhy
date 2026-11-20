package render_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/semirm-dev/kwhy/render"
)

func TestReadManifests(t *testing.T) {
	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("app/deploy.yaml", "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: api}\n---\napiVersion: v1\nkind: Service\nmetadata: {name: api, namespace: edge}\n---\n")
	write("app/list.yml", "apiVersion: v1\nkind: List\nitems:\n  - {apiVersion: v1, kind: ConfigMap, metadata: {name: cfg}}\n")
	write("README.md", "kind: Deployment\n")
	write(".github/ci.yaml", "kind: Workflow\n")

	docs, err := render.ReadManifests(dir, "shop")
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, d := range docs {
		got = append(got, d.Source+" "+d.Kind+" "+d.Namespace+"/"+d.Name)
	}
	want := "app/deploy.yaml Deployment shop/api, app/deploy.yaml Service edge/api, app/list.yml ConfigMap shop/cfg"
	if strings.Join(got, ", ") != want {
		t.Errorf("docs:\n%s\nwant\n%s", strings.Join(got, ", "), want)
	}

	write("overlay/kustomization.yaml", "resources: [../app]\n")
	if _, err := render.ReadManifests(dir, "shop"); err == nil || !strings.Contains(err.Error(), "Kustomize directory") {
		t.Errorf("kustomize dir: err = %v", err)
	}
}
