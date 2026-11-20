package render

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

// ReadManifests reads every .yaml and .yml file under dir, in path order, into documents. Objects without a
// namespace get namespace. A `kind: List` is read as its items. A directory with a kustomization file is refused:
// its files aren't what gets applied.
func ReadManifests(dir, namespace string) ([]Doc, error) {
	var docs []Doc
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != dir && strings.HasPrefix(d.Name(), ".") {
				return fs.SkipDir
			}
			return nil
		}

		switch strings.ToLower(d.Name()) {
		case "kustomization.yaml", "kustomization.yml", "kustomization":
			return fmt.Errorf("%s is a Kustomize directory, which kwhy doesn't support yet", filepath.Dir(path))
		}
		if ext := filepath.Ext(path); ext != ".yaml" && ext != ".yml" {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}

		dec := yaml.NewDecoder(bytes.NewReader(data))
		for {
			var obj map[string]any
			if err := dec.Decode(&obj); errors.Is(err, io.EOF) {
				return nil
			} else if err != nil {
				return fmt.Errorf("decode %s: %w", rel, err)
			}

			items := []any{obj}
			if obj["kind"] == "List" {
				items, _ = obj["items"].([]any)
			}
			for _, it := range items {
				if m, ok := it.(map[string]any); ok && m != nil {
					docs = append(docs, newDoc(rel, m, namespace))
				}
			}
		}
	})
	if err != nil {
		return nil, fmt.Errorf("read manifests: %w", err)
	}

	return docs, nil
}
