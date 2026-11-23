package value

import (
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"

	"sigs.k8s.io/kustomize/kyaml/filesys"
)

// Workers that all fail while jobs are left must not leave the sender blocked.
func TestRunBuildsFailingWorkersDontHang(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	o := &Overlay{mem: filesys.MakeFsInMemory(), target: "/"}
	fail := buildJob{files: map[string][]byte{"/missing.yaml": nil}}

	done := make(chan error)
	go func() {
		_, err := o.runBuilds(t.Context(), nil, []buildJob{fail, fail})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("runBuilds with failing jobs: no error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("runBuilds hangs when its workers fail")
	}
}

// Only the overlay's stack is copied into memory: the rest of the repo (here a big unrelated file) stays on disk,
// and every worker copies only the stack.
func TestLoadOverlayCopiesOnlyTheStack(t *testing.T) {
	repo := t.TempDir()
	if err := os.CopyFS(repo, os.DirFS("../testdata/kustomize/fixture")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, "blob.bin"), make([]byte, 1<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)

	o, err := LoadOverlay(t.Context(), ".", "overlays/prod", "")
	if err != nil {
		t.Fatal(err)
	}
	got := slices.Sorted(maps.Keys(o.orig))
	want := []string{"/base/deployment.yaml", "/base/kustomization.yaml", "/base/service.yaml",
		"/components/monitoring/annotations.yaml", "/components/monitoring/kustomization.yaml",
		"/overlays/prod/kustomization.yaml", "/overlays/prod/memory.yaml",
		"/overlays/staging/kustomization.yaml", "/overlays/staging/memory.yaml"}
	if !slices.Equal(got, want) {
		t.Errorf("copied %q, want %q", got, want)
	}
}
