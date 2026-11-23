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

	docs, err := render.Kustomize(dir, filepath.Join(dir, "prod"), "default")
	if err != nil {
		t.Fatal(err)
	}
	if len(docs) != 1 || docs[0].Name != "prod-api" || docs[0].Namespace != "shop" ||
		docs[0].Object["spec"].(map[string]any)["replicas"] != 3 {
		t.Errorf("docs = %+v", docs)
	}
}

// A kustomization from a pull request must not make kwhy fetch anything: remote bases and URLs are refused before
// Kustomize sees them. A URL that is only a label or annotation value is fine.
func TestKustomizeOffline(t *testing.T) {
	for _, k := range []string{
		"resources: [https://example.com/deploy.yaml]",
		"resources: ['github.com/acme/platform//base?ref=v1']",
		"resources: [git@github.com:acme/platform.git]",
		"components: [gitlab.com/acme/platform/monitoring]",
		"resources: [base]\npatches: [{path: https://example.com/patch.yaml}]",
		"resources: [base]\nconfigMapGenerator: [{name: c, files: [conf=https://example.com/app.conf]}]",
		"resources: [remote-base]",
	} {
		dir := t.TempDir()
		write(t, dir, "overlay/kustomization.yaml", k+"\n")
		write(t, dir, "overlay/base/kustomization.yaml", "resources: [cm.yaml]\n")
		write(t, dir, "overlay/base/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\n")
		write(t, dir, "overlay/remote-base/kustomization.yaml", "resources: [https://example.com/base.yaml]\n")
		_, err := render.Kustomize(dir, filepath.Join(dir, "overlay"), "default")
		if err == nil || !strings.Contains(err.Error(), "kwhy doesn't fetch") {
			t.Errorf("%s: err = %v, want a refusal", k, err)
		}
	}

	dir := t.TempDir()
	write(t, dir, "k/kustomization.yaml", "resources: [cm.yaml]\ncommonAnnotations: {docs: 'https://example.com'}\n")
	write(t, dir, "k/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: c}\n")
	if _, err := render.Kustomize(dir, filepath.Join(dir, "k"), "default"); err != nil {
		t.Errorf("URL annotation: %v", err)
	}
}

// A pull request's overlay must not reach past the checkout, by .. or by a symlink it commits.
func TestKustomizeStaysInRepo(t *testing.T) {
	for name, setup := range map[string]func(dir, repo string){
		"dotdot": func(dir, repo string) {
			write(t, repo, "overlay/kustomization.yaml", "resources: [../../outside]\n")
		},
		"symlinked dir": func(dir, repo string) {
			write(t, repo, "overlay/kustomization.yaml", "resources: [base]\n")
			symlink(t, filepath.Join(dir, "outside"), filepath.Join(repo, "overlay/base"))
		},
		"symlinked file": func(dir, repo string) {
			write(t, repo, "overlay/kustomization.yaml", "resources: [cm.yaml]\n")
			symlink(t, filepath.Join(dir, "outside/cm.yaml"), filepath.Join(repo, "overlay/cm.yaml"))
		},
		"symlinked overlay": func(dir, repo string) {
			symlink(t, filepath.Join(dir, "outside"), filepath.Join(repo, "overlay"))
		},
	} {
		dir := t.TempDir()
		repo := filepath.Join(dir, "repo")
		write(t, dir, "outside/kustomization.yaml", "resources: [cm.yaml]\n")
		write(t, dir, "outside/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: secret}\n")
		setup(dir, repo)
		if docs, err := render.Kustomize(repo, filepath.Join(repo, "overlay"), "default"); err == nil || !strings.Contains(err.Error(), "outside") {
			t.Errorf("%s: docs %v, err = %v; want a refusal", name, docs, err)
		}
	}
}

func TestReadManifestsSkipsSymlinks(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "outside/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: secret}\n")
	write(t, dir, "repo/app/deploy.yaml", "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: api}\n")
	symlink(t, filepath.Join(dir, "outside/cm.yaml"), filepath.Join(dir, "repo/app/cm.yaml"))

	docs, err := render.ReadManifests(filepath.Join(dir, "repo/app"), "default")
	if err != nil || len(docs) != 1 || docs[0].Kind != "Deployment" {
		t.Errorf("docs %+v, err %v; want only the Deployment", docs, err)
	}
}

func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
