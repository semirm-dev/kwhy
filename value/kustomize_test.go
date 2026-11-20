package value_test

import (
	"path/filepath"
	"testing"

	"github.com/semirm-dev/kwhy/value"
)

const fixture = "../testdata/kustomize/fixture"

func TestOverlay(t *testing.T) {
	t.Chdir(fixture)
	o, err := value.LoadOverlay(".", "overlays/prod")
	if err != nil {
		t.Fatal(err)
	}
	d, err := o.Select("deploy/api")
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		path, value, file string
		line              int
		kind              value.Kind
		doc               string
	}{
		{".data.LOG_LEVEL", "warn", "overlays/prod/kustomization.yaml", 26, value.Direct, "cm/api-config"},
		{".spec.replicas", "3", "overlays/prod/kustomization.yaml", 12, value.Direct, ""},
		{".spec.template.spec.containers[name=api].resources.requests.memory", "2Gi", "overlays/prod/memory.yaml", 12, value.Direct, ""},
		{".spec.template.spec.containers[name=api].resources.requests.cpu", "500m", "overlays/prod/kustomization.yaml", 21, value.Direct, ""},
		{".spec.template.spec.containers[name=api].image", "ghcr.io/acme/api:2.3.1", "overlays/prod/kustomization.yaml", 9, value.Direct, ""},
		{`.spec.template.metadata.annotations["prometheus.io/scrape"]`, "true", "components/monitoring/annotations.yaml", 9, value.Direct, ""},
		{".metadata.name", "prod-api", "overlays/prod/kustomization.yaml", 4, value.Direct, ""},
		{".metadata.namespace", "prod", "overlays/prod/kustomization.yaml", 3, value.Direct, ""},
		{".spec.template.spec.containers[name=api].name", "api", "base/deployment.yaml", 16, value.Direct, ""},
	} {
		p, err := o.FieldPath(d, tc.path)
		if tc.doc != "" {
			cm, err := o.Select(tc.doc)
			if err != nil {
				t.Fatal(err)
			}
			d := cm
			p, err = o.FieldPath(d, tc.path)
			if err != nil {
				t.Fatal(err)
			}
			a, err := o.ExplainOverlay(value.KField{Doc: d.ID, Path: p})
			if err != nil || len(a.Leaves) != 1 || a.Leaves[0].Chain[0].Line != tc.line {
				t.Errorf("%s %s: %+v, %v; want line %d", tc.doc, tc.path, a, err, tc.line)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		a, err := o.ExplainOverlay(value.KField{Doc: d.ID, Path: p})
		if err != nil {
			t.Fatal(err)
		}
		if len(a.Leaves) != 1 {
			t.Errorf("%s: %s %+v, want one source", tc.path, a.Kind, a.Leaves)
			continue
		}
		src := a.Leaves[0].Chain[0]
		if a.Kind != tc.kind || a.Value != tc.value && a.Value != 3 || filepath.ToSlash(src.File) != tc.file || src.Line != tc.line {
			t.Errorf("%s = %v %s %s:%d (%v), want %s %s:%d", tc.path, a.Value, a.Kind, src.File, src.Line, a.Leaves[0].Entry,
				tc.value, tc.file, tc.line)
		}
	}
}

// The guard: every line the overlay names shows the field's value; nothing is named on a guess.
func TestOverlayLinesShowValues(t *testing.T) {
	t.Chdir(fixture)
	o, err := value.LoadOverlay(".", "overlays/prod")
	if err != nil {
		t.Fatal(err)
	}

	for _, d := range o.Docs {
		as, err := o.ExplainAllOverlay(d.ID)
		if err != nil {
			t.Fatal(err)
		}
		for _, a := range as {
			for _, l := range a.Leaves {
				if s := l.Chain[0]; s.Line > 0 && a.Kind == value.Direct && !o.LineShows(s.File, s.Line, a.Value) {
					t.Errorf("%s %s = %v names %s:%d, which doesn't show it", a.Resource, a.Field.Path, a.Value, s.File, s.Line)
				}
			}
		}
	}
}
