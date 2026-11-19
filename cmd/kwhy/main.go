// Command kwhy answers why questions about Kubernetes config. Wiring only: flags in, features out.
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

	"github.com/semirm-dev/kwhy/render"
	"github.com/semirm-dev/kwhy/value"
)

var writers = map[string]func(io.Writer, []value.Answer) error{
	"human": value.WriteHuman, "markdown": value.WriteMarkdown, "json": value.WriteJSON,
}

// usageError marks errors that exit with code 2.
type usageError struct{ error }

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run returns the exit code: 0 ok, 1 runtime error, 2 usage error.
func run(args []string, stdout, stderr io.Writer) int {
	root := &cobra.Command{
		Use:           "kwhy",
		Short:         "Answers why questions about Kubernetes config",
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

	root.SetArgs(args)
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	root.AddCommand(valueCmd(), notYet("change", "What merging a change does to running workloads (v0.3)"),
		notYet("pod", "Why a pod is not scheduled (v0.4)"))

	err := root.Execute()
	if err == nil {
		return 0
	}

	fmt.Fprintln(stderr, "kwhy:", err)
	if errors.As(err, &usageError{}) {
		return 2
	}

	return 1
}

func valueCmd() *cobra.Command {
	var (
		chartRef, version, release, namespace, output, key string
		files, sets                                        []string
		all                                                bool
	)

	helm := cli.New()
	cmd := &cobra.Command{
		Use:   "value <kind>/<name> [<path>]",
		Short: "Where a rendered value comes from",
		Example: `  kwhy value deploy/api '.spec.template.spec.containers[name=api].resources.requests.memory' \
    --chart ./charts/api -f values-prod.yaml`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 2 || len(args) == 0 && key == "" {
				return usageError{errors.New("value takes <kind>/<name> and an optional <path>")}
			}

			if chartRef == "" {
				return usageError{errors.New("--chart is required")}
			}

			if n := countTrue(len(args) == 2, all, key != ""); n != 1 {
				return usageError{errors.New("give exactly one of <path>, --all or --key")}
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

			c, err := render.LoadChart(chartRef, version, helm)
			if err != nil {
				return err
			}

			v, err := value.LoadValues(c, files, sets)
			if err != nil {
				return err
			}

			o := render.Options{Release: release, Namespace: namespace}
			idx, err := value.Trace(ctx, c, v, o)
			if err != nil {
				return err
			}

			doc := -1
			if len(args) > 0 {
				if doc, err = value.SelectDoc(idx.Docs, args[0], selectNS); err != nil {
					return err
				}
			}

			var fields []value.Field
			switch {
			case all:
				as, err := value.ExplainAll(ctx, idx, c, v, o, doc)
				if err != nil {
					return err
				}

				return writers[output](cmd.OutOrStdout(), as)

			case key != "":
				i := slices.IndexFunc(v.Leaves, func(l value.Leaf) bool { return value.PathString(l.Path) == key })
				if i < 0 {
					return fmt.Errorf("unknown values key %s", key)
				}

				for _, f := range value.Affects(idx, i) {
					if doc < 0 || f.Doc == doc {
						fields = append(fields, f)
					}
				}

			default:
				p, err := value.ResolvePath(idx.Docs[doc].Object, args[1])
				if err != nil {
					var valid []string
					for f := range idx.Fields {
						if f.Doc == doc {
							valid = append(valid, f.Path)
						}
					}

					return fmt.Errorf("%w; did you mean %s", err, strings.Join(value.Suggest(valid, args[1], 3), ", "))
				}

				fields = append(fields, value.Field{Doc: doc, Path: p})
			}

			var as []value.Answer
			for _, f := range fields {
				a, err := value.Explain(ctx, idx, c, v, o, f)
				if err != nil {
					return err
				}
				as = append(as, a)
			}

			return writers[output](cmd.OutOrStdout(), as)
		},
	}

	f := cmd.Flags()
	f.StringVar(&chartRef, "chart", "", "chart directory, .tgz, oci:// reference or repo/chart")
	f.StringVar(&version, "version", "", "chart version (for repo and oci charts)")
	f.StringArrayVarP(&files, "values", "f", nil, "values file (repeatable, later wins)")
	f.StringArrayVar(&sets, "set", nil, "set a value: key=value (repeatable, wins over files)")
	f.StringVar(&release, "release", "rel", "release name")
	f.StringVarP(&namespace, "namespace", "n", "default", "release namespace")
	f.StringVarP(&output, "output", "o", "human", "output format: human, markdown or json")
	f.BoolVar(&all, "all", false, "explain every field of the resource")
	f.StringVar(&key, "key", "", "reverse lookup: rendered fields affected by this values key")

	// Only Helm's chart lookup flags: value is offline, so the cluster flags don't apply.
	f.StringVar(&helm.RepositoryConfig, "repository-config", helm.RepositoryConfig, "path to Helm's repositories file")
	f.StringVar(&helm.RepositoryCache, "repository-cache", helm.RepositoryCache, "path to Helm's repository index cache")
	f.StringVar(&helm.RegistryConfig, "registry-config", helm.RegistryConfig, "path to Helm's registry config")
	f.StringVar(&helm.ContentCache, "content-cache", helm.ContentCache, "path to Helm's chart content cache")

	return cmd
}

func notYet(name, short string) *cobra.Command {
	return &cobra.Command{
		Use:   name,
		Short: short,
		RunE: func(*cobra.Command, []string) error {
			return fmt.Errorf("%s: not implemented yet", name)
		},
	}
}

func countTrue(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}

	return n
}
