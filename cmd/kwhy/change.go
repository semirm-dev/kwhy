package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"

	"github.com/spf13/cobra"
	"helm.sh/helm/v4/pkg/chart/common"
	"helm.sh/helm/v4/pkg/cli"

	"github.com/semirm-dev/kwhy/change"
	"github.com/semirm-dev/kwhy/render"
)

var (
	changeWriters = map[string]func(io.Writer, []change.Impact) error{
		"human": change.WriteHuman, "markdown": change.WriteMarkdown, "json": change.WriteJSON,
	}

	// failVerdicts maps --fail-on names to verdicts: each verdict with - for spaces, plus fail and data-loss.
	failVerdicts = func() map[string]change.Verdict {
		m := map[string]change.Verdict{"fail": change.WillFail, "data-loss": change.Data}
		for _, v := range change.Verdicts {
			m[strings.ReplaceAll(string(v), " ", "-")] = v
		}

		return m
	}()
)

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

			ctx := cmd.Context()

			targets := []change.Target{{Source: src, Release: release, Namespace: namespace}}
			if config != "" {
				var err error
				if targets, err = change.LoadConfig(config); err != nil {
					return err
				}
			}

			b, h, warnings, err := change.Render(ctx, base, head, targets, apps, kubeVersion, helm)
			if errors.Is(err, change.ErrNotFound) {
				return usageError{err}
			}
			if err != nil {
				return err
			}

			for _, w := range warnings {
				fmt.Fprintln(cmd.ErrOrStderr(), "kwhy: warning:", w)
			}

			is := change.Diff(b, h)
			if cmd.Flags().Changed("context") {
				c, err := kubeClient(helm)
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
	f.StringVar(&src.Manifests, "manifests", "", "directory of plain YAML or a Kustomize overlay in the repo, instead of --chart")
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
