// Command kwhy answers why questions about Kubernetes config. Wiring only: flags in, features out.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/cli/values"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"

	"github.com/semirm-dev/kwhy/change"
	"github.com/semirm-dev/kwhy/pod"
	"github.com/semirm-dev/kwhy/render"
	"github.com/semirm-dev/kwhy/value"
)

var (
	// version is set at release time with -ldflags "-X main.version=...".
	version = "dev"

	writers = map[string]func(io.Writer, []value.Answer) error{
		"human": value.WriteHuman, "markdown": value.WriteMarkdown, "json": value.WriteJSON,
	}

	// failVerdicts maps --fail-on names to verdicts: each verdict with - for spaces, plus fail and data-loss.
	failVerdicts = map[string]change.Verdict{
		"fail": change.WillFail, "data-loss": change.Data, "restart": change.Restart, "ordered": change.Ordered,
		"scale": change.Scale, "resize": change.Resize, "recreate": change.Recreate, "runs-as-hook": change.Hook, "no-effect": change.NoEffect,
		"delayed": change.Delayed, "picked-up": change.PickedUp, "no-endpoints": change.Orphaned, "create": change.Create, "delete": change.Delete,
	}

	changeWriters = map[string]func(io.Writer, []change.Impact) error{
		"human": change.WriteHuman, "markdown": change.WriteMarkdown, "json": change.WriteJSON,
	}

	podWriters = map[string]func(io.Writer, *corev1.Pod, pod.Result) error{
		"human": pod.WriteHuman, "markdown": pod.WriteMarkdown, "json": pod.WriteJSON,
	}
)

// usageError marks errors that exit with code 2.
type usageError struct{ error }

// failOnError marks a change that hit --fail-on: exit code 3.
type failOnError struct{ error }

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run returns the exit code: 0 ok, 1 runtime error, 2 usage error, 3 --fail-on hit.
func run(args []string, stdout, stderr io.Writer) int {
	root := &cobra.Command{
		Use:           "kwhy",
		Short:         "Answers why questions about Kubernetes config",
		Version:       version,
		Args:          cobra.ArbitraryArgs, // so unknown commands reach RunE and exit 2
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return usageError{fmt.Errorf("unknown command %q", args[0])}
			}

			cmd.SetOut(stderr)
			_ = cmd.Help()

			return usageError{errors.New("missing command")}
		},
	}

	root.SetVersionTemplate("kwhy {{.Version}}\n")
	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	root.AddCommand(valueCmd(), changeCmd(), podCmd(), mcpCmd())

	err := root.Execute()
	if err == nil {
		return 0
	}

	fmt.Fprintln(stderr, "kwhy:", err)
	if errors.As(err, &usageError{}) {
		return 2
	}
	if errors.As(err, &failOnError{}) {
		return 3
	}

	return 1
}

func valueCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "value",
		Short: "Where a rendered value comes from",
		Args:  cobra.ArbitraryArgs, // so unknown subcommands reach RunE and exit 2
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 {
				return usageError{fmt.Errorf("unknown value command %q", args[0])}
			}

			cmd.SetOut(cmd.ErrOrStderr())
			_ = cmd.Help()

			return usageError{errors.New("missing value command: chart or kustomize")}
		},
	}

	cmd.AddCommand(valueChartCmd(), valueKustomizeCmd())

	return cmd
}

func valueChartCmd() *cobra.Command {
	var (
		chartVersion, release, namespace string
		output, kubeVersion              string
		vals                             values.Options
		all, noCache                     bool
		chunk                            int
	)

	helm := cli.New()

	cmd := &cobra.Command{
		Use:   "chart <chart> <kind>/<name> [<path>]",
		Short: "Where a rendered value comes from, for a Helm chart",
		Example: `  kwhy value chart ./charts/api deploy/api '.spec.template.spec.containers[name=api].resources.requests.memory' \
    -f values-prod.yaml`,
		Args: usageArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			chartRef, args := args[0], args[1:]

			if err := onePathOrAll(len(args) == 2, all); err != nil {
				return err
			}

			if chunk < 1 {
				return usageError{fmt.Errorf("--chunk-size must be at least 1, got %d", chunk)}
			}

			if _, err := common.ParseKubeVersion(kubeVersion); err != nil {
				return usageError{fmt.Errorf("--kube-version: %w", err)}
			}

			if writers[output] == nil {
				return usageError{fmt.Errorf("--output must be human, markdown or json, got %q", output)}
			}

			selectNS := ""
			if cmd.Flags().Changed("namespace") {
				selectNS = namespace
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()

			slog.SetDefault(slog.New(slog.DiscardHandler))

			c, err := render.LoadChart(chartRef, chartVersion, helm)
			if err != nil {
				return err
			}

			v, err := value.LoadValues(c, vals)
			if err != nil {
				return err
			}

			// Check the resource and path on one render before tracing, so a typo fails fast.
			o := render.Options{Release: release, Namespace: namespace, KubeVersion: kubeVersion}
			docs, err := c.Render(ctx, v.User, o)
			if err != nil {
				return err
			}

			doc, err := value.SelectDoc(docs, args[0], selectNS)
			if err != nil {
				return err
			}

			var fields []value.Field
			if !all {
				p, err := value.ResolvePath(docs[doc].Object, args[1])
				if err != nil {
					return pathError(err, docs, doc, args[1])
				}

				fields = append(fields, value.Field{Doc: doc, Path: p})
			}

			cache := value.OpenCache(c, v, o, chunk, traceCache(noCache))
			idx, err := cache.Trace(ctx, c, v, o, chunk)
			if err != nil {
				return err
			}

			if all {
				as, err := value.ExplainAll(ctx, idx, c, v, o, doc)
				if err != nil {
					return err
				}

				return writers[output](cmd.OutOrStdout(), as)
			}

			var as []value.Answer
			for _, f := range fields {
				a, err := cache.Explain(ctx, idx, c, v, o, f)
				if err != nil {
					return err
				}
				as = append(as, a)
			}

			return writers[output](cmd.OutOrStdout(), as)
		},
	}

	f := cmd.Flags()
	f.StringVar(&chartVersion, "version", "", "chart version (for repo and oci charts)")
	valuesFlags(f, &vals, "")
	f.StringVar(&release, "release", "rel", "release name")
	f.StringVarP(&namespace, "namespace", "n", "default", "release namespace")
	f.StringVar(&kubeVersion, "kube-version", render.KubeVersion, "Kubernetes version for .Capabilities.KubeVersion")
	f.StringVarP(&output, "output", "o", "human", "output format: human, markdown or json")
	f.BoolVar(&all, "all", false, "explain every field of the resource")
	f.IntVar(&chunk, "chunk-size", value.ChunkSize, "values leaves marked per tracing render (tuning)")
	f.BoolVar(&noCache, "no-cache", false, "trace the chart again instead of reusing the last trace of the same chart and values")

	// Only Helm's chart lookup flags: value is offline, so the cluster flags don't apply.
	f.StringVar(&helm.RepositoryConfig, "repository-config", helm.RepositoryConfig, "path to Helm's repositories file")
	f.StringVar(&helm.RepositoryCache, "repository-cache", helm.RepositoryCache, "path to Helm's repository index cache")
	f.StringVar(&helm.RegistryConfig, "registry-config", helm.RegistryConfig, "path to Helm's registry config")
	f.StringVar(&helm.ContentCache, "content-cache", helm.ContentCache, "path to Helm's chart content cache")

	return cmd
}

func valueKustomizeCmd() *cobra.Command {
	var (
		output, root string
		all, noCache bool
	)

	cmd := &cobra.Command{
		Use:     "kustomize <overlay dir> <kind>/<name> [<path>]",
		Short:   "Which kustomization entry and line set a built value (experimental)",
		Example: `  kwhy value kustomize overlays/prod deploy/api '.spec.template.spec.containers[name=api].resources.requests.memory'`,
		Args:    usageArgs(2, 3),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir, args := args[0], args[1:]
			if err := onePathOrAll(len(args) == 2, all); err != nil {
				return err
			}
			if writers[output] == nil {
				return usageError{fmt.Errorf("--output must be human, markdown or json, got %q", output)}
			}

			// The whole stack (bases, components) must be in the copy: default to the repo root.
			if root == "" {
				root = "."
				if top, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err == nil {
					root = strings.TrimSpace(string(top))
				}
			}

			o, err := value.LoadOverlay(root, dir, traceCache(noCache))
			if err != nil {
				return err
			}
			d, err := o.Select(args[0])
			if err != nil {
				return err
			}

			if all {
				as, err := o.ExplainAllOverlay(d.ID)
				if err != nil {
					return err
				}
				return writers[output](cmd.OutOrStdout(), as)
			}

			p, err := o.FieldPath(d, args[1])
			if err != nil {
				return err
			}
			a, err := o.ExplainOverlay(value.KField{Doc: d.ID, Path: p})
			if err != nil {
				return err
			}

			return writers[output](cmd.OutOrStdout(), []value.Answer{a})
		},
	}

	f := cmd.Flags()
	f.StringVarP(&output, "output", "o", "human", "output format: human, markdown or json")
	f.BoolVar(&all, "all", false, "explain every field of the resource")
	f.StringVar(&root, "root", "", "directory holding the whole overlay stack (default: the git repo root)")
	f.BoolVar(&noCache, "no-cache", false, "trace the overlay again instead of reusing the last trace of the same files")

	return cmd
}

func changeCmd() *cobra.Command {
	var (
		src                                     change.Source
		release, namespace, output, kubeVersion string
		config, apps                            string
		failOn                                  []string
	)

	helm := cli.New()

	cmd := &cobra.Command{
		Use:     "change <base>..<head>",
		Short:   "What merging a change does to running workloads",
		Example: `  kwhy change main..feature --chart ./charts/api -f values-prod.yaml -o markdown`,
		Args:    usageArgs(1, 1),
		RunE: func(cmd *cobra.Command, args []string) error {
			base, head, ok := strings.Cut(args[0], "..")
			if !ok || base == "" || head == "" {
				return usageError{fmt.Errorf("want <base>..<head>, got %q", args[0])}
			}

			given := 0
			for _, v := range []string{src.Chart, src.Manifests, config, apps} {
				if v != "" {
					given++
				}
			}
			if given == 0 {
				if _, err := os.Stat(".kwhy.yaml"); err != nil {
					return usageError{errors.New("give one of --chart, --manifests, --config or --apps (or add a .kwhy.yaml)")}
				}
				config = ".kwhy.yaml"
			}
			if given > 1 {
				return usageError{errors.New("give only one of --chart, --manifests, --config or --apps")}
			}
			if src.Chart == "" && !reflect.ValueOf(src.Values).IsZero() {
				return usageError{errors.New("values flags apply to --chart only")}
			}
			if (config != "" || apps != "") && (cmd.Flags().Changed("release") || cmd.Flags().Changed("namespace")) {
				return usageError{errors.New("--release and --namespace come from the config or the apps, not flags")}
			}
			if p := src.Chart + src.Manifests; p != "" {
				if _, err := os.Stat(p); err != nil {
					return usageError{fmt.Errorf("%s: %w", p, err)}
				}
			}

			if _, err := common.ParseKubeVersion(kubeVersion); err != nil {
				return usageError{fmt.Errorf("--kube-version: %w", err)}
			}

			if changeWriters[output] == nil {
				return usageError{fmt.Errorf("--output must be human, markdown or json, got %q", output)}
			}

			fail := map[change.Verdict]bool{}
			for _, n := range failOn {
				v, ok := failVerdicts[n]
				if !ok {
					return usageError{fmt.Errorf("--fail-on: unknown verdict %q", n)}
				}
				fail[v] = true
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()

			slog.SetDefault(slog.New(slog.DiscardHandler))

			targets := []change.Target{{Source: src, Release: release, Namespace: namespace}}
			var err error
			if config != "" {
				if targets, err = change.LoadConfig(config); err != nil {
					return err
				}
			}
			// What to render at each ref; with --apps, each ref's own app definitions.
			at := map[string][]change.Target{base: targets, head: targets}

			if apps != "" {
				tmp, err := os.MkdirTemp("", "kwhy-apps-")
				if err != nil {
					return err
				}
				defer os.RemoveAll(tmp)

				said := map[string]bool{}
				for i, ref := range []string{base, head} {
					// Each ref's inline values in their own directory: the file names repeat.
					dir := filepath.Join(tmp, strconv.Itoa(i))
					if err := os.Mkdir(dir, 0o700); err != nil {
						return err
					}
					ts, warnings, err := change.DiscoverAt(ctx, ref, apps, change.Remote(ctx), dir)
					if err != nil {
						return err
					}
					for _, w := range warnings {
						if !said[w] {
							said[w] = true
							fmt.Fprintln(cmd.ErrOrStderr(), "kwhy: warning:", w)
						}
					}
					at[ref] = ts
				}
				if len(at[base])+len(at[head]) == 0 {
					return fmt.Errorf("--apps %s: no Argo CD Application or Flux HelmRelease this repo can render", apps)
				}
			}

			// One diff over every target's documents, so an object one app changes and another consumes is seen.
			var b, h []render.Doc
			for _, side := range []struct {
				ref  string
				docs *[]render.Doc
			}{{base, &b}, {head, &h}} {
				for _, t := range at[side.ref] {
					o := render.Options{Release: t.Release, Namespace: t.Namespace, KubeVersion: kubeVersion}
					ds, err := change.Render(ctx, side.ref, t.Source, o, helm)
					if err != nil {
						return err
					}
					*side.docs = append(*side.docs, ds...)
				}
			}

			is := change.Diff(b, h)
			if cmd.Flags().Changed("context") {
				cfg, err := helm.RESTClientGetter().ToRESTConfig()
				if err != nil {
					return fmt.Errorf("--context: %w", err)
				}
				c, err := kubernetes.NewForConfig(cfg)
				if err != nil {
					return fmt.Errorf("--context: %w", err)
				}
				drift, err := change.Drift(ctx, c, h, is)
				if err != nil {
					return err
				}
				is = append(is, drift...)
				if err := change.Live(ctx, c, is); err != nil {
					return err
				}
			}

			if err := changeWriters[output](cmd.OutOrStdout(), is); err != nil {
				return err
			}

			for _, i := range is {
				if fail[i.Verdict] {
					return failOnError{fmt.Errorf("--fail-on: %s %s", i.Verdict, i.Resource)}
				}
			}

			return nil
		},
	}

	f := cmd.Flags()
	f.StringVar(&src.Chart, "chart", "", "chart directory in the repo")
	f.StringVar(&src.Manifests, "manifests", "", "directory of plain YAML or a Kustomize overlay (experimental) in the repo, instead of --chart")
	f.StringVar(&config, "config", "", "file listing several charts or manifests directories (default .kwhy.yaml when present)")
	f.StringVar(&apps, "apps", "", "directory with Argo CD Applications or Flux HelmReleases: render every app they point at in this repo")
	valuesFlags(f, &src.Values, "; files in the repo are read at each ref")
	f.StringVar(&release, "release", "rel", "release name")
	f.StringVarP(&namespace, "namespace", "n", "default", "release namespace")
	f.StringVar(&kubeVersion, "kube-version", render.KubeVersion, "Kubernetes version for .Capabilities.KubeVersion")
	f.StringVarP(&output, "output", "o", "human", "output format: human, markdown or json")
	f.StringVar(&helm.KubeContext, "context", "", "kube context to read live replicas, HPAs, node counts and StorageClasses from (read-only)")
	f.StringSliceVar(&failOn, "fail-on", nil, "exit 3 if any of these verdicts occur: fail, data-loss, restart, ordered, recreate, no-effect, ...")

	return cmd
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
				src.root = "."
				if top, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err == nil {
					src.root = strings.TrimSpace(string(top))
				}
			}

			if logs < 0 {
				return usageError{fmt.Errorf("--logs must be 0 or more, got %d", logs)}
			}

			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
			defer stop()

			klog.SetOutput(io.Discard) // the scheduler framework logs through klog
			klog.LogToStderr(false)

			cfg, err := helm.RESTClientGetter().ToRESTConfig()
			if err != nil {
				return fmt.Errorf("kubeconfig: %w", err)
			}
			c, err := kubernetes.NewForConfig(cfg)
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
					slog.SetDefault(slog.New(slog.DiscardHandler))
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
				slog.SetDefault(slog.New(slog.DiscardHandler))
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
	f.StringVar(&src.kustomize, "kustomize", "", "Kustomize overlay that deploys the pod: also show which entry and line set each field a failing check reads (experimental)")
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
		a, err := o.ExplainOverlay(value.KField{Doc: d.ID, Path: kp})
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

// traceCache is where traces are kept between runs, "" (no cache) with --no-cache or without a user cache dir.
func traceCache(off bool) string {
	dir, err := os.UserCacheDir()
	if off || err != nil {
		return ""
	}

	return filepath.Join(dir, "kwhy", "trace")
}

// valuesFlags registers Helm's values flags: -f and the --set family, applied in Helm's order.
func valuesFlags(f *pflag.FlagSet, o *values.Options, note string) {
	f.StringArrayVarP(&o.ValueFiles, "values", "f", nil, "values file or URL (repeatable, later wins)"+note)
	f.StringArrayVar(&o.Values, "set", nil, "set a value: key=value (repeatable, wins over files)"+note)
	f.StringArrayVar(&o.StringValues, "set-string", nil, "set a STRING value: key=value (repeatable)"+note)
	f.StringArrayVar(&o.JSONValues, "set-json", nil, "set a JSON value: key=json or a JSON object (repeatable)"+note)
	f.StringArrayVar(&o.FileValues, "set-file", nil, "set a value from a file's content: key=path (repeatable)"+note)
	f.StringArrayVar(&o.LiteralValues, "set-literal", nil, "set a literal STRING value, no escaping: key=value (repeatable)"+note)
}

// usageArgs is cobra.RangeArgs with a wrong count exiting 2.
func usageArgs(lo, hi int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := cobra.RangeArgs(lo, hi)(cmd, args); err != nil {
			return usageError{err}
		}

		return nil
	}
}

// onePathOrAll enforces that exactly one of <path> or --all is given.
func onePathOrAll(hasPath, all bool) error {
	if hasPath == all {
		return usageError{errors.New("give exactly one of <path> or --all")}
	}

	return nil
}

// pathError adds the closest valid paths of doc to a path resolution error.
func pathError(err error, docs []render.Doc, doc int, path string) error {
	var valid []string
	for f := range value.Flatten(docs) {
		if f.Doc == doc {
			valid = append(valid, f.Path)
		}
	}

	return fmt.Errorf("%w; did you mean %s", err, strings.Join(value.Suggest(valid, path, 3), ", "))
}
