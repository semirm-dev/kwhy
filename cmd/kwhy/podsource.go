package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/cli/values"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/semirm-dev/kwhy/pod"
	"github.com/semirm-dev/kwhy/render"
	"github.com/semirm-dev/kwhy/value"
)

// Where pod and value meet: which values or overlay lines set the fields a pod's failing checks read.

// podChart is how the pod's chart was deployed.
type podChart struct {
	noCache                 bool
	chart, version, release string
	values                  values.Options
	kustomize, root         string // or a Kustomize overlay, and the directory holding its stack
}

// sources is, with --chart or --kustomize, the lines that set the fields r's failing checks and problems read.
func (src podChart) sources(ctx context.Context, c kubernetes.Interface, p *corev1.Pod, r pod.Result,
	helm *cli.EnvSettings) ([]pod.Source, error) {
	switch {
	case src.chart != "":
		return podSources(ctx, c, p, r, src, helm)
	case src.kustomize != "":
		return podKustomizeSources(ctx, c, p, r, src.root, src.kustomize, src.noCache)
	}

	return nil, nil
}

// wanted is a field of the rendered workload a failing check or problem reads.
type wanted struct {
	path string // in the rendered document, list items by index
	src  pod.Source
}

// podSources finds, with kwhy value, the values lines that set the fields p's failing checks read. It renders the
// chart, finds the workload that owns p in the render, and explains only the fields those checks read.
func podSources(ctx context.Context, c kubernetes.Interface, p *corev1.Pod, r pod.Result, src podChart,
	helm *cli.EnvSettings) ([]pod.Source, error) {
	kind, name, err := pod.Owner(ctx, c, p)
	if err != nil {
		return nil, err
	}

	release := src.release
	if release == "" {
		release = p.Labels["app.kubernetes.io/instance"]
	}
	if release == "" {
		return nil, usageError{errors.New("--chart: give --release, the pod has no app.kubernetes.io/instance label")}
	}

	ch, err := render.LoadChart(src.chart, src.version, helm)
	if err != nil {
		return nil, err
	}

	v, err := value.LoadValues(ch, src.values)
	if err != nil {
		return nil, err
	}

	// Render for the cluster's version, as the chart was installed.
	sv, err := c.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("server version: %w", err)
	}

	o := render.Options{Release: release, Namespace: p.Namespace, KubeVersion: sv.GitVersion}
	docs, err := ch.Render(ctx, v.User, o)
	if err != nil {
		return nil, err
	}

	doc, err := value.SelectDoc(docs, kind+"/"+name, p.Namespace)
	if err != nil {
		return nil, fmt.Errorf("--chart: %w", err)
	}

	wants := podWanted(r, kind, docs[doc].Object)
	if len(wants) == 0 {
		return nil, nil
	}

	idx, err := value.Trace(ctx, ch, v, o, value.ChunkSize, traceCache(src.noCache))
	if err != nil {
		return nil, err
	}

	var out []pod.Source
	for _, w := range wants {
		a, err := idx.Explain(ctx, value.Field{Doc: doc, Path: w.path})
		if err != nil {
			return nil, err
		}
		if len(a.Leaves) == 0 {
			continue // set in the chart's templates, not by a value
		}
		out = append(out, sourceOf(w, a, kind))
	}

	return out, nil
}

// podKustomizeSources is podSources for a pod deployed from a Kustomize overlay.
func podKustomizeSources(ctx context.Context, c kubernetes.Interface, p *corev1.Pod, r pod.Result, root, dir string, noCache bool) ([]pod.Source, error) {
	kind, name, err := pod.Owner(ctx, c, p)
	if err != nil {
		return nil, err
	}

	o, err := value.LoadOverlay(ctx, root, dir, traceCache(noCache))
	if err != nil {
		return nil, err
	}
	d, err := o.Select(kind + "/" + name)
	if err != nil {
		return nil, fmt.Errorf("--kustomize: %w", err)
	}

	o.Confirm = true // a few fields a failing check reads: worth the rebuild that confirms each line
	var out []pod.Source
	for _, w := range podWanted(r, kind, d.Object) {
		kp, err := o.FieldPath(d, w.path)
		if err != nil {
			continue
		}
		a, err := o.Explain(value.KField{Doc: d.ID, Path: kp})
		if err != nil {
			return nil, err
		}
		if a.Kind == value.Chart {
			continue // no entry or confirmed line to point at
		}
		out = append(out, sourceOf(w, a, kind))
	}

	return out, nil
}

// podWanted lists the fields of a rendered workload (or bare Pod) obj that r's failing checks and problems read.
func podWanted(r pod.Result, kind string, obj map[string]any) []wanted {
	prefix := templatePrefix(kind)
	spec := obj
	for k := range strings.SplitSeq(strings.TrimPrefix(prefix+".spec", "."), ".") {
		spec, _ = spec[k].(map[string]any)
	}

	type matcher struct {
		src   pod.Source
		reads func(path string) bool
		check bool
	}
	var matchers []matcher
	for _, n := range r.Nodes {
		for _, chk := range n.Failed {
			matchers = append(matchers, matcher{pod.Source{Plugin: chk.Plugin}, func(p string) bool { return pod.Reads(chk, p) }, true})
		}
	}
	for _, pr := range r.Problems {
		i := containerIndex(spec, pr.Container, pr.Init)
		label := pr.Kind
		if pr.Container != "" {
			label += " (" + pr.Container + ")"
		}
		matchers = append(matchers, matcher{pod.Source{Problem: label}, func(p string) bool { return pod.ReadsProblem(pr, p, i) }, false})
	}

	fields := value.Flatten([]render.Doc{{Object: obj}})
	var out []wanted
	for f := range fields {
		path, ok := strings.CutPrefix(f.Path, prefix)
		if !ok || !strings.HasPrefix(path, ".spec") {
			continue
		}
		// For scheduling, a limit only matters when the container sets no request for it: Kubernetes copies the
		// limit. An OOM kill is about the limit itself.
		req := strings.Replace(f.Path, ".resources.limits.", ".resources.requests.", 1)
		_, hasReq := fields[value.Field{Path: req}]
		shadowed := hasReq && req != f.Path

		for _, m := range matchers {
			if (!m.check || !shadowed) && m.reads(path) {
				out = append(out, wanted{f.Path, m.src})
				break
			}
		}
	}
	slices.SortFunc(out, func(a, b wanted) int { return strings.Compare(a.path, b.path) })

	return out
}

// sourceOf turns an answer into the pod's Source line: the field in pod-spec terms, its value and where it's set.
func sourceOf(w wanted, a value.Answer, kind string) pod.Source {
	prefix := templatePrefix(kind)

	var from []string
	for _, l := range a.Leaves {
		name := l.Entry
		if name == "" {
			name = value.PathString(l.Path)
		}
		from = append(from, fmt.Sprintf("%s (%s)", name, l.Chain[0]))
	}

	s := w.src
	s.Field, s.Value, s.From = strings.TrimPrefix(w.path, prefix), a.Value, strings.Join(from, ", ")

	return s
}

// templatePrefix is where a workload's pod template sits; a bare Pod is the document itself.
func templatePrefix(kind string) string {
	switch kind {
	case "Pod":
		return ""
	case "CronJob":
		return ".spec.jobTemplate.spec.template"
	}

	return ".spec.template"
}

// containerIndex is the position of the container named name in a rendered pod spec, -1 if absent.
func containerIndex(spec map[string]any, name string, init bool) int {
	key := "containers"
	if init {
		key = "initContainers"
	}

	list, _ := spec[key].([]any)
	for i, c := range list {
		if m, _ := c.(map[string]any); m["name"] == name {
			return i
		}
	}

	return -1
}
