package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"

	"github.com/spf13/cobra"
	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/cli/values"

	"github.com/semirm-dev/kwhy/render"
	"github.com/semirm-dev/kwhy/value"
)

var writers = map[string]func(io.Writer, []value.Answer) error{
	"human": value.WriteHuman, "markdown": value.WriteMarkdown, "json": value.WriteJSON,
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

			var field value.Field
			if !all {
				p, err := value.ResolvePath(docs[doc].Object, args[1])
				if err != nil {
					return pathError(err, docs, doc, args[1])
				}

				field = value.Field{Doc: doc, Path: p}
			}

			idx, err := value.Trace(ctx, c, v, o, chunk, traceCache(noCache))
			if err != nil {
				return err
			}

			if all {
				as, err := idx.ExplainAll(ctx, doc)
				if err != nil {
					return err
				}

				return writers[output](cmd.OutOrStdout(), as)
			}

			a, err := idx.Explain(ctx, field)
			if err != nil {
				return err
			}

			return writers[output](cmd.OutOrStdout(), []value.Answer{a})
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
		Short:   "Which kustomization entry and line set a built value",
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

			if root == "" {
				root = gitRoot()
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
				as, err := o.ExplainAll(d.ID)
				if err != nil {
					return err
				}
				return writers[output](cmd.OutOrStdout(), as)
			}

			p, err := o.FieldPath(d, args[1])
			if err != nil {
				return err
			}
			a, err := o.Explain(value.KField{Doc: d.ID, Path: p})
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
