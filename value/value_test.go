package value

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"helm.sh/helm/v4/pkg/cli"

	"github.com/semirm-dev/kwhy/render"
)

const (
	mini    = "../testdata/charts/mini"
	prod    = "../testdata/values-prod.yaml"
	anchors = "../testdata/values-anchors.yaml"
)

func loadValues(t *testing.T, files, sets []string) *Values {
	t.Helper()

	c, err := render.LoadChart(mini, "", cli.New())
	if err != nil {
		t.Fatal(err)
	}

	v, err := LoadValues(c, files, sets)
	if err != nil {
		t.Fatal(err)
	}

	return v
}

func lookup(t *testing.T, v *Values, path ...any) Leaf {
	t.Helper()

	l, ok := v.Lookup(path)
	if !ok {
		t.Fatalf("no leaf %s", PathString(path))
	}

	return l
}

// writeFiles writes name → content files to a temp dir and returns their paths in the given order.
func writeFiles(t *testing.T, files ...[2]string) []string {
	t.Helper()

	dir := t.TempDir()
	var paths []string
	for _, f := range files {
		p := filepath.Join(dir, f[0])
		if err := os.WriteFile(p, []byte(f[1]), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}

	return paths
}

func TestChainOverride(t *testing.T) {
	l := lookup(t, loadValues(t, []string{prod}, nil), "replicaCount")

	want := []Source{{prod, 1, float64(3)}, {"mini/values.yaml", 1, float64(1)}}
	if l.Value != float64(3) || !reflect.DeepEqual(l.Chain, want) {
		t.Fatalf("leaf = %+v, want chain %+v", l, want)
	}
}

func TestSubchartDefaultChain(t *testing.T) {
	l := lookup(t, loadValues(t, []string{prod}, nil), "sub", "greeting")

	want := []Source{{prod, 8, "hi"}, {"mini/charts/sub/values.yaml", 1, "hello"}}
	if !reflect.DeepEqual(l.Chain, want) {
		t.Fatalf("chain = %+v, want %+v", l.Chain, want)
	}
}

func TestNullDeletes(t *testing.T) {
	v := loadValues(t, writeFiles(t, [2]string{"null.yaml", "resources: null\n"}), nil)

	if _, ok := v.Lookup([]any{"resources", "requests", "memory"}); ok {
		t.Fatal("resources: null should delete the chart default")
	}
}

func TestListsReplace(t *testing.T) {
	files := writeFiles(t,
		[2]string{"base.yaml", "ports:\n  - p: 1\n  - p: 2\n"},
		[2]string{"top.yaml", "ports:\n  - p: 3\n"},
	)
	v := loadValues(t, files, nil)

	l := lookup(t, v, "ports", 0, "p")
	if want := []Source{{files[1], 2, float64(3)}}; !reflect.DeepEqual(l.Chain, want) {
		t.Fatalf("chain = %+v, want %+v (lists replace, never override item by item)", l.Chain, want)
	}

	if _, ok := v.Lookup([]any{"ports", 1, "p"}); ok {
		t.Fatal("ports[1] should be gone: the top list replaced the base list")
	}
}

func TestAnchors(t *testing.T) {
	l := lookup(t, loadValues(t, []string{anchors}, nil), "resources", "requests", "memory")

	if l.Value != "4Gi" || l.Chain[0] != (Source{anchors, 2, "4Gi"}) {
		t.Fatalf("leaf = %+v, want winner at the anchor line 2", l)
	}
}

func TestMergeKeys(t *testing.T) {
	files := writeFiles(t, [2]string{"merge.yaml", "defaults: &d\n  size: 1Gi\n  tier: low\ndb:\n  <<: *d\n  tier: high\n"})
	v := loadValues(t, files, nil)

	if got := lookup(t, v, "db", "size").Chain[0]; got != (Source{files[0], 2, "1Gi"}) {
		t.Fatalf("merged size = %+v, want line 2", got)
	}

	if got := lookup(t, v, "db", "tier").Chain[0]; got != (Source{files[0], 6, "high"}) {
		t.Fatalf("explicit key over merge = %+v, want line 6", got)
	}
}

func TestSetFlags(t *testing.T) {
	v := loadValues(t, []string{prod}, []string{"image.tag=9.9", `a\.b=1`, "list[0].x=2"})

	for _, tt := range []struct {
		path []any
		line int
	}{
		{[]any{"image", "tag"}, 0},
		{[]any{"a.b"}, 1},
		{[]any{"list", 0, "x"}, 2},
	} {
		if got := lookup(t, v, tt.path...).Chain[0]; got.File != "--set" || got.Line != tt.line {
			t.Errorf("%s winner = %+v, want --set flag %d", PathString(tt.path), got, tt.line)
		}
	}
}

func TestWinnerMatchesHelm(t *testing.T) {
	v := loadValues(t, []string{prod, anchors}, []string{"replicaCount=5"})

	if len(v.Leaves) == 0 {
		t.Fatal("no leaves")
	}

	for i, l := range v.Leaves {
		if l.ID != i {
			t.Errorf("%s: ID %d, want %d", PathString(l.Path), l.ID, i)
		}

		if len(l.Chain) == 0 || !reflect.DeepEqual(l.Chain[0].Value, l.Value) {
			t.Errorf("%s: value %#v, chain %+v", PathString(l.Path), l.Value, l.Chain)
		}

		if strings.HasPrefix(PathString(l.Path), "sub.global.") {
			t.Errorf("%s: subchart copy of a global is not a leaf", PathString(l.Path))
		}
	}
}

func TestPathString(t *testing.T) {
	tests := []struct {
		path []any
		want string
	}{
		{[]any{"resources", "requests", "memory"}, "resources.requests.memory"},
		{[]any{"list", 0, "x"}, "list[0].x"},
		{[]any{"a.b", "c"}, `a\.b.c`},
	}
	for _, tt := range tests {
		if got := PathString(tt.path); got != tt.want {
			t.Errorf("PathString(%v) = %q, want %q", tt.path, got, tt.want)
		}
	}
}
