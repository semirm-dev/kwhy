package value

import (
	"runtime"
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
		_, err := o.runBuilds(nil, []buildJob{fail, fail})
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
