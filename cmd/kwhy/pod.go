package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"slices"
	"strings"

	"github.com/spf13/cobra"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/cli/values"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/semirm-dev/kwhy/pod"
	"github.com/semirm-dev/kwhy/render"
	"github.com/semirm-dev/kwhy/value"
)

var podWriters = map[string]func(io.Writer, *corev1.Pod, pod.Result) error{
	"human": pod.WriteHuman, "markdown": pod.WriteMarkdown, "json": pod.WriteJSON,
}

func podCmd() *cobra.Command {
	var (
		namespace, output string
		src               podChart
		logs              int64
		schedConfig       string
	)

	helm := cli.New()

	cmd := &cobra.Command{
		Use:   "pod <name|pod/name|deploy/name|sts/name|ds/name|rs/name|job/name|cj/name>",
		Short: "Why a pod is not running: every scheduler check on every node, or what kubelet and its controller report",
		Example: `  kwhy pod api-7d9f-x2x -n shop
  kwhy pod deploy/api -n shop --context prod -o markdown
  kwhy pod deploy/api -n shop --chart ./charts/api -f values-prod.yaml   # also: which values line set it`,
		Args: usageArgs(1, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if podWriters[output] == nil {
				return usageError{fmt.Errorf("--output must be human, markdown or json, got %q", output)}
			}

			if src.chart != "" && src.kustomize != "" {
				return usageError{errors.New("give --chart or --kustomize, not both")}
			}
			if src.kustomize != "" && src.root == "" {
				src.root = gitRoot()
			}

			if logs < 0 {
				return usageError{fmt.Errorf("--logs must be 0 or more, got %d", logs)}
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()

			klog.SetOutput(io.Discard) // the scheduler framework logs through klog
			klog.LogToStderr(false)
			slog.SetDefault(slog.New(slog.DiscardHandler)) // and Helm, with --chart, through slog

			c, err := kubeClient(helm)
			if err != nil {
				return fmt.Errorf("kubeconfig: %w", err)
			}

			if namespace == "" {
				namespace = helm.Namespace()
			}

			ctrl, err := pod.Controller(ctx, c, namespace, args[0])
			if err != nil {
				return err
			}

			p, err := pod.Target(ctx, c, namespace, args[0])
			var healthy pod.Healthy
			if err != nil && !errors.As(err, &healthy) {
				return err
			}

			// All pods ready, or none yet: the controller's problems are the answer when it has any.
			if err != nil || pod.IsTemplate(p) && len(ctrl) > 0 {
				if len(ctrl) == 0 {
					return pod.WriteHealthy(cmd.OutOrStdout(), output, namespace, args[0], healthy)
				}
				w := &corev1.Pod{Name: args[0], Namespace: namespace}
				return podWriters[output](cmd.OutOrStdout(), w, pod.Result{Problems: ctrl})
			}

			if p.Spec.NodeName != "" {
				ps, err := pod.Diagnose(ctx, c, p)
				if err != nil {
					return err
				}

				if logs > 0 {
					pod.AddLogs(ctx, c, p, ps, logs)
				}

				r := pod.Result{Problems: append(ps, ctrl...)}
				if src.kustomize != "" && len(ps) > 0 {
					if r.Sources, err = podKustomizeSources(ctx, c, p, r, src.root, src.kustomize, src.noCache); err != nil {
						return err
					}
				}
				if src.chart != "" && len(ps) > 0 {
					v, err := c.Discovery().ServerVersion()
					if err != nil {
						return fmt.Errorf("server version: %w", err)
					}
					if r.Sources, err = podSources(ctx, c, p, r, src, v.GitVersion, helm); err != nil {
						return err
					}
				}

				return podWriters[output](cmd.OutOrStdout(), p, r)
			}

			cl, err := pod.Snapshot(ctx, c)
			if err != nil {
				return err
			}

			if schedConfig != "" {
				if err := cl.UseSchedulerConfig(schedConfig, p.Spec.SchedulerName); err != nil {
					return err
				}
			}

			for _, w := range pod.Warnings(cl, p) {
				fmt.Fprintln(cmd.ErrOrStderr(), "kwhy: warning:", w)
			}

			r, err := pod.Evaluate(ctx, cl, p)
			if err != nil {
				return err
			}
			if !pod.IsTemplate(p) {
				if r.Problems, err = pod.Autoscaler(ctx, c, p); err != nil {
					return err
				}
			}
			r.Problems = append(r.Problems, ctrl...)

			if src.chart != "" {
				if r.Sources, err = podSources(ctx, c, p, r, src, cl.Version, helm); err != nil {
					return err
				}
			}
			if src.kustomize != "" {
				if r.Sources, err = podKustomizeSources(ctx, c, p, r, src.root, src.kustomize, src.noCache); err != nil {
					return err
				}
			}

			return podWriters[output](cmd.OutOrStdout(), p, r)
		},
	}

	f := cmd.Flags()
	f.StringVarP(&namespace, "namespace", "n", "", "namespace (default: the kube context's)")
	f.StringVarP(&output, "output", "o", "human", "output format: human, markdown or json")
	f.StringVar(&helm.KubeContext, "context", "", "kube context to read from (read-only)")
	f.StringVar(&schedConfig, "scheduler-config", "", "the cluster's KubeSchedulerConfiguration file: run the pod's profile instead of the default")
	f.Int64Var(&logs, "logs", 0, "last N log lines of a crashed or OOM-killed container's last run (logs may contain secrets)")
	f.StringVar(&src.chart, "chart", "", "chart that deploys the pod: also show which values line sets each field a failing check reads")
	f.StringVar(&src.version, "version", "", "chart version, for repo and OCI charts")
	valuesFlags(f, &src.values, ", as deployed")
	f.BoolVar(&src.noCache, "no-cache", false, "with --chart or --kustomize: trace again instead of reusing the last trace")
	f.StringVar(&src.kustomize, "kustomize", "", "Kustomize overlay that deploys the pod: also show which entry and line set each field a failing check reads")
	f.StringVar(&src.root, "root", "", "with --kustomize: directory holding the whole overlay stack (default: the git repo root)")
	f.StringVar(&src.release, "release", "", "Helm release name (default: the pod's app.kubernetes.io/instance label)")

	return cmd
}

// podChart is how the pod's chart was deployed.
type podChart struct {
	noCache                 bool
	chart, version, release string
	values                  values.Options
	kustomize, root         string // or a Kustomize overlay, and the directory holding its stack
}

// podSources finds, with kwhy value, the values lines that set the fields p's failing checks read. It renders the
// chart, finds the workload that owns p in the render, and explains only the fields those checks read.
func podSources(ctx context.Context, c kubernetes.Interface, p *corev1.Pod, r pod.Result, src podChart, kubeVersion string,
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

	o := render.Options{Release: release, Namespace: p.Namespace, KubeVersion: kubeVersion}
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

	cache := value.OpenCache(ch, v, o, value.ChunkSize, traceCache(src.noCache))
	idx, err := cache.Trace(ctx, ch, v, o, value.ChunkSize)
	if err != nil {
		return nil, err
	}

	var out []pod.Source
	for _, w := range wants {
		a, err := cache.Explain(ctx, idx, ch, v, o, value.Field{Doc: doc, Path: w.path})
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

	o, err := value.LoadOverlay(root, dir, traceCache(noCache))
	if err != nil {
		return nil, err
	}
	d, err := o.Select(kind + "/" + name)
	if err != nil {
		return nil, fmt.Errorf("--kustomize: %w", err)
	}

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

// wanted is a field of the rendered workload a failing check or problem reads.
type wanted struct {
	path string // in the rendered document, list items by index
	src  pod.Source
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
