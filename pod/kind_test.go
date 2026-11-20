//go:build kind

// The kind suite checks kwhy against the real scheduler: for each fixture in testdata/pod, the scheduler's first
// failing check per node (the PodScheduled message) must equal kwhy's, and kwhy must list it among all checks.
// Run: kind create cluster --name kwhy-pod --config testdata/pod/kind.yaml && go test -tags kind -run TestKind ./pod/
// KWHY_CONTEXT picks another kube context (default kind-kwhy-pod).
package pod_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/semirm-dev/kwhy/pod"
)

func TestKindParity(t *testing.T) {
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

	worker := strings.TrimPrefix(kctx, "kind-") + "-worker"
	if out, err := kubectl("taint", "node", worker, "kwhy/gpu=true:NoSchedule", "--overwrite"); err != nil {
		t.Fatalf("taint %s: %v\n%s", worker, err, out)
	}

	files, err := filepath.Glob("../testdata/pod/*.yaml")
	if err != nil {
		t.Fatal(err)
	}

	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".yaml")
		if name == "kind" {
			continue
		}

		t.Run(name, func(t *testing.T) {
			ns := "kwhy-pod-" + name
			if out, err := kubectl("create", "namespace", ns); err != nil {
				t.Fatalf("%v\n%s", err, out)
			}
			t.Cleanup(func() {
				kubectl("delete", "namespace", ns, "--wait=false")
				kubectl("delete", "pv", "-l", "topology.kubernetes.io/zone=c", "--wait=false")
			})

			// Anchors first and running, so the scheduler sees them before the target.
			if out, err := kubectl("apply", "-n", ns, "-f", f, "-l", "!kwhy/target"); err != nil && !strings.Contains(out, "no objects passed") {
				t.Fatalf("apply anchors: %v\n%s", err, out)
			}
			if out, err := kubectl("wait", "-n", ns, "--for=condition=Ready", "pod", "--all", "--timeout=120s"); err != nil &&
				!strings.Contains(out, "no matching resources") {
				t.Fatalf("anchors not ready: %v\n%s", err, out)
			}
			if out, err := kubectl("apply", "-n", ns, "-f", f, "-l", "kwhy/target=true"); err != nil {
				t.Fatalf("apply target: %v\n%s", err, out)
			}

			msg := unschedulable(t, c, ns)

			cl, err := pod.Snapshot(t.Context(), c)
			if err != nil {
				t.Fatal(err)
			}
			p, err := pod.Target(t.Context(), c, ns, "target")
			if err != nil {
				t.Fatal(err)
			}
			r, err := pod.Evaluate(t.Context(), cl, p)
			if err != nil {
				t.Fatal(err)
			}

			want := fitMessage(r)
			t.Logf("scheduler: %s", msg)
			if !strings.HasPrefix(msg, want) {
				t.Errorf("scheduler: %s\nkwhy:      %s", msg, want)
			}

			for _, n := range r.Nodes {
				t.Logf("%s: all %v", n.Name, n.Failed)
				if n.First != nil && !slices.ContainsFunc(n.Failed, func(c pod.Check) bool { return c.Plugin == n.First.Plugin }) {
					t.Errorf("%s: first failing %s missing from all checks %v", n.Name, n.First.Plugin, n.Failed)
				}
			}
		})
	}
}

// unschedulable waits for the target's PodScheduled=False condition and returns its message.
func unschedulable(t *testing.T, c kubernetes.Interface, ns string) string {
	t.Helper()

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		p, err := c.CoreV1().Pods(ns).Get(t.Context(), "target", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if p.Spec.NodeName != "" {
			t.Fatalf("target was scheduled on %s", p.Spec.NodeName)
		}
		for _, cond := range p.Status.Conditions {
			if cond.Type == v1.PodScheduled && cond.Status == v1.ConditionFalse && cond.Reason == v1.PodReasonUnschedulable {
				return cond.Message
			}
		}
		time.Sleep(time.Second)
	}
	t.Fatal("target never reported Unschedulable")

	return ""
}

// fitMessage rebuilds the scheduler's FitError text from kwhy's first failing check per node.
func fitMessage(r pod.Result) string {
	counts := map[string]int{}
	for _, n := range r.Nodes {
		if n.First != nil {
			for _, s := range n.First.Reasons {
				counts[s]++
			}
		}
	}

	var parts []string
	for s, n := range counts {
		parts = append(parts, fmt.Sprintf("%d %s", n, s))
	}
	sort.Strings(parts)

	return fmt.Sprintf("0/%d nodes are available: %s.", len(r.Nodes), strings.Join(parts, ", "))
}
