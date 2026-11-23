package render

import (
	"fmt"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"
	"sigs.k8s.io/kustomize/api/konfig"
	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// fileKeys are the kustomization fields that name files to read; Kustomize fetches a file given as a URL.
var fileKeys = []string{"patches", "patchesStrategicMerge", "patchesJson6902", "configMapGenerator", "secretGenerator",
	"crds", "configurations", "replacements", "openapi", "generators", "transformers", "validators"}

// Kustomize builds the overlay in dir with Kustomize's own library, as `kubectl kustomize` does, into documents.
// Objects without a namespace get namespace. helmCharts and plugins stay disabled (Kustomize's defaults), and a
// stack that would fetch anything, or read past root (the checkout; "" for none), is refused (Offline).
func Kustomize(root, dir, namespace string) ([]Doc, error) {
	var err error
	if root != "" {
		if root, err = filepath.EvalSymlinks(root); err != nil {
			return nil, err
		}
		if root, err = filepath.Abs(root); err != nil {
			return nil, err
		}
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return nil, err
	}

	fsys := filesys.MakeFsOnDisk()
	if err := Offline(fsys, root, dir); err != nil {
		return nil, err
	}

	rm, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("kustomize build %s: %w", dir, err)
	}

	var docs []Doc
	for _, r := range rm.Resources() {
		obj, err := r.Map()
		if err != nil {
			return nil, fmt.Errorf("kustomize build %s: %w", dir, err)
		}
		docs = append(docs, newDoc(dir, obj, namespace))
	}

	return docs, nil
}

// Offline refuses a kustomization stack in dir that would make Kustomize reach the network: a resource, base or
// component that isn't a local file or directory (a git repo or a URL), or a URL where a file is read. Kustomize
// would clone or download them, and the stack may come from a pull request. With root (an absolute path without
// symlinks), a directory or resource outside it, through .. or a symlink, is refused too; "" skips that check, for an
// in-memory copy that holds nothing else.
func Offline(fsys filesys.FileSystem, root, dir string) error {
	return offline(fsys, root, dir, map[string]bool{})
}

func offline(fsys filesys.FileSystem, root, dir string, seen map[string]bool) error {
	if seen[dir] {
		return nil
	}
	seen[dir] = true

	if !inside(root, dir) {
		return fmt.Errorf("%s is outside the repo (kwhy reads only the checkout)", dir)
	}

	file := ""
	for _, n := range konfig.RecognizedKustomizationFileNames() {
		if fsys.Exists(filepath.Join(dir, n)) {
			file = filepath.Join(dir, n)
			break
		}
	}
	if file == "" {
		return nil // a plain resource directory, or none: Kustomize reports it
	}

	data, err := fsys.ReadFile(file)
	if err != nil {
		return fmt.Errorf("read %s: %w", file, err)
	}

	var k map[string]any
	if err := yaml.Unmarshal(data, &k); err != nil {
		return fmt.Errorf("read %s: %w", file, err)
	}

	for _, key := range []string{"resources", "bases", "components"} {
		l, _ := k[key].([]any)
		for _, e := range l {
			p := filepath.Join(dir, fmt.Sprint(e))
			if fsys.IsDir(p) {
				if err := offline(fsys, root, p, seen); err != nil {
					return err
				}
				continue
			}
			if !fsys.Exists(p) {
				return fmt.Errorf("%s: %v is neither a local file nor a directory (kwhy doesn't fetch remote bases or URLs)", file, e)
			}
			if !inside(root, p) {
				return fmt.Errorf("%s: %v is outside the repo (kwhy reads only the checkout)", file, e)
			}
		}
	}

	for _, key := range fileKeys {
		if u := remoteURL(k[key]); u != "" {
			return fmt.Errorf("%s: %s is a URL (kwhy doesn't fetch remote bases or URLs)", file, u)
		}
	}

	return nil
}

// inside reports whether p, symlinks followed, is root or under it; root "" holds everything.
func inside(root, p string) bool {
	if root == "" {
		return true
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(root, real)

	return err == nil && filepath.IsLocal(rel)
}

// remoteURL is the first http(s) URL among v's strings, as a whole or as the path of key=path, "" for none.
func remoteURL(v any) string {
	switch v := v.(type) {
	case string:
		_, after, _ := strings.Cut(v, "=")
		for _, s := range []string{v, after} {
			if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
				return v
			}
		}
	case []any:
		for _, e := range v {
			if u := remoteURL(e); u != "" {
				return u
			}
		}
	case map[string]any:
		for _, e := range v {
			if u := remoteURL(e); u != "" {
				return u
			}
		}
	}

	return ""
}
