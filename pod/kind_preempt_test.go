//go:build kind

package pod_test

import (
	"context"
	"os"
	"slices"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	resourcehelper "k8s.io/component-helpers/resource"

	"github.com/semirm-dev/kwhy/pod"
)

// TestKindPreemption checks kwhy's preemption dry run against the scheduler's real preemption. A low-priority pod
// fills each worker; the one on worker is guarded by a PodDisruptionBudget. A high-priority target fits neither,
// so the scheduler must preempt, and should pick worker2 to break no budget. kwhy evaluates the target before it
// exists; then the target is created and the scheduler's pick and evictions must match kwhy's.
func TestKindPreemption(t *testing.T) {
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
	ctx := t.Context()
	cluster := kctx[len("kind-"):]
	workers := []string{cluster + "-worker", cluster + "-worker2"}

	// Pods of earlier tests still terminating hold cpu now and free it later, which would let the target fit
	// without preempting.
	free := map[string]int64{}
	for deadline := time.Now().Add(3 * time.Minute); ; time.Sleep(3 * time.Second) {
		terminating := false
		for _, w := range workers {
			if free[w], terminating = freeCPU(t, c, w); terminating {
				break
			}
		}
		if !terminating {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("pods still terminating on the workers")
		}
	}

	ns := "kwhy-preempt"
	if _, err := c.CoreV1().Namespaces().Create(ctx, &v1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns}}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx := context.Background() // t.Context() is canceled before cleanups run
		c.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
		// Wait, so a rerun can create the namespace again and later tests don't share the workers with its pods.
		for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
			if _, err := c.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{}); apierrors.IsNotFound(err) {
				break
			}
		}
		for _, pc := range []string{"kwhy-low", "kwhy-high"} {
			c.SchedulingV1().PriorityClasses().Delete(ctx, pc, metav1.DeleteOptions{})
		}
	})
	for name, v := range map[string]int32{"kwhy-low": 100, "kwhy-high": 1000} {
		pc := &schedulingv1.PriorityClass{ObjectMeta: metav1.ObjectMeta{Name: name}, Value: v}
		if _, err := c.SchedulingV1().PriorityClasses().Create(ctx, pc, metav1.CreateOptions{}); err != nil && !apierrors.IsAlreadyExists(err) {
			t.Fatal(err)
		}
	}

	guarded := map[string]string{"app": "guarded"}
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "guard"},
		Spec: policyv1.PodDisruptionBudgetSpec{MinAvailable: new(intstr.FromInt32(1)), Selector: &metav1.LabelSelector{MatchLabels: guarded}}}
	if _, err := c.PolicyV1().PodDisruptionBudgets(ns).Create(ctx, pdb, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	// Each low pod leaves 200m free; the target needs 500m.
	lows := map[string]string{workers[0]: "low-a", workers[1]: "low-b"}
	for _, w := range workers {
		p := preemptPod(lows[w], "kwhy-low", free[w]-200)
		p.Spec.NodeName = w
		if w == workers[0] {
			p.Labels = guarded
		}
		if _, err := c.CoreV1().Pods(ns).Create(ctx, p, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	for deadline := time.Now().Add(2 * time.Minute); ; time.Sleep(2 * time.Second) {
		got, err := c.PolicyV1().PodDisruptionBudgets(ns).Get(ctx, "guard", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if podRunning(t, c, ns, "low-a") && podRunning(t, c, ns, "low-b") && got.Status.CurrentHealthy == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("low pods not running, or the budget not counted")
		}
	}

	target := preemptPod("target", "kwhy-high", 500)
	target.Namespace = ns
	target.Spec.Priority = new(int32(1000)) // admission sets it from the class on create

	cl, err := pod.Snapshot(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	r, err := pod.Evaluate(ctx, cl, target)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("kwhy: preempt %v, pick %q, evict %v, breaks %d", r.Preempt, r.PreemptNode, r.Victims, r.PDBViolations)
	if !slices.Equal(r.Preempt, workers) || r.PreemptNode != workers[1] || !slices.Equal(r.Victims, []string{ns + "/low-b"}) || r.PDBViolations != 0 {
		t.Fatalf("kwhy: want preempt %v, pick %s, evict [%s/low-b], break 0", workers, workers[1], ns)
	}

	target.Spec.Priority = nil
	if _, err := c.CoreV1().Pods(ns).Create(ctx, target, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}

	var node string
	for deadline := time.Now().Add(90 * time.Second); node == ""; time.Sleep(time.Second) {
		p, err := c.CoreV1().Pods(ns).Get(ctx, "target", metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		node = p.Status.NominatedNodeName
		if node == "" {
			node = p.Spec.NodeName
		}
		if time.Now().After(deadline) {
			t.Fatal("the scheduler never nominated or bound the target")
		}
	}
	t.Logf("scheduler: picked %s", node)
	if node != r.PreemptNode {
		t.Errorf("scheduler picked %s, kwhy %s", node, r.PreemptNode)
	}
	// With async preemption the target can be nominated before the victim's delete call.
	for deadline := time.Now().Add(30 * time.Second); !evicted(t, c, ns, "low-b"); time.Sleep(time.Second) {
		if time.Now().After(deadline) {
			t.Fatal("scheduler didn't evict low-b, kwhy said it would")
		}
	}
	if evicted(t, c, ns, "low-a") {
		t.Error("scheduler evicted low-a, kwhy said it wouldn't")
	}
}

func preemptPod(name, class string, milliCPU int64) *v1.Pod {
	return &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: v1.PodSpec{
		PriorityClassName: class,
		Tolerations:       []v1.Toleration{{Key: "kwhy/gpu", Operator: v1.TolerationOpExists}},
		Containers: []v1.Container{{Name: "c", Image: "registry.k8s.io/pause:3.10", Resources: v1.ResourceRequirements{
			Requests: v1.ResourceList{v1.ResourceCPU: *resource.NewMilliQuantity(milliCPU, resource.DecimalSI)}}}}}}
}

// freeCPU is node's allocatable cpu minus what its pods request, in millicores, and whether any pod there is
// terminating.
func freeCPU(t *testing.T, c kubernetes.Interface, node string) (int64, bool) {
	t.Helper()

	n, err := c.CoreV1().Nodes().Get(t.Context(), node, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pods, err := c.CoreV1().Pods("").List(t.Context(), metav1.ListOptions{FieldSelector: "spec.nodeName=" + node})
	if err != nil {
		t.Fatal(err)
	}

	free := n.Status.Allocatable.Cpu().MilliValue()
	for _, p := range pods.Items {
		if p.Status.Phase == v1.PodSucceeded || p.Status.Phase == v1.PodFailed {
			continue
		}
		if p.DeletionTimestamp != nil {
			return 0, true
		}
		req := resourcehelper.PodRequests(&p, resourcehelper.PodResourcesOptions{})
		free -= req.Cpu().MilliValue()
	}

	return free, false
}

func podRunning(t *testing.T, c kubernetes.Interface, ns, name string) bool {
	t.Helper()

	p, err := c.CoreV1().Pods(ns).Get(t.Context(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	return p.Status.Phase == v1.PodRunning
}

func evicted(t *testing.T, c kubernetes.Interface, ns, name string) bool {
	t.Helper()

	p, err := c.CoreV1().Pods(ns).Get(t.Context(), name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return true
	}
	if err != nil {
		t.Fatal(err)
	}

	return p.DeletionTimestamp != nil
}
