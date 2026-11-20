package change

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Target is one app to render at each ref: a source, with its release name and namespace.
type Target struct {
	Source
	Release, Namespace string
}

// config is the shape of .kwhy.yaml.
type config struct {
	Targets []struct {
		Chart     string   `yaml:"chart"`
		Manifests string   `yaml:"manifests"`
		Values    []string `yaml:"values"`
		Set       []string `yaml:"set"`
		SetString []string `yaml:"setString"`
		SetJSON   []string `yaml:"setJSON"`
		Release   string   `yaml:"release"`
		Namespace string   `yaml:"namespace"`
	} `yaml:"targets"`
}

// LoadConfig reads the targets of a .kwhy.yaml file. Paths in it are relative to the repo root, where kwhy runs.
func LoadConfig(path string) ([]Target, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	var c config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}

	var ts []Target
	for i, t := range c.Targets {
		if (t.Chart == "") == (t.Manifests == "") {
			return nil, fmt.Errorf("read config %s: target %d: give exactly one of chart or manifests", path, i+1)
		}
		tg := Target{Source: Source{Chart: t.Chart, Manifests: t.Manifests}, Release: or(t.Release, "rel"), Namespace: or(t.Namespace, "default")}
		tg.Values.ValueFiles, tg.Values.Values, tg.Values.StringValues, tg.Values.JSONValues = t.Values, t.Set, t.SetString, t.SetJSON
		ts = append(ts, tg)
	}
	if len(ts) == 0 {
		return nil, fmt.Errorf("read config %s: no targets", path)
	}

	return ts, nil
}

// exists says whether path exists in the working tree.
func exists(path string) bool {
	_, err := os.Stat(path)
	return !errors.Is(err, fs.ErrNotExist)
}

// isChart says whether dir holds a Helm chart.
func isChart(dir string) bool {
	return exists(filepath.Join(dir, "Chart.yaml"))
}

// isKustomize says whether dir holds a kustomization file.
func isKustomize(dir string) bool {
	for _, n := range []string{"kustomization.yaml", "kustomization.yml", "Kustomization"} {
		if exists(filepath.Join(dir, n)) {
			return true
		}
	}

	return false
}

// sameRepo compares two git remote URLs, ignoring scheme, user, a .git suffix and https/ssh form.
func sameRepo(a, b string) bool {
	norm := func(u string) string {
		u = strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(u), "/"), ".git")
		if i := strings.Index(u, "://"); i >= 0 {
			u = u[i+3:]
		}
		if i := strings.Index(u, "@"); i >= 0 {
			u = u[i+1:]
		}
		return strings.ToLower(strings.Replace(u, ":", "/", 1))
	}

	return a != "" && b != "" && norm(a) == norm(b)
}
