package change

import (
	"bytes"
	"cmp"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Discover finds Argo CD Applications and Flux Kustomizations and HelmReleases in the YAML under dir and turns each
// into a target. Their paths are relative to the repo checkout at tree, and never leave it.
// remote is this repo's git remote URL: sources from another repo are skipped ("" accepts every repoURL). What
// can't be rendered from this repo is skipped with a warning naming the object and why, never guessed. Inline
// values are written to temp files under tmp, which the caller removes.
func Discover(tree, dir, remote, tmp string) (ts []Target, warnings []string, err error) {
	objs, err := readObjects(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("discover apps: %w", err)
	}

	gitRepos := map[string]string{} // GitRepository name → url
	for _, o := range objs {
		if o["kind"] == "GitRepository" {
			gitRepos[str(o, "metadata", "name")] = str(o, "spec", "url")
		}
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
				id := "Application " + name
				if len(srcs) > 1 {
					id += fmt.Sprintf(" source %d", i+1)
				}

				t, warn, err := argoTarget(s, tree, name, str(o, "spec", "destination", "namespace"), remote, id, tmp)
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
			if why := notThisRepo(get(o, "spec", "sourceRef"), "source is", gitRepos, remote); why != "" {
				warnings = append(warnings, "Kustomization "+name+": "+why+": skipped")
				continue
			}

			ts = append(ts, Target{Manifests: inRepo(str(o, "spec", "path")), Release: name,
				Namespace: cmp.Or(str(o, "spec", "targetNamespace"), "default")})

		case o["kind"] == "HelmRelease" && strings.HasPrefix(apiVersion, "helm.toolkit.fluxcd.io/"):
			id := "HelmRelease " + name
			spec := get(o, "spec", "chart", "spec")
			if why := notThisRepo(get(spec, "sourceRef"), "chart from", gitRepos, remote); why != "" {
				warnings = append(warnings, id+": "+why+": skipped")
				continue
			}

			t, err := fluxHelmTarget(o, spec, name, tmp)
			if err != nil {
				return nil, nil, err
			}
			ts = append(ts, t)

			if get(o, "spec", "valuesFrom") != nil {
				warnings = append(warnings, id+": valuesFrom (ConfigMaps, Secrets in the cluster) isn't read; the render may differ")
			}
		}
	}

	return ts, warnings, nil
}

// readObjects decodes every YAML document under dir; files that aren't manifests are passed over.
func readObjects(dir string) ([]obj, error) {
	var objs []obj
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
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

	return objs, err
}

// argoTarget turns one Argo CD source into a target, or a warning saying why it can't.
func argoTarget(s any, tree, name, ns, remote, id, tmp string) (*Target, string, error) {
	if str(s, "chart") != "" {
		return nil, fmt.Sprintf("%s: chart %s from a Helm repository, not this repo: skipped", id, str(s, "chart")), nil
	}
	if url := str(s, "repoURL"); remote != "" && !sameRepo(url, remote) {
		return nil, fmt.Sprintf("%s: source %s, not this repo: skipped", id, url), nil
	}

	if str(s, "path") == "" {
		if str(s, "ref") != "" {
			return nil, "", nil // a values-only source other sources refer to as $ref
		}
		return nil, fmt.Sprintf("%s: no path: skipped", id), nil
	}

	path := inRepo(str(s, "path"))
	helm := get(s, "helm")
	t := Target{Release: name, Namespace: cmp.Or(ns, "default")}
	if !isChart(filepath.Join(tree, path)) && helm == nil { // plain YAML or a Kustomize overlay
		t.Manifests = path
		return &t, "", nil
	}

	t.Chart = path
	t.Release = cmp.Or(str(helm, "releaseName"), name)

	// Value files are relative to the source's path; a leading / is the repo root.
	for _, f := range list(get(helm, "valueFiles")) {
		vf := fmt.Sprint(f)
		if strings.HasPrefix(vf, "$") {
			return nil, fmt.Sprintf("%s: value file %s from another source isn't supported: skipped", id, vf), nil
		}
		if !strings.HasPrefix(vf, "/") {
			vf = filepath.Join(path, vf)
		}
		t.Values.ValueFiles = append(t.Values.ValueFiles, inRepo(vf))
	}

	for _, v := range []string{"values", "valuesObject"} {
		if x := get(helm, v); x != nil && x != "" {
			f, err := writeValues(tmp, "argo-"+name, x)
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

// fluxHelmTarget turns a Flux HelmRelease o, whose chart spec is spec, into a target.
func fluxHelmTarget(o obj, spec any, name, tmp string) (Target, error) {
	targetNS := str(o, "spec", "targetNamespace")
	release := str(o, "spec", "releaseName")
	if release == "" {
		release = name
		if targetNS != "" {
			release = targetNS + "-" + name
		}
	}

	t := Target{Chart: inRepo(str(spec, "chart")), Release: release,
		Namespace: cmp.Or(targetNS, str(o, "metadata", "namespace"), "default")}

	// valuesFiles are relative to the repo root.
	for _, f := range list(get(spec, "valuesFiles")) {
		t.Values.ValueFiles = append(t.Values.ValueFiles, inRepo(fmt.Sprint(f)))
	}

	if v := get(o, "spec", "values"); v != nil {
		f, err := writeValues(tmp, "flux-"+name, v)
		if err != nil {
			return Target{}, fmt.Errorf("discover apps: %w", err)
		}
		t.Values.ValueFiles = append(t.Values.ValueFiles, f)
	}

	return t, nil
}

// notThisRepo says why a Flux sourceRef doesn't point at this repo, "" when it does. what starts the reason for a
// source of another kind ("source is a HelmRepository").
func notThisRepo(ref any, what string, gitRepos map[string]string, remote string) string {
	if kind := str(ref, "kind"); kind != "GitRepository" {
		return fmt.Sprintf("%s a %s, not this repo", what, kind)
	}

	name := str(ref, "name")
	if url, ok := gitRepos[name]; ok && remote != "" && !sameRepo(url, remote) {
		return fmt.Sprintf("GitRepository %s is %s, not this repo", name, url)
	}

	return ""
}

// writeValues writes inline values (a YAML string or an object) to a new file under tmp. A unique file each: sources
// of one app, and values next to valuesObject, share the app's name.
func writeValues(tmp, name string, values any) (string, error) {
	data, ok := values.(string)
	if !ok {
		b, err := yaml.Marshal(values)
		if err != nil {
			return "", err
		}
		data = string(b)
	}

	f, err := os.CreateTemp(tmp, strings.ReplaceAll(name, "/", "_")+"-*.values.yaml")
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(data); err != nil {
		f.Close()
		return "", err
	}

	return f.Name(), f.Close()
}

// inRepo is p as Argo CD and Flux read a repo path: from the repo root, where a leading / is the root and .. stops
// there. A path from a pull request never reaches past the checkout.
func inRepo(p string) string {
	clean := strings.TrimPrefix(path.Join("/", filepath.ToSlash(p)), "/")
	return filepath.FromSlash(cmp.Or(clean, "."))
}
