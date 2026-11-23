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

// Discover finds Argo CD Applications and Flux HelmReleases in the YAML under dir and turns each into a target.
// Their paths are relative to the repo checkout at tree.
// remote is this repo's git remote URL: sources from another repo are skipped ("" accepts every repoURL). What
// can't be rendered from this repo is skipped with a warning naming the object and why, never guessed. Inline
// values are written to temp files under tmp, which the caller removes.
func Discover(tree, dir, remote, tmp string) (ts []Target, warnings []string, err error) {
	var objs []obj
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		dec := yaml.NewDecoder(bytes.NewReader(data))
		for {
			var o obj
			if err := dec.Decode(&o); errors.Is(err, io.EOF) {
				return nil
			} else if err != nil {
				return nil // not every YAML file is a manifest; the ones that are decode
			}
			if o != nil {
				objs = append(objs, o)
			}
		}
	})
	if err != nil {
		return nil, nil, fmt.Errorf("discover apps: %w", err)
	}

	gitRepos := map[string]string{} // GitRepository name → url
	for _, o := range objs {
		if o["kind"] == "GitRepository" {
			gitRepos[str(o, "metadata", "name")] = str(o, "spec", "url")
		}
	}

	write := func(name string, values any) (string, error) {
		var data []byte
		if s, ok := values.(string); ok {
			data = []byte(s)
		} else if data, err = yaml.Marshal(values); err != nil {
			return "", err
		}
		// A unique file each: sources of one app, and values next to valuesObject, share the app's name.
		f, err := os.CreateTemp(tmp, strings.ReplaceAll(name, "/", "_")+"-*.values.yaml")
		if err != nil {
			return "", err
		}
		if _, err := f.Write(data); err != nil {
			f.Close()
			return "", err
		}
		return f.Name(), f.Close()
	}

	for _, o := range objs {
		apiVersion, _ := o["apiVersion"].(string)
		name := str(o, "metadata", "name")
		switch {
		case o["kind"] == "Application" && strings.HasPrefix(apiVersion, "argoproj.io/"):
			srcs := list(get(o, "spec", "sources"))
			if s := get(o, "spec", "source"); s != nil {
				srcs = append(srcs, s)
			}
			for i, s := range srcs {
				id := fmt.Sprintf("Application %s", name)
				if len(srcs) > 1 {
					id += fmt.Sprintf(" source %d", i+1)
				}
				t, warn, err := argoTarget(s, tree, name, str(o, "spec", "destination", "namespace"), remote, id, write)
				if err != nil {
					return nil, nil, err
				}
				if warn != "" {
					warnings = append(warnings, warn)
				} else if t != nil {
					ts = append(ts, *t)
				}
			}

		case o["kind"] == "Kustomization" && strings.HasPrefix(apiVersion, "kustomize.toolkit.fluxcd.io/"):
			id := "Kustomization " + name
			ref := str(o, "spec", "sourceRef", "name")
			if kind := str(o, "spec", "sourceRef", "kind"); kind != "GitRepository" {
				warnings = append(warnings, fmt.Sprintf("%s: source is a %s, not this repo: skipped", id, kind))
				continue
			}
			if url, ok := gitRepos[ref]; ok && remote != "" && !sameRepo(url, remote) {
				warnings = append(warnings, fmt.Sprintf("%s: GitRepository %s is %s, not this repo: skipped", id, ref, url))
				continue
			}
			ts = append(ts, Target{Manifests: filepath.Clean(or(str(o, "spec", "path"), ".")), Release: name,
				Namespace: or(str(o, "spec", "targetNamespace"), "default")})

		case o["kind"] == "HelmRelease" && strings.HasPrefix(apiVersion, "helm.toolkit.fluxcd.io/"):
			id := "HelmRelease " + name
			spec := get(o, "spec", "chart", "spec")
			ref := str(spec, "sourceRef", "name")
			if kind := str(spec, "sourceRef", "kind"); kind != "GitRepository" {
				warnings = append(warnings, fmt.Sprintf("%s: chart from a %s, not this repo: skipped", id, kind))
				continue
			}
			if url, ok := gitRepos[ref]; ok && remote != "" && !sameRepo(url, remote) {
				warnings = append(warnings, fmt.Sprintf("%s: GitRepository %s is %s, not this repo: skipped", id, ref, url))
				continue
			}

			path := filepath.Clean(str(spec, "chart"))
			targetNS := str(o, "spec", "targetNamespace")
			release := str(o, "spec", "releaseName")
			if release == "" {
				release = name
				if targetNS != "" {
					release = targetNS + "-" + name
				}
			}
			t := Target{Chart: path, Release: release, Namespace: or(targetNS, or(str(o, "metadata", "namespace"), "default"))}
			for _, f := range list(get(spec, "valuesFiles")) {
				t.Values.ValueFiles = append(t.Values.ValueFiles, filepath.Clean(fmt.Sprint(f)))
			}
			if v := get(o, "spec", "values"); v != nil {
				f, err := write("flux-"+name, v)
				if err != nil {
					return nil, nil, fmt.Errorf("discover apps: %w", err)
				}
				t.Values.ValueFiles = append(t.Values.ValueFiles, f)
			}
			if get(o, "spec", "valuesFrom") != nil {
				warnings = append(warnings, fmt.Sprintf("%s: valuesFrom (ConfigMaps, Secrets in the cluster) isn't read; the render may differ", id))
			}
			ts = append(ts, t)
		}
	}

	return ts, warnings, nil
}

// argoTarget turns one Argo CD source into a target, or a warning saying why it can't.
func argoTarget(s any, tree, name, ns, remote, id string, write func(string, any) (string, error)) (*Target, string, error) {
	if str(s, "chart") != "" {
		return nil, fmt.Sprintf("%s: chart %s from a Helm repository, not this repo: skipped", id, str(s, "chart")), nil
	}
	if url := str(s, "repoURL"); remote != "" && !sameRepo(url, remote) {
		return nil, fmt.Sprintf("%s: source %s, not this repo: skipped", id, url), nil
	}

	path := str(s, "path")
	if path == "" {
		if str(s, "ref") != "" {
			return nil, "", nil // a values-only source other sources refer to as $ref
		}
		return nil, fmt.Sprintf("%s: no path: skipped", id), nil
	}
	path = filepath.Clean(path)

	t := Target{Release: name, Namespace: or(ns, "default")}
	switch {
	case !isChart(filepath.Join(tree, path)) && get(s, "helm") == nil: // plain YAML or a Kustomize overlay
		t.Manifests = path
		return &t, "", nil
	}

	t.Chart = path
	helm := get(s, "helm")
	t.Release = or(str(helm, "releaseName"), name)
	for _, f := range list(get(helm, "valueFiles")) {
		vf := fmt.Sprint(f)
		if strings.HasPrefix(vf, "$") {
			return nil, fmt.Sprintf("%s: value file %s from another source isn't supported: skipped", id, vf), nil
		}
		t.Values.ValueFiles = append(t.Values.ValueFiles, filepath.Join(path, vf))
	}
	for _, v := range []string{"values", "valuesObject"} {
		if x := get(helm, v); x != nil && x != "" {
			f, err := write("argo-"+name, x)
			if err != nil {
				return nil, "", fmt.Errorf("discover apps: %w", err)
			}
			t.Values.ValueFiles = append(t.Values.ValueFiles, f)
		}
	}
	for _, p := range list(get(helm, "parameters")) {
		kv := str(p, "name") + "=" + fmt.Sprint(get(p, "value"))
		if get(p, "forceString") == true {
			t.Values.StringValues = append(t.Values.StringValues, kv)
		} else {
			t.Values.Values = append(t.Values.Values, kv)
		}
	}

	return &t, "", nil
}
