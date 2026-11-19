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
	cmd.AddCommand(valueChartCmd())

	return cmd
}

// usageArgs is cobra.RangeArgs with a wrong count exiting 2.
func usageArgs(min, max int) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := cobra.RangeArgs(min, max)(cmd, args); err != nil {
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

func valueChartCmd() *cobra.Command {
	var (
		version, release, namespace, output string
		files, sets                         []string
		all                                 bool
		chunk                               int
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

			// Check the resource and path on one render before tracing, so a typo fails fast.
			o := render.Options{Release: release, Namespace: namespace}
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

			idx, err := value.Trace(ctx, c, v, o, chunk)
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
	f.StringVar(&version, "version", "", "chart version (for repo and oci charts)")
	f.StringArrayVarP(&files, "values", "f", nil, "values file (repeatable, later wins)")
	f.StringArrayVar(&sets, "set", nil, "set a value: key=value (repeatable, wins over files)")
	f.StringVar(&release, "release", "rel", "release name")
	f.StringVarP(&namespace, "namespace", "n", "default", "release namespace")
	f.StringVarP(&output, "output", "o", "human", "output format: human, markdown or json")
	f.BoolVar(&all, "all", false, "explain every field of the resource")
	f.IntVar(&chunk, "chunk-size", value.ChunkSize, "values leaves marked per tracing render (tuning)")

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
