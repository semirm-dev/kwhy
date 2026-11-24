package main

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	mini = "../../testdata/charts/mini"
	prod = "../../testdata/values-prod.yaml"
)

// TestMain keeps trace caches out of the user's cache directory.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "kwhy-cache-")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_CACHE_HOME", dir) //nolint:errcheck
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func TestRunExitCodes(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want int
	}{
		{"no command", nil, 2},
		{"unknown command", []string{"nope"}, 2},
		{"help", []string{"help"}, 0},
		{"help flag", []string{"value", "--help"}, 0},
		{"unknown flag", []string{"value", "--nope"}, 2},
		{"value no subcommand", []string{"value"}, 2},
		{"value chart missing resource", []string{"value", "chart", "c"}, 2},
		{"value chart old flag", []string{"value", "chart", "c", "deploy/api", "--all", "--chart", "c"}, 2},
		{"value chart missing path", []string{"value", "chart", "c", "deploy/api"}, 2},
		{"value chart path and all", []string{"value", "chart", "c", "deploy/api", ".spec.replicas", "--all"}, 2},
		{"value chart bad output", []string{"value", "chart", "c", "deploy/api", "--all", "-o", "yaml"}, 2},
		{"value chart bad chunk size", []string{"value", "chart", "c", "deploy/api", "--all", "--chunk-size", "0"}, 2},
		{"value chart bad kube version", []string{"value", "chart", "c", "deploy/api", "--all", "--kube-version", "nope"}, 2},
		{"value chart flags after args", []string{"value", "chart", "c", "deploy/api", ".spec.replicas", "-f", "a.yaml", "-f", "b.yaml"}, 1},
		{"change missing range", []string{"change", "--chart", "c"}, 2},
		{"change not a range", []string{"change", "main", "--chart", "c"}, 2},
		{"change empty head", []string{"change", "main..", "--chart", "c"}, 2},
		{"change missing chart", []string{"change", "main..feature"}, 2},
		{"change chart and manifests", []string{"change", "main..feature", "--chart", "c", "--manifests", "m"}, 2},
		{"change manifests with values", []string{"change", "main..feature", "--manifests", "m", "--set", "a=1"}, 2},
		{"change bad output", []string{"change", "main..feature", "--chart", "c", "-o", "yaml"}, 2},
		{"pod missing name", []string{"pod"}, 2},
		{"pod two names", []string{"pod", "a", "b"}, 2},
		{"pod bad output", []string{"pod", "x", "-o", "yaml"}, 2},
		{"pod negative logs", []string{"pod", "x", "--logs", "-1"}, 2},
		{"pod values without chart", []string{"pod", "x", "-f", "a.yaml"}, 2},
		{"change bad fail-on", []string{"change", "main..feature", "--chart", "c", "--fail-on", "fail,nope"}, 2},
		{"pod chart and kustomize", []string{"pod", "x", "--chart", "c", "--kustomize", "k"}, 2},
		{"value kustomize missing resource", []string{"value", "kustomize", "o"}, 2},
		{"value kustomize missing path", []string{"value", "kustomize", "o", "deploy/api"}, 2},
		{"value kustomize path and all", []string{"value", "kustomize", "o", "deploy/api", ".spec.replicas", "--all"}, 2},
		{"value kustomize bad output", []string{"value", "kustomize", "o", "deploy/api", "--all", "-o", "yaml"}, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := run(tt.args, io.Discard, io.Discard); got != tt.want {
				t.Errorf("run(%q) = %d, want %d", tt.args, got, tt.want)
			}
		})
	}
}

func TestRunVersion(t *testing.T) {
	var out bytes.Buffer
	code := run([]string{"--version"}, &out, io.Discard)

	if code != 0 || !strings.Contains(out.String(), "kwhy dev") {
		t.Fatalf("code %d, stdout %q", code, out.String())
	}
}

func TestRunHappyPath(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"value", "chart", mini, "deploy/api", ".spec.replicas", "-f", prod}, &out, &errb)

	if code != 0 || !strings.HasPrefix(out.String(), "3  "+prod+":1") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out.String(), errb.String())
	}
}

func TestRunBaselineRenderFails(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"Chart.yaml":        "apiVersion: v2\nname: req\nversion: 0.1.0\n",
		"values.yaml":       "db: {}\n",
		"templates/cm.yaml": "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x\ndata:\n  host: {{ required \"db.host is required\" .Values.db.host }}\n",
	}
	for name, content := range files {
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	var errb bytes.Buffer
	code := run([]string{"value", "chart", dir, "cm/x", ".data.host"}, io.Discard, &errb)

	if code != 1 || !strings.Contains(errb.String(), "render chart:") || !strings.Contains(errb.String(), "db.host is required") {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
}

func TestRunValueKustomize(t *testing.T) {
	const fixture = "../../testdata/kustomize/fixture"
	var out, errb bytes.Buffer
	code := run([]string{"value", "kustomize", fixture + "/overlays/prod", "deploy/api", ".spec.replicas", "--root", fixture, "--no-cache"}, &out, &errb)

	if code != 0 || !strings.HasPrefix(out.String(), "3  "+fixture+"/overlays/prod/kustomization.yaml:12  replicas[0] name=api") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out.String(), errb.String())
	}
}

func TestRunUnknownPath(t *testing.T) {
	var errb bytes.Buffer
	code := run([]string{"value", "chart", mini, "deploy/api", ".spec.replica"}, io.Discard, &errb)

	if code != 1 || !strings.Contains(errb.String(), "did you mean .spec.replicas") {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
}

func TestRunChange(t *testing.T) {
	repo := t.TempDir()
	if err := os.CopyFS(filepath.Join(repo, "chart"), os.DirFS(mini)); err != nil {
		t.Fatal(err)
	}

	git := gitIn(t, repo)
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("prod.yaml", "replicaCount: 2\n")
	git("init", "-q", "-b", "main")
	git("add", ".")
	git("commit", "-qm", "base")

	git("checkout", "-qb", "feature")
	write("prod.yaml", "replicaCount: 3\nimage: {tag: '2.0'}\n")
	git("commit", "-qam", "head")
	git("checkout", "-q", "main")

	// The working copy is on main, so 3 pods proves prod.yaml was read from the feature checkout.
	t.Chdir(repo)
	var out, errb bytes.Buffer
	code := run([]string{"change", "main..feature", "--chart", "chart", "-f", "prod.yaml"}, &out, &errb)

	if code != 0 || !strings.Contains(out.String(), "Deployment default/rel-api  3 pods  image") ||
		!strings.Contains(out.String(), "3 pods restarted") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out.String(), errb.String())
	}

	out.Reset()
	if code := run([]string{"change", "main..feature", "--chart", "chart", "-f", "prod.yaml", "--fail-on", "fail,data-loss"}, &out, &errb); code != 0 {
		t.Errorf("--fail-on with no failure: code %d", code)
	}
	errb.Reset()
	if code := run([]string{"change", "main..feature", "--chart", "chart", "-f", "prod.yaml", "--fail-on", "restart", "-o", "json"}, &out, &errb); code != 3 ||
		!strings.Contains(errb.String(), "--fail-on: restart Deployment default/rel-api") {
		t.Errorf("--fail-on restart: code %d, stderr %q", code, errb.String())
	}

	cmd := exec.Command("git", "worktree", "list")
	cmd.Dir = repo
	if wt, _ := cmd.Output(); strings.Count(string(wt), "\n") != 1 {
		t.Errorf("worktrees left behind:\n%s", wt)
	}
}

// A pull request's symlink to a file outside the repo would let its chart print that file: refused.
func TestRunChangeSymlinkOutside(t *testing.T) {
	repo, outside := t.TempDir(), t.TempDir()
	if err := os.CopyFS(filepath.Join(repo, "chart"), os.DirFS(mini)); err != nil {
		t.Fatal(err)
	}
	git := gitIn(t, repo)
	git("init", "-q", "-b", "main")
	git("add", ".")
	git("commit", "-qm", "base")

	git("checkout", "-qb", "feature")
	secret := filepath.Join(outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("topsecret123"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(repo, "chart", "leak")); err != nil {
		t.Fatal(err)
	}
	leak := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: x-{{ .Files.Get \"leak\" | trim }}\n"
	if err := os.WriteFile(filepath.Join(repo, "chart", "templates", "leak.yaml"), []byte(leak), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "head")
	git("checkout", "-q", "main")

	t.Chdir(repo)
	var out, errb bytes.Buffer
	code := run([]string{"change", "main..feature", "--chart", "chart"}, &out, &errb)
	if code == 0 || strings.Contains(out.String()+errb.String(), "topsecret123") ||
		!strings.Contains(errb.String(), "links outside the repo") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out.String(), errb.String())
	}
}

// A pull request's overlay that names a base outside the checkout would build it on the CI runner: refused.
func TestRunChangeKustomizeOutside(t *testing.T) {
	repo, outside := t.TempDir(), t.TempDir()
	git := gitIn(t, repo)
	write := func(dir, name, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(outside, "kustomization.yaml", "resources: [cm.yaml]\n")
	write(outside, "cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: topsecret}\n")
	write(repo, "overlay/kustomization.yaml", "resources: [cm.yaml]\n")
	write(repo, "overlay/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: app}\n")
	git("init", "-q", "-b", "main")
	git("add", ".")
	git("commit", "-qm", "base")

	git("checkout", "-qb", "feature")
	// .. stops at /, so enough of them reach outside wherever the checkout is.
	write(repo, "overlay/kustomization.yaml", "resources: [cm.yaml, "+strings.Repeat("../", 40)+strings.TrimPrefix(outside, "/")+"]\n")
	git("commit", "-qam", "head")
	git("checkout", "-q", "main")

	t.Chdir(repo)
	var out, errb bytes.Buffer
	code := run([]string{"change", "main..feature", "--manifests", "overlay"}, &out, &errb)
	if code == 0 || strings.Contains(out.String(), "topsecret") || !strings.Contains(errb.String(), "outside the repo") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out.String(), errb.String())
	}
}

func TestRunChangeManifests(t *testing.T) {
	repo := t.TempDir()
	git := gitIn(t, repo)
	deploy := func(image string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(repo, "k8s"), 0o755); err != nil {
			t.Fatal(err)
		}
		y := "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: api}\nspec:\n  replicas: 2\n  selector: {matchLabels: {app: api}}\n" +
			"  template:\n    metadata: {labels: {app: api}}\n    spec: {containers: [{name: api, image: " + image + "}]}\n"
		if err := os.WriteFile(filepath.Join(repo, "k8s", "api.yaml"), []byte(y), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	deploy("acme/api:1.0")
	git("init", "-q", "-b", "main")
	git("add", ".")
	git("commit", "-qm", "base")
	git("checkout", "-qb", "feature")
	deploy("acme/api:2.0")
	git("commit", "-qam", "head")
	git("checkout", "-q", "main")

	t.Chdir(repo)
	var out, errb bytes.Buffer
	code := run([]string{"change", "main..feature", "--manifests", "k8s", "-n", "shop"}, &out, &errb)
	if code != 0 || !strings.Contains(out.String(), "Deployment shop/api  2 pods  image") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out.String(), errb.String())
	}
}

func TestRunChangeTargets(t *testing.T) {
	repo := t.TempDir()
	if err := os.CopyFS(filepath.Join(repo, "chart"), os.DirFS(mini)); err != nil {
		t.Fatal(err)
	}
	git := gitIn(t, repo)
	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write(".kwhy.yaml", "targets:\n  - {chart: chart, values: [prod.yaml], release: api, namespace: shop}\n  - {manifests: infra, namespace: infra}\n")
	write("prod.yaml", "replicaCount: 2\n")
	write("apps/api.yaml", "apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata: {name: api}\n"+
		"spec: {destination: {namespace: shop}, source: {repoURL: x, path: chart, helm: {valueFiles: [../prod.yaml]}}}\n")
	git("init", "-q", "-b", "main")
	git("add", ".")
	git("commit", "-qm", "base")

	git("checkout", "-qb", "feature")
	write("prod.yaml", "replicaCount: 2\nimage: {tag: '2.0'}\n")
	write("infra/cm.yaml", "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: limits}\ndata: {max: '5'}\n")
	git("add", ".")
	git("commit", "-qm", "head")
	// A second branch that also changes the app definitions: a parameter on api, and a new infra app.
	git("checkout", "-qb", "apps-change")
	write("apps/api.yaml", "apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata: {name: api}\n"+
		"spec: {destination: {namespace: shop}, source: {repoURL: x, path: chart, helm: {valueFiles: [../prod.yaml], parameters: [{name: replicaCount, value: '4'}]}}}\n")
	write("apps/infra.yaml", "apiVersion: argoproj.io/v1alpha1\nkind: Application\nmetadata: {name: infra}\n"+
		"spec: {destination: {namespace: infra}, source: {repoURL: x, path: infra}}\n")
	git("add", ".")
	git("commit", "-qm", "apps")
	git("checkout", "-q", "main")
	t.Chdir(repo)

	// No source flag: .kwhy.yaml. The infra directory only exists at head, so its ConfigMap is created.
	var out, errb bytes.Buffer
	if code := run([]string{"change", "main..feature"}, &out, &errb); code != 0 ||
		!strings.Contains(out.String(), "Deployment shop/api-api  2 pods  image") || !strings.Contains(out.String(), "create   ConfigMap infra/limits") {
		t.Fatalf(".kwhy.yaml: code %d, stdout %q, stderr %q", code, out.String(), errb.String())
	}

	// Argo CD discovery: the Application's release is its name, its namespace the destination.
	out.Reset()
	errb.Reset()
	if code := run([]string{"change", "main..feature", "--apps", "apps"}, &out, &errb); code != 0 ||
		!strings.Contains(out.String(), "Deployment shop/api-api  2 pods  image") {
		t.Fatalf("--apps: code %d, stdout %q, stderr %q", code, out.String(), errb.String())
	}

	// The working copy is on main: 4 pods and the infra app prove each ref's own definitions were read.
	out.Reset()
	errb.Reset()
	if code := run([]string{"change", "main..apps-change", "--apps", "apps"}, &out, &errb); code != 0 ||
		!strings.Contains(out.String(), "Deployment shop/api-api  4 pods  image") || !strings.Contains(out.String(), "create   ConfigMap infra/limits") {
		t.Fatalf("--apps changed at head: code %d, stdout %q, stderr %q", code, out.String(), errb.String())
	}

	// App paths are relative to the repo root, wherever kwhy runs.
	t.Chdir(filepath.Join(repo, "apps"))
	out.Reset()
	errb.Reset()
	if code := run([]string{"change", "main..feature", "--apps", "."}, &out, &errb); code != 0 ||
		!strings.Contains(out.String(), "Deployment shop/api-api  2 pods  image") {
		t.Fatalf("--apps from a subdirectory: code %d, stdout %q, stderr %q", code, out.String(), errb.String())
	}
	t.Chdir(repo)

	if code := run([]string{"change", "main..feature", "--apps", "apps", "-n", "x"}, io.Discard, io.Discard); code != 2 {
		t.Errorf("--apps with -n: code %d, want 2", code)
	}
	if code := run([]string{"change", "main..feature", "--chart", "nope"}, io.Discard, io.Discard); code != 2 {
		t.Errorf("--chart to a missing path: code %d, want 2", code)
	}
	// A config path at neither ref is a typo, not "no effect".
	write("typo.yaml", "targets:\n  - {chart: chartz}\n")
	if code := run([]string{"change", "main..feature", "--config", "typo.yaml"}, io.Discard, io.Discard); code != 2 {
		t.Errorf("config path at neither ref: code %d, want 2", code)
	}
}

func TestRunChangeKustomize(t *testing.T) {
	repo := t.TempDir()
	git := gitIn(t, repo)
	write := func(name, content string) {
		t.Helper()
		p := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write("base/kustomization.yaml", "resources: [deploy.yaml]\n")
	write("base/deploy.yaml", "apiVersion: apps/v1\nkind: Deployment\nmetadata: {name: api}\nspec:\n  replicas: 1\n  selector: {matchLabels: {app: api}}\n"+
		"  template: {metadata: {labels: {app: api}}, spec: {containers: [{name: c, image: app:1}]}}\n")
	write("prod/kustomization.yaml", "resources: [../base]\nnamespace: shop\nreplicas: [{name: api, count: 2}]\n")
	git("init", "-q", "-b", "main")
	git("add", ".")
	git("commit", "-qm", "base")
	git("checkout", "-qb", "feature")
	write("prod/kustomization.yaml", "resources: [../base]\nnamespace: shop\nreplicas: [{name: api, count: 5}]\n")
	git("commit", "-qam", "head")
	git("checkout", "-q", "main")
	t.Chdir(repo)

	var out, errb bytes.Buffer
	if code := run([]string{"change", "main..feature", "--manifests", "prod"}, &out, &errb); code != 0 ||
		!strings.Contains(out.String(), "Deployment shop/api") || !strings.Contains(out.String(), "2 → 5") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, out.String(), errb.String())
	}
}

// gitIn runs git commands in repo, failing the test on an error.
func gitIn(t *testing.T, repo string) func(args ...string) {
	return func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}
