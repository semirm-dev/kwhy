//go:build kind

package pod_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/semirm-dev/kwhy/pod"
)

// TestKindController checks the problems a workload's controller reports (testdata/pod/controller) and a pod
// stuck terminating, against a real cluster.
func TestKindController(t *testing.T) {
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

	setup := func(t *testing.T, name string) string {
		ns := "kwhy-controller-" + name
		if out, err := kubectl("create", "namespace", ns); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		t.Cleanup(func() { kubectl("delete", "namespace", ns, "--wait=false") })
		if out, err := kubectl("apply", "-n", ns, "-f", "../testdata/pod/controller/"+name+".yaml"); err != nil {
			t.Fatalf("apply: %v\n%s", err, out)
		}
		return ns
	}

	// wait polls check every 3s for up to limit.
	wait := func(t *testing.T, limit time.Duration, check func() ([]pod.Problem, error), kind string) pod.Problem {
		var ps []pod.Problem
		for deadline := time.Now().Add(limit); time.Now().Before(deadline); time.Sleep(3 * time.Second) {
			var err error
			if ps, err = check(); err != nil {
				t.Fatal(err)
			}
			for _, p := range ps {
				if p.Kind == kind {
					t.Logf("%+v", p)
					return p
				}
			}
		}
		t.Fatalf("no %s problem; last: %+v", kind, ps)
		return pod.Problem{}
	}

	t.Run("quota", func(t *testing.T) {
		t.Parallel()
		ns := setup(t, "quota")
		wait(t, 2*time.Minute, func() ([]pod.Problem, error) { return pod.Controller(t.Context(), c, ns, "deploy/target") }, pod.Create)

		if p, err := pod.Target(t.Context(), c, ns, "deploy/target"); err != nil || !pod.IsTemplate(p) {
			t.Errorf("target = %v, %v; want the template (no pods)", p, err)
		}
	})

	t.Run("job", func(t *testing.T) {
		t.Parallel()
		ns := setup(t, "job")
		pr := wait(t, 2*time.Minute, func() ([]pod.Problem, error) { return pod.Controller(t.Context(), c, ns, "job/target") }, pod.JobFail)
		if pr.Reason != "BackoffLimitExceeded" {
			t.Errorf("reason = %s, want BackoffLimitExceeded", pr.Reason)
		}
	})

	t.Run("finalizer", func(t *testing.T) {
		t.Parallel()
		ns := setup(t, "finalizer")
		t.Cleanup(func() {
			kubectl("patch", "pod", "target", "-n", ns, "--type=json", `-p=[{"op":"remove","path":"/metadata/finalizers"}]`)
		})

		if out, err := kubectl("wait", "-n", ns, "--for=condition=Ready", "pod/target", "--timeout=90s"); err != nil {
			t.Fatalf("not ready: %v\n%s", err, out)
		}
		if out, err := kubectl("delete", "pod", "target", "-n", ns, "--wait=false"); err != nil {
			t.Fatalf("delete: %v\n%s", err, out)
		}

		pr := wait(t, 3*time.Minute, func() ([]pod.Problem, error) {
			p, err := pod.Target(t.Context(), c, ns, "target")
			if err != nil {
				return nil, err
			}
			return pod.Diagnose(t.Context(), c, p)
		}, pod.Stuck)
		if pr.Detail == "" || !strings.Contains(pr.Detail, "finalizers: kwhy.test/hold") {
			t.Errorf("detail = %q", pr.Detail)
		}
	})
}
