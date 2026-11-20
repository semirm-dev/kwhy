//go:build kind

package pod_test

import (
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/semirm-dev/kwhy/pod"
)

// TestKindRunning breaks a pod on a node in each way of testdata/pod/running and checks that kwhy names the
// problem kubelet reports, for the right container.
// Run: kind create cluster --name kwhy-pod --config testdata/pod/kind.yaml && go test -tags kind -run TestKindRunning ./pod/
func TestKindRunning(t *testing.T) {
	kctx := os.Getenv("KWHY_CONTEXT")
	if kctx == "" {
		kctx = "kind-kwhy-pod"
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{CurrentContext: kctx}).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	c, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}

	kubectl := func(args ...string) (string, error) {
		out, err := exec.Command("kubectl", append([]string{"--context", kctx}, args...)...).CombinedOutput()
		return string(out), err
	}

	for _, tc := range []struct {
		fixture, kind, container string
		init                     bool
	}{
		{"pull", pod.Pull, "app", false},
		{"crash", pod.Crash, "app", false},
		{"oom", pod.OOM, "app", false},
		{"probe", pod.Probe, "app", false},
		{"config", pod.Config, "app", false},
		{"mount", pod.Mount, "", false},
		{"init", pod.Crash, "migrate", true},
	} {
		t.Run(tc.fixture, func(t *testing.T) {
			t.Parallel()

			ns := "kwhy-running-" + tc.fixture
			if out, err := kubectl("create", "namespace", ns); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			t.Cleanup(func() { kubectl("delete", "namespace", ns, "--wait=false") })

			if out, err := kubectl("apply", "-n", ns, "-f", "../testdata/pod/running/"+tc.fixture+".yaml"); err != nil {
				t.Fatalf("apply: %v\n%s", err, out)
			}

			// Kubelet needs a few restarts for CrashLoopBackOff and a few probe periods for Unhealthy.
			var ps []pod.Problem
			for deadline := time.Now().Add(3 * time.Minute); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
				p, err := pod.Target(t.Context(), c, ns, "target")
				if err != nil {
					t.Fatal(err)
				}
				if p.Spec.NodeName == "" {
					continue
				}
				if ps, err = pod.Diagnose(t.Context(), c, p); err != nil {
					t.Fatal(err)
				}
				if len(ps) > 0 && ps[0].Kind == tc.kind {
					break
				}
			}

			if len(ps) == 0 {
				t.Fatal("no problem found")
			}
			got := ps[0]
			t.Logf("%+v", got)
			if got.Kind != tc.kind || got.Container != tc.container || got.Init != tc.init {
				t.Errorf("got %s %q init=%v, want %s %q init=%v (all: %+v)", got.Kind, got.Container, got.Init,
					tc.kind, tc.container, tc.init, ps)
			}
			if got.Detail == "" || strings.Contains(got.Detail, "kubelet reports no reason") {
				t.Errorf("no detail: %+v", got)
			}

			if tc.fixture == "crash" {
				p, err := pod.Target(t.Context(), c, ns, "target")
				if err != nil {
					t.Fatal(err)
				}
				pod.AddLogs(t.Context(), c, p, ps, 5)
				if !slices.Contains(ps[0].Logs, "kwhy-crash-log") {
					t.Errorf("logs = %q, want the crashed run's line", ps[0].Logs)
				}
			}
		})
	}
}
