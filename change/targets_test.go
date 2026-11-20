package change_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/semirm-dev/kwhy/change"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()

	root := t.TempDir()
	for name, content := range files {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	return root
}

func TestLoadConfig(t *testing.T) {
	root := writeTree(t, map[string]string{".kwhy.yaml": `
targets:
  - {chart: charts/api, values: [prod.yaml], set: [replicaCount=3], release: api, namespace: shop}
  - {manifests: k8s/infra}
`})

	ts, err := change.LoadConfig(filepath.Join(root, ".kwhy.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 2 || ts[0].Chart != "charts/api" || ts[0].Release != "api" || ts[0].Namespace != "shop" ||
		!slices.Equal(ts[0].Values.ValueFiles, []string{"prod.yaml"}) || !slices.Equal(ts[0].Values.Values, []string{"replicaCount=3"}) ||
		ts[1].Manifests != "k8s/infra" || ts[1].Release != "rel" || ts[1].Namespace != "default" {
		t.Errorf("targets = %+v", ts)
	}

	for _, bad := range []string{"targets: [{chart: a, manifests: b}]", "targets: []", "targets: [{chart: a, typo: 1}]"} {
		root := writeTree(t, map[string]string{".kwhy.yaml": bad})
		if _, err := change.LoadConfig(filepath.Join(root, ".kwhy.yaml")); err == nil {
			t.Errorf("%q: want an error", bad)
		}
	}
}

func TestDiscover(t *testing.T) {
	const remote = "git@github.com:acme/platform.git"
	root := writeTree(t, map[string]string{
		"charts/api/Chart.yaml":            "apiVersion: v2\nname: api\nversion: 1.0.0\n",
		"charts/api/values-prod.yaml":      "replicaCount: 3\n",
		"k8s/infra/cm.yaml":                "apiVersion: v1\nkind: ConfigMap\nmetadata: {name: x}\n",
		"overlays/prod/kustomization.yaml": "resources: []\n",
		"apps/argo.yaml": `
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: api}
spec:
  destination: {namespace: shop}
  source:
    repoURL: https://github.com/acme/platform
    path: charts/api
    helm:
      releaseName: api-argo
      valueFiles: [values-prod.yaml]
      parameters: [{name: image.tag, value: "1.2"}, {name: build, value: "007", forceString: true}]
      valuesObject: {debug: true}
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: infra}
spec:
  destination: {namespace: infra}
  source: {repoURL: https://github.com/acme/platform.git, path: k8s/infra}
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: prod}
spec:
  source: {repoURL: https://github.com/acme/platform, path: overlays/prod}
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: redis}
spec:
  source: {repoURL: https://charts.bitnami.com/bitnami, chart: redis, targetRevision: 19.0.0}
---
apiVersion: argoproj.io/v1alpha1
kind: Application
metadata: {name: elsewhere}
spec:
  source: {repoURL: https://github.com/other/repo, path: charts/x}
`,
		"apps/flux.yaml": `
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata: {name: api, namespace: flux-system}
spec:
  targetNamespace: shop
  chart:
    spec:
      chart: ./charts/api
      sourceRef: {kind: GitRepository, name: flux-system}
      valuesFiles: [charts/api/values-prod.yaml]
  values: {replicaCount: 5}
  valuesFrom: [{kind: ConfigMap, name: overrides}]
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata: {name: redis, namespace: flux-system}
spec:
  chart: {spec: {chart: redis, sourceRef: {kind: HelmRepository, name: bitnami}}}
---
apiVersion: source.toolkit.fluxcd.io/v1
kind: GitRepository
metadata: {name: flux-system, namespace: flux-system}
spec: {url: ssh://git@github.com/acme/platform}
`,
	})
	t.Chdir(root)

	ts, warns, err := change.Discover("apps", remote, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	byRelease := map[string]change.Target{}
	for _, tg := range ts {
		byRelease[tg.Release] = tg
	}
	if len(ts) != 3 {
		t.Fatalf("targets = %+v, want argo api, argo infra, flux api", ts)
	}

	argo := byRelease["api-argo"]
	if argo.Chart != "charts/api" || argo.Namespace != "shop" || len(argo.Values.ValueFiles) != 2 ||
		argo.Values.ValueFiles[0] != filepath.Join("charts/api", "values-prod.yaml") ||
		!slices.Equal(argo.Values.Values, []string{"image.tag=1.2"}) || !slices.Equal(argo.Values.StringValues, []string{"build=007"}) {
		t.Errorf("argo api = %+v", argo)
	}
	if inline, _ := os.ReadFile(argo.Values.ValueFiles[1]); !strings.Contains(string(inline), "debug: true") {
		t.Errorf("argo inline values = %q", inline)
	}

	if infra := byRelease["infra"]; infra.Manifests != "k8s/infra" || infra.Namespace != "infra" {
		t.Errorf("argo infra = %+v", infra)
	}

	// Flux's default release name is <targetNamespace>-<name>; valuesFiles are relative to the repo root.
	if flux := byRelease["shop-api"]; flux.Chart != "charts/api" || flux.Namespace != "shop" || len(flux.Values.ValueFiles) != 2 ||
		flux.Values.ValueFiles[0] != "charts/api/values-prod.yaml" {
		t.Errorf("flux api = %+v", flux)
	}

	got := strings.Join(warns, "\n")
	for _, w := range []string{
		"Application prod: overlays/prod is a Kustomize directory",
		"Application redis: chart redis from a Helm repository",
		"Application elsewhere: source https://github.com/other/repo, not this repo",
		"HelmRelease api: valuesFrom",
		"HelmRelease redis: chart from a HelmRepository",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("no warning %q in\n%s", w, got)
		}
	}
}
