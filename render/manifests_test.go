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
	if _, err := render.ReadManifests(dir, "shop"); err == nil || !strings.Contains(err.Error(), "Kustomize overlay") {
		t.Errorf("kustomize dir: err = %v", err)
	}
}

func TestKustomize(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"base/kustomization.yaml": "resources: [deploy.yaml]\n",
		"base/deploy.yaml":        "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: api}\nspec: {replicas: 1}\n",
		"prod/kustomization.yaml": "resources: [../base]\nnamePrefix: prod-\nnamespace: shop\nreplicas: [{name: api, count: 3}]\n",
	} {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	docs, err := render.Kustomize(filepath.Join(dir, "prod"), "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Name != "prod-api" || docs[0].Namespace != "shop" ||
		docs[0].Object["spec"].(map[string]any)["replicas"] != 3 {
		t.Errorf("docs = %+v", docs)
	}
}
