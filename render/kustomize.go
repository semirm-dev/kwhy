package render

import (
	"fmt"

	"sigs.k8s.io/kustomize/api/krusty"
	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// Kustomize builds the overlay in dir with Kustomize's own library, as `kubectl kustomize` does, into documents.
// Objects without a namespace get namespace. helmCharts and plugins stay disabled (Kustomize's defaults).
func Kustomize(dir, namespace string) ([]Doc, error) {
	rm, err := krusty.MakeKustomizer(krusty.MakeDefaultOptions()).Run(filesys.MakeFsOnDisk(), dir)
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
