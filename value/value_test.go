package value_test

import (
	"bytes"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/cli/values"

	"github.com/semirm-dev/kwhy/render"
	"github.com/semirm-dev/kwhy/value"
)

const (
	mini    = "../testdata/charts/mini"
	prod    = "../testdata/values-prod.yaml"
	anchors = "../testdata/values-anchors.yaml"
)

var update = flag.Bool("update", false, "rewrite golden files")

func TestChainOverride(t *testing.T) {
	l := lookup(t, loadValues(t, []string{prod}, nil), "replicaCount")

	want := []value.Source{{prod, 1, float64(3), false}, {"mini/values.yaml", 1, float64(1), true}}
	if l.Value != float64(3) || !reflect.DeepEqual(l.Chain, want) {
		t.Fatalf("leaf = %+v, want chain %+v", l, want)
	}
}

func TestValuesURL(t *testing.T) {
	srv := httptest.NewServer(http.FileServer(http.Dir("../testdata")))
	defer srv.Close()

	url := srv.URL + "/values-prod.yaml"
	l := lookup(t, loadValues(t, []string{url}, nil), "replicaCount")
	if want := (value.Source{url, 1, float64(3), false}); l.Chain[0] != want {
		t.Fatalf("chain[0] = %+v, want %+v", l.Chain[0], want)
	}
}

// A chain that never reaches a chart file has no default entry.
func TestHumanNoDefaultInUserChain(t *testing.T) {
	a := value.Answer{Resource: "Deployment/rel-api", Field: value.Field{Path: ".spec.replicas"}, Value: 3, Kind: value.Direct,
		Leaves: []value.Leaf{{Path: []any{"x"}, Value: 3, Chain: []value.Source{{"b.yaml", 1, 3, false}, {"a.yaml", 1, 2, false}}}}}

	var b bytes.Buffer
	if err := value.WriteHuman(&b, []value.Answer{a}); err != nil {
		t.Fatal(err)
	}
	if out := b.String(); strings.Contains(out, "default") || !strings.Contains(out, "└─ overrides") {
		t.Fatalf("output:\n%s", out)
	}
}

func TestSubchartDefaultChain(t *testing.T) {
	l := lookup(t, loadValues(t, []string{prod}, nil), "sub", "greeting")

	want := []value.Source{{prod, 8, "hi", false}, {"mini/charts/sub/values.yaml", 1, "hello", true}}
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
	if want := []value.Source{{files[1], 2, float64(3), false}}; !reflect.DeepEqual(l.Chain, want) {
		t.Fatalf("chain = %+v, want %+v (lists replace, never override item by item)", l.Chain, want)
	}

	if _, ok := v.Lookup([]any{"ports", 1, "p"}); ok {
		t.Fatal("ports[1] should be gone: the top list replaced the base list")
	}
}

func TestAnchors(t *testing.T) {
	l := lookup(t, loadValues(t, []string{anchors}, nil), "resources", "requests", "memory")

	if l.Value != "4Gi" || l.Chain[0] != (value.Source{anchors, 2, "4Gi", false}) {
		t.Fatalf("leaf = %+v, want winner at the anchor line 2", l)
	}
}

func TestMergeKeys(t *testing.T) {
	files := writeFiles(t, [2]string{"merge.yaml", "defaults: &d\n  size: 1Gi\n  tier: low\ndb:\n  <<: *d\n  tier: high\n"})
	v := loadValues(t, files, nil)

	if got := lookup(t, v, "db", "size").Chain[0]; got != (value.Source{files[0], 2, "1Gi", false}) {
		t.Fatalf("merged size = %+v, want line 2", got)
	}

	if got := lookup(t, v, "db", "tier").Chain[0]; got != (value.Source{files[0], 6, "high", false}) {
		t.Fatalf("explicit key over merge = %+v, want line 6", got)
	}
}

func TestSetFlags(t *testing.T) {
	sets := []string{"image.tag=9.9", `a\.b=1`, "list[0].x=2"}
	v := loadValues(t, []string{prod}, sets)

	for _, tt := range []struct {
		path []any
		line int
	}{
		{[]any{"image", "tag"}, 0},
		{[]any{"a.b"}, 1},
		{[]any{"list", 0, "x"}, 2},
	} {
		if got := lookup(t, v, tt.path...).Chain[0]; got.File != "--set "+sets[tt.line] || got.Line != tt.line {
			t.Errorf("%s winner = %+v, want --set flag %d", value.PathString(tt.path), got, tt.line)
		}
	}
}

// Every --set flag kind is a source, applied in Helm's order (json, set, string, file, literal) whatever the
// order on the command line, and the winner always equals what Helm merged.
func TestSetFlagKinds(t *testing.T) {
	c, err := render.LoadChart(mini, "", cli.New())
	if err != nil {
		t.Fatal(err)
	}

	file := writeFiles(t, [2]string{"level.txt", "debug"})[0]
	v, err := value.LoadValues(c, values.Options{
		JSONValues:    []string{`replicaCount=7`, `{"auth":{"user":"root"}}`},
		Values:        []string{"replicaCount=8", "image.tag=9.9"},
		StringValues:  []string{"image.tag=10"},
		FileValues:    []string{"config.logLevel=" + file},
		LiteralValues: []string{"ingress.host=a,b{c}"},
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		path  []any
		file  string
		value any
	}{
		{[]any{"replicaCount"}, "--set replicaCount=8", int64(8)},
		{[]any{"auth", "user"}, `--set-json {"auth":{"user":"root"}}`, "root"},
		{[]any{"image", "tag"}, "--set-string image.tag=10", "10"},
		{[]any{"config", "logLevel"}, "--set-file config.logLevel=" + file, "debug"},
		{[]any{"ingress", "host"}, "--set-literal ingress.host=a,b{c}", "a,b{c}"},
	} {
		l := lookup(t, v, tt.path...)
		if got := l.Chain[0]; got.File != tt.file || !reflect.DeepEqual(l.Value, tt.value) {
			t.Errorf("%s: winner %+v value %#v, want %s = %#v", value.PathString(tt.path), got, l.Value, tt.file, tt.value)
		}
	}

	if chain := lookup(t, v, "replicaCount").Chain; len(chain) < 2 || chain[1].File != "--set-json replicaCount=7" {
		t.Errorf("replicaCount chain %+v: --set should override --set-json", chain)
	}
}

func TestWinnerMatchesHelm(t *testing.T) {
	v := loadValues(t, []string{prod, anchors}, []string{"replicaCount=5"})

	if len(v.Leaves) == 0 {
		t.Fatal("no leaves")
	}

	for i, l := range v.Leaves {
		if l.ID != i {
			t.Errorf("%s: ID %d, want %d", value.PathString(l.Path), l.ID, i)
		}

		if len(l.Chain) == 0 || !reflect.DeepEqual(l.Chain[0].Value, l.Value) {
			t.Errorf("%s: value %#v, chain %+v", value.PathString(l.Path), l.Value, l.Chain)
		}

		if strings.HasPrefix(value.PathString(l.Path), "sub.global.") {
			t.Errorf("%s: subchart copy of a global is not a leaf", value.PathString(l.Path))
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
		if got := value.PathString(tt.path); got != tt.want {
			t.Errorf("value.PathString(%v) = %q, want %q", tt.path, got, tt.want)
		}
	}
}

func TestFlatten(t *testing.T) {
	docs := []render.Doc{{Object: map[string]any{
		"spec":     map[string]any{"replicas": 3, "ports": []any{map[string]any{"port": 80}}},
		"metadata": map[string]any{"annotations": map[string]any{"app.kubernetes.io/name": "x"}},
	}}}

	got := value.Flatten(docs)
	want := map[value.Field]any{
		{0, ".spec.replicas"}:                                  3,
		{0, ".spec.ports[0].port"}:                             80,
		{0, `.metadata.annotations["app.kubernetes.io/name"]`}: "x",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestResolveByName(t *testing.T) {
	d := selectDoc(t, renderMini(t), "deploy/api")

	got, err := value.ResolvePath(d.Object, ".spec.template.spec.containers[name=api].image")
	if err != nil {
		t.Fatal(err)
	}
	if got != ".spec.template.spec.containers[0].image" {
		t.Fatalf("got %s", got)
	}
}

func TestResolveQuotedKey(t *testing.T) {
	d := selectDoc(t, renderMini(t), "deploy/api")

	got, err := value.ResolvePath(d.Object, `.spec.template.metadata.annotations["checksum/config"]`)
	if err != nil {
		t.Fatal(err)
	}
	if got != ".spec.template.metadata.annotations.checksum/config" {
		t.Fatalf("got %s", got)
	}
}

func TestResolveUnknown(t *testing.T) {
	d := selectDoc(t, renderMini(t), "deploy/api")

	if _, err := value.ResolvePath(d.Object, ".spec.replica"); err == nil {
		t.Fatal("want error")
	}

	var valid []string
	for f := range value.Flatten([]render.Doc{d}) {
		valid = append(valid, f.Path)
	}
	if got := value.Suggest(valid, ".spec.replica", 3); len(got) == 0 || got[0] != ".spec.replicas" {
		t.Fatalf("got %v", got)
	}
}

func TestSelectKindAlias(t *testing.T) {
	docs := renderMini(t)

	for sel, kind := range map[string]string{"deploy/api": "Deployment", "svc/api": "Service", "Deployment/rel-api": "Deployment", "service/rel-api": "Service"} {
		if got := selectDoc(t, docs, sel).Kind; got != kind {
			t.Errorf("%s: got %s, want %s", sel, got, kind)
		}
	}
}

func TestSelectAmbiguousNamespace(t *testing.T) {
	docs := []render.Doc{
		{Kind: "Deployment", Name: "x", Namespace: "a"},
		{Kind: "Deployment", Name: "x", Namespace: "b"},
	}

	_, err := value.SelectDoc(docs, "deploy/x", "")
	if err == nil || err.Error() != "deployment/x exists in namespaces a, b: use --namespace" {
		t.Fatalf("got %v", err)
	}

	i, err := value.SelectDoc(docs, "deploy/x", "b")
	if err != nil || i != 1 {
		t.Fatalf("got %d, %v", i, err)
	}
}

func TestSelectUnknownListsResources(t *testing.T) {
	_, err := value.SelectDoc(renderMini(t), "deploy/nope", "")
	if err == nil || !strings.Contains(err.Error(), "resources: ConfigMap/rel-config, ConfigMap/rel-sub, Deployment/rel-api, Secret/rel-auth, Service/rel-api") {
		t.Fatalf("got %v", err)
	}
}

func TestTraceDirect(t *testing.T) {
	idx, v, _ := trace(t, mini, prod)

	f := field(t, idx, "deploy/api", ".spec.replicas")
	if want := []value.Hit{{Leaf: lookup(t, v, "replicaCount").ID, Exact: true}}; !reflect.DeepEqual(idx.Hits[f], want) {
		t.Fatalf("got %+v, want %+v", idx.Hits[f], want)
	}
}

// Trace skips empty strings (image.tag: ""); Explain finds them (TestExplain).
func TestTraceComposed(t *testing.T) {
	idx, v, _ := trace(t, mini, prod)

	f := field(t, idx, "deploy/api", ".spec.template.spec.containers[name=api].image")
	if want := []value.Hit{{Leaf: lookup(t, v, "image", "repository").ID}}; !reflect.DeepEqual(idx.Hits[f], want) {
		t.Fatalf("got %+v, want %+v", idx.Hits[f], want)
	}
}

func TestTraceEncoded(t *testing.T) {
	idx, v, _ := trace(t, mini, prod)

	f := field(t, idx, "secret/auth", ".data.user")
	if want := []value.Hit{{Leaf: lookup(t, v, "auth", "user").ID, Encoded: true}}; !reflect.DeepEqual(idx.Hits[f], want) {
		t.Fatalf("got %+v, want %+v", idx.Hits[f], want)
	}
}

func TestTraceRandom(t *testing.T) {
	idx, _, _ := trace(t, mini, prod)

	if f := field(t, idx, "secret/auth", ".data.password"); !idx.Random[f] {
		t.Fatal("password not random")
	}
	if f := field(t, idx, "secret/auth", ".data.user"); idx.Random[f] {
		t.Fatal("user random")
	}
}

func TestTraceLargeNumber(t *testing.T) {
	idx, v, _ := trace(t, mini, prod)

	f := field(t, idx, "deploy/api", ".spec.template.spec.containers[0].args[0]")
	if idx.Fields[f] != "--max-body=2e+06" {
		t.Fatalf("baseline %v", idx.Fields[f])
	}
	if want := []value.Hit{{Leaf: lookup(t, v, "maxBodyBytes").ID}}; !reflect.DeepEqual(idx.Hits[f], want) {
		t.Fatalf("got %+v, want %+v", idx.Hits[f], want)
	}
}

func TestTraceSubchartAndGlobal(t *testing.T) {
	idx, v, _ := trace(t, mini, prod)

	for path, leaf := range map[string][]any{".data.greeting": {"sub", "greeting"}, ".data.env": {"global", "env"}} {
		f := field(t, idx, "cm/sub", path)
		if want := []value.Hit{{Leaf: lookup(t, v, leaf...).ID, Exact: true}}; !reflect.DeepEqual(idx.Hits[f], want) {
			t.Errorf("%s: got %+v, want %+v", path, idx.Hits[f], want)
		}
	}
}

func TestTraceStructuralLeafIsolated(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mini")
	if err := os.CopyFS(dir, os.DirFS(mini)); err != nil {
		t.Fatal(err)
	}
	tpl := "apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: mode\ndata:\n  y: z\n{{- if eq .Values.mode \"fast\" }}\n  x: \"1\"\n{{- end }}\n"
	if err := os.WriteFile(filepath.Join(dir, "templates", "mode.yaml"), []byte(tpl), 0o644); err != nil {
		t.Fatal(err)
	}
	files := writeFiles(t, [2]string{"mode.yaml", "mode: fast\n"})

	idx, v, _ := trace(t, dir, prod, files[0])

	if got := idx.Unmarkable[lookup(t, v, "mode").ID]; got != "changes structure" {
		t.Fatalf("mode: %q", got)
	}
	if len(idx.Unmarkable) != 1 {
		t.Fatalf("unmarkable %v", idx.Unmarkable)
	}

	f := field(t, idx, "deploy/api", ".spec.replicas")
	if want := []value.Hit{{Leaf: lookup(t, v, "replicaCount").ID, Exact: true}}; !reflect.DeepEqual(idx.Hits[f], want) {
		t.Fatalf("got %+v, want %+v", idx.Hits[f], want)
	}
}

func TestMarkerCollisionShift(t *testing.T) {
	files := writeFiles(t, [2]string{"big.yaml", "replicaCount: 900003\n"})
	idx, v, _ := trace(t, mini, files[0])

	f := field(t, idx, "deploy/api", ".spec.replicas")
	if want := []value.Hit{{Leaf: lookup(t, v, "replicaCount").ID, Exact: true}}; !reflect.DeepEqual(idx.Hits[f], want) {
		t.Fatalf("got %+v, want %+v", idx.Hits[f], want)
	}
}

func TestExplainKinds(t *testing.T) {
	idx, v, c := trace(t, mini, prod)

	tests := []struct {
		sel, path string
		kind      value.Kind
		leaves    [][]any
	}{
		{"deploy/api", ".spec.replicas", value.Direct, [][]any{{"replicaCount"}}},
		{"deploy/api", ".spec.template.spec.containers[0].image", value.Composed, [][]any{{"image", "repository"}, {"image", "tag"}}},
		{"secret/auth", ".data.user", value.Encoded, [][]any{{"auth", "user"}}},
		{"secret/auth", ".data.password", value.Random, nil},
		{"deploy/api", `.spec.template.metadata.annotations["checksum/config"]`, value.Hashed, nil},
		{"deploy/api", ".spec.template.spec.containers[0].env[0].value", value.Derived, [][]any{{"replicaCount"}}},
		{"ingress/api", ".spec.ingressClassName", value.Structural, [][]any{{"ingress", "enabled"}}},
		{"deploy/api", ".spec.template.spec.containers[0].ports[0].containerPort", value.Chart, nil},
	}
	for _, tt := range tests {
		t.Run(string(tt.kind), func(t *testing.T) {
			f := field(t, idx, tt.sel, tt.path)

			a, err := value.Explain(t.Context(), idx, c, v, render.Options{}, f)
			if err != nil {
				t.Fatal(err)
			}

			var got [][]any
			for _, l := range a.Leaves {
				got = append(got, l.Path)
			}
			if a.Kind != tt.kind || !reflect.DeepEqual(got, tt.leaves) {
				t.Fatalf("got %s %v, want %s %v", a.Kind, got, tt.kind, tt.leaves)
			}
			if a.Field != f || a.Value != idx.Fields[f] || a.Template != idx.Docs[f.Doc].Source {
				t.Fatalf("answer %+v", a)
			}
		})
	}

	a, _ := value.Explain(t.Context(), idx, c, v, render.Options{}, field(t, idx, "deploy/api", ".spec.replicas"))
	if want := (value.Source{File: prod, Line: 1, Value: float64(3)}); a.Leaves[0].Chain[0] != want {
		t.Fatalf("chain %+v", a.Leaves[0].Chain)
	}
}

// Test hooks with random names (podinfo's tests/*.yaml) must still be found in perturbed renders,
// and in the same order, or every leaf looks like it moves their fields.
func TestExplainRandomNamedHooks(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mini")
	if err := os.CopyFS(dir, os.DirFS(mini)); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"Never", "OnFailure"} {
		tpl := "apiVersion: v1\nkind: Pod\nmetadata:\n  name: t-{{ randAlphaNum 5 | lower }}\n  annotations:\n    helm.sh/hook: test\nspec:\n  restartPolicy: " + p + "\n"
		if err := os.WriteFile(filepath.Join(dir, "templates", "test-"+p+".yaml"), []byte(tpl), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	idx, v, c := trace(t, dir, prod)
	for i, d := range idx.Docs {
		if d.Kind != "Pod" {
			continue
		}

		f := value.Field{Doc: i, Path: ".spec.restartPolicy"}
		a, err := value.Explain(t.Context(), idx, c, v, render.Options{}, f)
		if err != nil {
			t.Fatal(err)
		}
		if a.Kind != value.Chart || len(a.Leaves) != 0 {
			t.Fatalf("%s: got %s with %d leaves, want chart", d.Source, a.Kind, len(a.Leaves))
		}
	}
}

// An empty leaf behind `default` (ingress-nginx's namespaceOverride: "") is the knob that sets the field.
func TestExplainEmptyLeafDefault(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mini")
	if err := os.CopyFS(dir, os.DirFS(mini)); err != nil {
		t.Fatal(err)
	}
	tpl := "apiVersion: v1\nkind: ServiceAccount\nmetadata:\n  name: {{ .Values.saName | default \"api\" }}\n"
	if err := os.WriteFile(filepath.Join(dir, "templates", "sa.yaml"), []byte(tpl), 0o644); err != nil {
		t.Fatal(err)
	}
	files := writeFiles(t, [2]string{"sa.yaml", "saName: \"\"\n"})

	idx, v, c := trace(t, dir, files[0])
	a, err := value.Explain(t.Context(), idx, c, v, render.Options{}, field(t, idx, "serviceaccount/api", ".metadata.name"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != value.Derived || len(a.Leaves) != 1 || a.Leaves[0].ID != lookup(t, v, "saName").ID {
		t.Fatalf("got %s %+v, want derived from saName", a.Kind, a.Leaves)
	}
}

// A leaf that fails the render when perturbed (an enum the chart validates) is left out up front;
// answers must not change.
func TestExplainSkipsBreakers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mini")
	if err := os.CopyFS(dir, os.DirFS(mini)); err != nil {
		t.Fatal(err)
	}
	tpl := "{{- if not (has .Values.mode (list \"a\" \"b\")) }}{{ fail \"bad mode\" }}{{- end }}\n"
	if err := os.WriteFile(filepath.Join(dir, "templates", "mode.yaml"), []byte(tpl), 0o644); err != nil {
		t.Fatal(err)
	}
	files := writeFiles(t, [2]string{"mode.yaml", "mode: a\n"})

	idx, v, c := trace(t, dir, prod, files[0])
	for range 2 { // the second call reuses the breakers found by the first
		a, err := value.Explain(t.Context(), idx, c, v, render.Options{}, field(t, idx, "deploy/api", ".spec.template.spec.containers[0].env[0].value"))
		if err != nil {
			t.Fatal(err)
		}
		if a.Kind != value.Derived || len(a.Leaves) != 1 || a.Leaves[0].ID != lookup(t, v, "replicaCount").ID {
			t.Fatalf("got %s %+v, want derived from replicaCount", a.Kind, a.Leaves)
		}
	}
}

// A leaf whose nudge fails the render but whose zeroing doesn't (redis's global.defaultFips) is
// still found through the zeroed render.
func TestExplainBreakerPerMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "mini")
	if err := os.CopyFS(dir, os.DirFS(mini)); err != nil {
		t.Fatal(err)
	}
	tpl := "{{- if not (has .Values.fips (list \"on\" \"\")) }}{{ fail \"bad fips\" }}{{- end }}\n" +
		"apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: fips\ndata:\n  mode: {{ if .Values.fips }}strict{{ else }}off{{ end }}\n"
	if err := os.WriteFile(filepath.Join(dir, "templates", "fips.yaml"), []byte(tpl), 0o644); err != nil {
		t.Fatal(err)
	}
	files := writeFiles(t, [2]string{"fips.yaml", "fips: \"on\"\n"})

	idx, v, c := trace(t, dir, files[0])
	a, err := value.Explain(t.Context(), idx, c, v, render.Options{}, field(t, idx, "cm/fips", ".data.mode"))
	if err != nil {
		t.Fatal(err)
	}
	if a.Kind != value.Derived || len(a.Leaves) != 1 || a.Leaves[0].ID != lookup(t, v, "fips").ID {
		t.Fatalf("got %s %+v, want derived from fips", a.Kind, a.Leaves)
	}
}

func TestExplainAll(t *testing.T) {
	idx, v, c := trace(t, mini, prod)

	i, _ := value.SelectDoc(idx.Docs, "cm/config", "")
	as, err := value.ExplainAll(t.Context(), idx, c, v, render.Options{}, i)
	if err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, a := range as {
		got = append(got, a.Field.Path+" "+string(a.Kind))
	}
	want := []string{".apiVersion chart", ".data.LOG_LEVEL direct", ".kind chart", ".metadata.name chart"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}
}

func TestWriters(t *testing.T) {
	as := kindAnswers(t)

	for name, write := range map[string]func(io.Writer, []value.Answer) error{
		"answers.txt": value.WriteHuman, "answers.md": value.WriteMarkdown, "answers.json": value.WriteJSON,
		"one.txt": func(w io.Writer, as []value.Answer) error { return value.WriteHuman(w, as[:1]) },
	} {
		var b bytes.Buffer
		if err := write(&b, as); err != nil {
			t.Fatal(err)
		}

		golden := filepath.Join("testdata", "golden", name)
		if *update {
			if err := os.WriteFile(golden, b.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
		}

		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatal(err)
		}
		if b.String() != string(want) {
			t.Errorf("%s:\n%s\nwant:\n%s", name, b.String(), want)
		}
	}
}

func TestJSONSchemaField(t *testing.T) {
	var b bytes.Buffer
	if err := value.WriteJSON(&b, kindAnswers(t)); err != nil {
		t.Fatal(err)
	}

	var out struct {
		Schema  string `json:"schema"`
		Answers []struct {
			Kind  string `json:"kind"`
			Value any    `json:"value"`
			Field struct {
				Path string `json:"path"`
			} `json:"field"`
			Leaves []struct {
				Chain []struct {
					File string `json:"file"`
					Line int    `json:"line"`
				} `json:"chain"`
			} `json:"leaves"`
		} `json:"answers"`
	}
	if err := json.Unmarshal(b.Bytes(), &out); err != nil {
		t.Fatal(err)
	}

	a := out.Answers[0]
	if out.Schema != "kwhy.value/v1" || a.Kind != "direct" || a.Value != float64(3) || a.Field.Path != ".spec.replicas" ||
		a.Leaves[0].Chain[0].File != prod || a.Leaves[0].Chain[0].Line != 1 {
		t.Fatalf("got %+v", out)
	}
}

func loadValues(t *testing.T, files, sets []string) *value.Values {
	t.Helper()

	c, err := render.LoadChart(mini, "", cli.New())
	if err != nil {
		t.Fatal(err)
	}

	v, err := value.LoadValues(c, values.Options{ValueFiles: files, Values: sets})
	if err != nil {
		t.Fatal(err)
	}

	return v
}

func lookup(t *testing.T, v *value.Values, path ...any) value.Leaf {
	t.Helper()

	l, ok := v.Lookup(path)
	if !ok {
		t.Fatalf("no leaf %s", value.PathString(path))
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

func renderMini(t *testing.T) []render.Doc {
	t.Helper()

	c, err := render.LoadChart(mini, "", cli.New())
	if err != nil {
		t.Fatal(err)
	}

	docs, err := c.Render(t.Context(), nil, render.Options{})
	if err != nil {
		t.Fatal(err)
	}

	return docs
}

func selectDoc(t *testing.T, docs []render.Doc, sel string) render.Doc {
	t.Helper()

	i, err := value.SelectDoc(docs, sel, "")
	if err != nil {
		t.Fatal(err)
	}

	return docs[i]
}

func trace(t *testing.T, chart string, files ...string) (*value.Index, *value.Values, *render.Chart) {
	t.Helper()

	c, err := render.LoadChart(chart, "", cli.New())
	if err != nil {
		t.Fatal(err)
	}

	v, err := value.LoadValues(c, values.Options{ValueFiles: files})
	if err != nil {
		t.Fatal(err)
	}

	idx, err := value.Trace(t.Context(), c, v, render.Options{}, value.ChunkSize)
	if err != nil {
		t.Fatal(err)
	}

	return idx, v, c
}

func field(t *testing.T, idx *value.Index, sel, path string) value.Field {
	t.Helper()

	i, err := value.SelectDoc(idx.Docs, sel, "")
	if err != nil {
		t.Fatal(err)
	}

	p, err := value.ResolvePath(idx.Docs[i].Object, path)
	if err != nil {
		t.Fatal(err)
	}

	return value.Field{Doc: i, Path: p}
}

// kindAnswers explains one field of every kind, in the order of TestExplainKinds.
func kindAnswers(t *testing.T) []value.Answer {
	t.Helper()

	idx, v, c := trace(t, mini, prod)
	var as []value.Answer
	for _, fp := range [][2]string{
		{"deploy/api", ".spec.replicas"},
		{"deploy/api", ".spec.template.spec.containers[0].image"},
		{"secret/auth", ".data.user"},
		{"secret/auth", ".data.password"},
		{"deploy/api", `.spec.template.metadata.annotations["checksum/config"]`},
		{"deploy/api", ".spec.template.spec.containers[0].env[0].value"},
		{"ingress/api", ".spec.ingressClassName"},
		{"deploy/api", ".spec.template.spec.containers[0].ports[0].containerPort"},
	} {
		a, err := value.Explain(t.Context(), idx, c, v, render.Options{}, field(t, idx, fp[0], fp[1]))
		if err != nil {
			t.Fatal(err)
		}
		as = append(as, a)
	}
	as[3].Value = "<random>" // differs per render

	return as
}
