package value_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/semirm-dev/kwhy/value"
)

const fixture = "../testdata/kustomize/fixture"

func TestOverlay(t *testing.T) {
	t.Chdir(fixture)
	o, err := value.LoadOverlay(t.Context(), ".", "overlays/prod", "")
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
			a, err := o.Explain(value.KField{Doc: d.ID, Path: p})
			if err != nil || len(a.Leaves) != 1 || a.Leaves[0].Chain[0].Line != tc.line {
				t.Errorf("%s %s: %+v, %v; want line %d", tc.doc, tc.path, a, err, tc.line)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", tc.path, err)
		}
		a, err := o.Explain(value.KField{Doc: d.ID, Path: p})
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
	o, err := value.LoadOverlay(t.Context(), ".", "overlays/prod", "")
	if err != nil {
		t.Fatal(err)
	}

	for _, d := range o.Docs {
		as, err := o.ExplainAll(d.ID)
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

// A cached load answers exactly like a fresh one, and any edited file is a new key.
func TestOverlayCache(t *testing.T) {
	repo := t.TempDir()
	if err := os.CopyFS(repo, os.DirFS(fixture)); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	cache := t.TempDir()

	answers := func() []value.Answer {
		t.Helper()
		o, err := value.LoadOverlay(t.Context(), ".", "overlays/prod", cache)
		if err != nil {
			t.Fatal(err)
		}
		var all []value.Answer
		for _, d := range o.Docs {
			as, err := o.ExplainAll(d.ID)
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, as...)
		}
		return all
	}

	fresh := answers()
	files, _ := filepath.Glob(filepath.Join(cache, "kustomize-*.gob"))
	if len(files) != 1 {
		t.Fatalf("cache files = %v, want one", files)
	}
	if cached := answers(); !reflect.DeepEqual(cached, fresh) {
		t.Errorf("cached answers differ:\n%+v\nwant\n%+v", cached, fresh)
	}

	f, err := os.OpenFile("overlays/prod/memory.yaml", os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("# edited\n")
	f.Close()
	answers()
	if files, _ = filepath.Glob(filepath.Join(cache, "kustomize-*.gob")); len(files) != 2 {
		t.Errorf("after an edit, cache files = %v, want two", files)
	}
}

// An overlay at the root itself ("kwhy value kustomize .") loads.
func TestOverlayAtRoot(t *testing.T) {
	t.Chdir(filepath.Join(fixture, "base"))
	o, err := value.LoadOverlay(t.Context(), ".", ".", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Docs) == 0 {
		t.Error("no documents")
	}
}

// A replacement that writes the value the resource's own line already shows: the line shows the value but its edit
// doesn't move the field. Confirm's rebuild catches that; the guard alone can't.
func TestOverlayConfirm(t *testing.T) {
	dir := t.TempDir()
	for name, content := range map[string]string{
		"kustomization.yaml": "resources: [cm.yaml, deploy.yaml]\nreplacements:\n- source: {kind: ConfigMap, name: cfg, fieldPath: data.host}\n" +
			"  targets: [{select: {kind: Deployment}, fieldPaths: [metadata.annotations.host]}]\n",
		"cm.yaml":     "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: cfg}\ndata:\n  host: db.prod\n",
		"deploy.yaml": "apiVersion: apps/v1\nkind: Deployment\nmetadata:\n  name: api\n  annotations:\n    host: db.prod\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	o, err := value.LoadOverlay(t.Context(), ".", ".", "")
	if err != nil {
		t.Fatal(err)
	}
	d, err := o.Select("deploy/api")
	if err != nil {
		t.Fatal(err)
	}
	p, err := o.FieldPath(d, ".metadata.annotations.host")
	if err != nil {
		t.Fatal(err)
	}

	for _, confirm := range []bool{false, true} {
		o.Confirm = confirm
		a, err := o.Explain(value.KField{Doc: d.ID, Path: p})
		if err != nil {
			t.Fatal(err)
		}
		if confirm {
			for _, l := range a.Leaves {
				if src := l.Chain[0]; src.File == "deploy.yaml" && src.Line != 0 {
					t.Errorf("confirm names deploy.yaml:%d, whose edit doesn't move the field", src.Line)
				}
			}
		}
	}
}
