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
		{"change bad fail-on", []string{"change", "main..feature", "--chart", "c", "--fail-on", "fail,nope"}, 2},
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

	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
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

func TestRunChangeManifests(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
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
