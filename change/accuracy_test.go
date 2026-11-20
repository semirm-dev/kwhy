//go:build accuracy

package change_test

import (
	"path/filepath"
	"testing"

	"helm.sh/helm/v4/pkg/cli"
	helmvalues "helm.sh/helm/v4/pkg/cli/values"

	"github.com/semirm-dev/kwhy/change"
	"github.com/semirm-dev/kwhy/render"
	"github.com/semirm-dev/kwhy/value"
)

// TestAccuracyUpgrades upgrades four real charts by a minor version (testdata/charts/prev → public)
// and requires no "will fail": these upgrades apply in practice, so any would be a false alarm.
func TestAccuracyUpgrades(t *testing.T) {
	for _, name := range []string{"ingress-nginx", "argo-cd", "cert-manager", "grafana"} {
		t.Run(name, func(t *testing.T) {
			values := filepath.Join("..", "testdata", "charts", "public", name+".prod.yaml")
			base := renderChart(t, filepath.Join("..", "testdata", "charts", "prev", name+".tgz"), values)
			head := renderChart(t, filepath.Join("..", "testdata", "charts", "public", name+".tgz"), values)

			is := change.Diff(base, head)
			if len(is) == 0 {
				t.Fatal("the upgrade predicts nothing")
			}
			for _, i := range is {
				t.Logf("%s %s %s", i.Verdict, i.Resource, i.Detail)
				if i.Verdict == change.WillFail {
					t.Errorf("false will fail: %s %s", i.Resource, i.Detail)
				}
			}
		})
	}
}

func renderChart(t *testing.T, path, values string) []render.Doc {
	t.Helper()

	c, err := render.LoadChart(path, "", cli.New())
	if err != nil {
		t.Fatal(err)
	}
	v, err := value.LoadValues(c, helmvalues.Options{ValueFiles: []string{values}})
	if err != nil {
		t.Fatal(err)
	}
	docs, err := c.Render(t.Context(), v.User, render.Options{Release: "rel", Namespace: "prod"})
	if err != nil {
		t.Fatal(err)
	}

	return docs
}
