package main

import (
	"bytes"
	"io"
	"os"
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
		{"value missing chart", []string{"value", "deploy/api", ".spec.replicas"}, 2},
		{"value missing path", []string{"value", "deploy/api", "--chart", "c"}, 2},
		{"value path and all", []string{"value", "deploy/api", ".spec.replicas", "--all", "--chart", "c"}, 2},
		{"value bad output", []string{"value", "deploy/api", "--all", "--chart", "c", "-o", "yaml"}, 2},
		{"value bad chunk size", []string{"value", "deploy/api", "--all", "--chart", "c", "--chunk-size", "0"}, 2},
		{"value flags after args", []string{"value", "deploy/api", ".spec.replicas", "--chart", "c", "-f", "a.yaml", "-f", "b.yaml"}, 1},
		{"change", []string{"change"}, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := run(tt.args, io.Discard, io.Discard); got != tt.want {
				t.Errorf("run(%q) = %d, want %d", tt.args, got, tt.want)
			}
		})
	}
}

func TestRunHappyPath(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"value", "deploy/api", ".spec.replicas", "--chart", mini, "-f", prod}, &out, &errb)

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
	code := run([]string{"value", "cm/x", ".data.host", "--chart", dir}, io.Discard, &errb)

	if code != 1 || !strings.Contains(errb.String(), "render chart:") || !strings.Contains(errb.String(), "db.host is required") {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
}

func TestRunUnknownPath(t *testing.T) {
	var errb bytes.Buffer
	code := run([]string{"value", "deploy/api", ".spec.replica", "--chart", mini}, io.Discard, &errb)

	if code != 1 || !strings.Contains(errb.String(), "did you mean .spec.replicas") {
		t.Fatalf("code %d, stderr %q", code, errb.String())
	}
}
