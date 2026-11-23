package main

import (
	"errors"
	"fmt"
	"io"
	"reflect"

	"github.com/spf13/cobra"
	"helm.sh/helm/v4/pkg/cli"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"github.com/semirm-dev/kwhy/pod"
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
			if src.chart == "" && !reflect.ValueOf(src.values).IsZero() {
				return usageError{errors.New("values flags apply to --chart only")}
			}
			if src.kustomize != "" && src.root == "" {
				src.root = gitRoot()
			}

			if logs < 0 {
				return usageError{fmt.Errorf("--logs must be 0 or more, got %d", logs)}
			}

			ctx := cmd.Context()

			klog.SetOutput(io.Discard) // the scheduler framework logs through klog
			klog.LogToStderr(false)

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
				if len(ps) > 0 {
					if r.Sources, err = src.sources(ctx, c, p, r, helm); err != nil {
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

			if r.Sources, err = src.sources(ctx, c, p, r, helm); err != nil {
				return err
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
