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
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/cli/values"
	"k8s.io/client-go/kubernetes"
)

// version is set at release time with -ldflags "-X main.version=...".
var version = "dev"

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
	root.AddCommand(valueCmd(), changeCmd(), podCmd())

	// Helm logs through slog; kwhy reports errors itself.
	slog.SetDefault(slog.New(slog.DiscardHandler))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	err := root.ExecuteContext(ctx)
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

// traceCache is where traces are kept between runs, "" (no cache) with --no-cache or without a user cache dir.
func traceCache(off bool) string {
	dir, err := os.UserCacheDir()
	if off || err != nil {
		return ""
	}

	return filepath.Join(dir, "kwhy", "trace")
}

// gitRoot is the repo root, or "." outside a git repo: the whole overlay stack (bases, components) must be under it.
func gitRoot() string {
	top, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "."
	}

	return strings.TrimSpace(string(top))
}

// kubeClient connects to the cluster of helm's kube context.
func kubeClient(helm *cli.EnvSettings) (*kubernetes.Clientset, error) {
	cfg, err := helm.RESTClientGetter().ToRESTConfig()
	if err != nil {
		return nil, err
	}

	return kubernetes.NewForConfig(cfg)
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
