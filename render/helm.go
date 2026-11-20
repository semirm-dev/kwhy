// Package render renders Helm charts in-process into documents that remember their template.
package render

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
	"helm.sh/helm/v4/pkg/action"
	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/chart/common/util"
	chart "helm.sh/helm/v4/pkg/chart/v2"
	"helm.sh/helm/v4/pkg/chart/v2/loader"
	chartutil "helm.sh/helm/v4/pkg/chart/v2/util"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/registry"
	rspb "helm.sh/helm/v4/pkg/release/v1"
	releaseutil "helm.sh/helm/v4/pkg/release/v1/util"
)

// KubeVersion is the Kubernetes version charts see when Options.KubeVersion is empty. Helm
// assumes v1.20 under go test and the client-go version otherwise; pinning it makes both agree.
// Bump it with client-go.
const KubeVersion = "v1.37.0"

// Chart is a located Helm chart. Every Render and Coalesce loads a fresh copy, because Helm's
// dependency processing removes disabled subcharts and renames aliased ones in place.
type Chart struct {
	path string
}

// LoadChart loads a chart from a directory, .tgz, oci:// reference or repo/chart name.
// settings carries Helm's repository and registry paths.
func LoadChart(ref, version string, settings *cli.EnvSettings) (*Chart, error) {
	rc, err := registry.NewClient(
		registry.ClientOptEnableCache(true),
		registry.ClientOptCredentialsFile(settings.RegistryConfig),
	)
	if err != nil {
		return nil, fmt.Errorf("registry client: %w", err)
	}

	// ChartPathOptions only takes a registry client through Install.
	install := action.NewInstall(action.NewConfiguration())
	install.Version = version
	install.SetRegistryClient(rc)

	path, err := install.LocateChart(ref, settings)
	if err != nil {
		return nil, fmt.Errorf("locate chart: %w", err)
	}

	c := &Chart{path: path}
	if _, err := c.load(); err != nil {
		return nil, err
	}

	return c, nil
}

// Coalesce merges chart and subchart defaults with user values, as Helm does before rendering:
// disabled subcharts are dropped and aliased ones are mounted under their alias.
func (c *Chart) Coalesce(user map[string]any) (map[string]any, error) {
	ch, err := c.load()
	if err != nil {
		return nil, err
	}

	if err := chartutil.ProcessDependencies(ch, user); err != nil {
		return nil, fmt.Errorf("process dependencies: %w", err)
	}

	v, err := util.CoalesceValues(ch, user)
	if err != nil {
		return nil, fmt.Errorf("coalesce values: %w", err)
	}

	return v, nil
}

// ValuesFiles returns the default values files that apply with user values, lowest precedence first
// (subcharts before their parent).
func (c *Chart) ValuesFiles(user map[string]any) ([]ValuesFile, error) {
	ch, err := c.load()
	if err != nil {
		return nil, err
	}

	if err := chartutil.ProcessDependencies(ch, user); err != nil {
		return nil, fmt.Errorf("process dependencies: %w", err)
	}

	return valuesFiles(ch, nil), nil
}

// Render renders the chart with user values, hooks and CRDs included, like `helm template`.
func (c *Chart) Render(ctx context.Context, user map[string]any, o Options) ([]Doc, error) {
	if o.Release == "" {
		o.Release = "rel"
	}
	if o.Namespace == "" {
		o.Namespace = "default"
	}
	if o.KubeVersion == "" {
		o.KubeVersion = KubeVersion
	}

	kv, err := common.ParseKubeVersion(o.KubeVersion)
	if err != nil {
		return nil, fmt.Errorf("kube version: %w", err)
	}

	ch, err := c.load()
	if err != nil {
		return nil, err
	}

	install := action.NewInstall(action.NewConfiguration())
	install.DryRunStrategy = action.DryRunClient
	install.ReleaseName = o.Release
	install.Namespace = o.Namespace
	install.IncludeCRDs = true
	install.KubeVersion = kv

	r, err := install.RunWithContext(ctx, ch, user)
	if err != nil {
		return nil, fmt.Errorf("render chart: %w", err)
	}

	rel, ok := r.(*rspb.Release)
	if !ok {
		return nil, fmt.Errorf("render chart: unsupported release type %T", r)
	}

	split := releaseutil.SplitManifests(rel.Manifest)
	keys := make([]string, 0, len(split))
	for k := range split {
		keys = append(keys, k)
	}

	var docs []Doc
	sort.Sort(releaseutil.BySplitManifestsOrder(keys))
	for _, k := range keys {
		m := split[k]
		if err := appendDoc(&docs, source(m), m, o.Namespace); err != nil {
			return nil, err
		}
	}

	// Helm orders hooks by weight then name; random names (test pods) would reorder docs per render.
	slices.SortStableFunc(rel.Hooks, func(a, b *rspb.Hook) int { return strings.Compare(a.Path, b.Path) })
	for _, h := range rel.Hooks {
		if err := appendDoc(&docs, h.Path, h.Manifest, o.Namespace); err != nil {
			return nil, err
		}
	}

	return docs, nil
}

// Digest is a hash of every file of the chart (directory or archive), to key caches on.
func (c *Chart) Digest() (string, error) {
	h := sha256.New()
	err := filepath.WalkDir(c.path, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fmt.Fprintf(h, "%s\x00%d\x00", filepath.ToSlash(strings.TrimPrefix(path, c.path)), len(data))
		h.Write(data)
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("digest chart: %w", err)
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// shortcut: reloads from disk on every call; cache the archive bytes if Task 7 shows it is slow.
func (c *Chart) load() (*chart.Chart, error) {
	ch, err := loader.Load(c.path)
	if err != nil {
		return nil, fmt.Errorf("load chart: %w", err)
	}

	return ch, nil
}

// Options are the release settings a render runs with. Empty fields default to "rel", "default"
// and KubeVersion.
type Options struct {
	Release, Namespace string
	KubeVersion        string // as for helm --kube-version, e.g. "1.31" or "v1.31.2"
}

// Doc is one rendered Kubernetes document.
type Doc struct {
	Source                string // template path, e.g. "mini/templates/deployment.yaml"
	Kind, Name, Namespace string
	Object                map[string]any
}

// ValuesFile is the default values.yaml of the chart or one of its enabled subcharts.
type ValuesFile struct {
	Mount []string // where its values sit in the coalesced values: nil for the root chart, {"sub"} for a subchart
	Name  string   // e.g. "mini/charts/sub/values.yaml"; an aliased subchart shows its alias
	Data  []byte
}

func valuesFiles(ch *chart.Chart, mount []string) []ValuesFile {
	var files []ValuesFile
	for _, dep := range ch.Dependencies() {
		files = append(files, valuesFiles(dep, append(slices.Clip(mount), dep.Name()))...)
	}

	for _, f := range ch.Raw {
		if f.Name == "values.yaml" {
			files = append(files, ValuesFile{Mount: mount, Name: ch.ChartFullPath() + "/values.yaml", Data: f.Data})
		}
	}

	return files
}

func appendDoc(docs *[]Doc, src, manifest, namespace string) error {
	var obj map[string]any
	if err := yaml.Unmarshal([]byte(manifest), &obj); err != nil {
		return fmt.Errorf("decode %s: %w", src, err)
	}

	if obj == nil {
		return nil
	}

	*docs = append(*docs, newDoc(src, obj, namespace))

	return nil
}

// newDoc wraps a decoded object; namespace is used when the object has none.
func newDoc(src string, obj map[string]any, namespace string) Doc {
	d := Doc{Source: src, Object: obj, Namespace: namespace}
	d.Kind, _ = obj["kind"].(string)

	if md, ok := obj["metadata"].(map[string]any); ok {
		d.Name, _ = md["name"].(string)
		if ns, ok := md["namespace"].(string); ok && ns != "" {
			d.Namespace = ns
		}
	}

	return d
}

func source(manifest string) string {
	for line := range strings.Lines(manifest) {
		if s, ok := strings.CutPrefix(line, "# Source: "); ok {
			return strings.TrimSpace(s)
		}
	}

	return ""
}
