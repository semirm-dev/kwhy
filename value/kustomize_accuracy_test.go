//go:build accuracy

package value_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/semirm-dev/kwhy/value"
)

// TestOverlayAccuracy runs Kustomize attribution on the spike's public overlays (scripts/fetch-overlays.sh) and
// checks the guarantee: every line kwhy names shows the field's value. It reports how many fields it could only
// pin to an entry or a file.
func TestOverlayAccuracy(t *testing.T) {
	for _, tc := range []struct{ repo, overlay string }{
		{"podinfo", "kustomize"},
		{"flux2-example", "apps/production"},
		{"flux2-example", "infrastructure/controllers"},
		{"microservices-demo", "kustomize/tests/memorystore-with-all-components"},
		{"microservices-demo", "kustomize/tests/service-mesh-istio-with-all-components"},
		{"argo-cd", "manifests/cluster-install"},
		{"argo-cd", "manifests/ha/cluster-install"},
	} {
		t.Run(tc.repo+"/"+tc.overlay, func(t *testing.T) {
			root, err := filepath.Abs(filepath.Join("../testdata/kustomize/public", tc.repo))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(root); err != nil {
				t.Skip("run scripts/fetch-overlays.sh first")
			}
			t.Chdir(root)

			o, err := value.LoadOverlay(".", tc.overlay)
			if err != nil {
				t.Fatal(err)
			}

			var fields, lines, entryOnly, fileOnly int
			for _, d := range o.Docs {
				as, err := o.ExplainAllOverlay(d.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, a := range as {
					fields++
					src := a.Leaves[0].Chain[0]
					switch {
					case a.Kind == value.Chart:
						fileOnly++
					case a.Kind == value.Composed || src.Line == 0:
						entryOnly++
					case len(a.Leaves) == 1 && a.Leaves[0].Entry != "" && len(a.Leaves[0].Entry) > 0 &&
						a.Leaves[0].Entry[len(a.Leaves[0].Entry)-1] == ')':
						entryOnly++ // "(field's line not confirmed)"
					default:
						lines++
						if !o.LineShows(src.File, src.Line, a.Value) {
							t.Errorf("%s %s = %v names %s:%d, which doesn't show it", a.Resource, a.Field.Path, a.Value, src.File, src.Line)
						}
					}
				}
			}
			t.Logf("%d fields: %d with a line that shows the value, %d entry only, %d file only", fields, lines, entryOnly, fileOnly)
		})
	}
}
