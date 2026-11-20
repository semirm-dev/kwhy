package pod_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/version"
	fakediscovery "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/semirm-dev/kwhy/pod"
)

func TestTarget(t *testing.T) {
	labels := map[string]string{"app": "api"}
	at := func(p *v1.Pod, node string, age time.Duration) *v1.Pod {
		p.Namespace, p.Labels, p.Spec.NodeName = "default", labels, node
		p.CreationTimestamp = metav1.NewTime(time.Now().Add(-age))
		return p
	}
	deploy := func(name string) *appsv1.Deployment {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": name}},
			Template: v1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": name}},
				Spec: v1.PodSpec{Containers: []v1.Container{{Name: "c"}}}}}}
	}

	c := fake.NewClientset(
		deploy("api"), deploy("idle"),
		at(newPod("api-running", "", "1", "1Gi"), "n1", time.Hour),
		at(newPod("api-new", "", "1", "1Gi"), "", time.Minute),
		at(newPod("api-old", "", "1", "1Gi"), "", 10*time.Minute),
	)

	for _, tc := range []struct{ ref, want string }{
		{"api-running", "api-running"},
		{"pod/api-new", "api-new"},
		{"deploy/api", "api-old"}, // oldest unscheduled
		{"deploy/idle", "idle"},   // no pod waiting: from the template
	} {
		p, err := pod.Target(t.Context(), c, "default", tc.ref)
		if err != nil {
			t.Fatalf("%s: %v", tc.ref, err)
		}
		if p.Name != tc.want {
			t.Errorf("%s = %s, want %s", tc.ref, p.Name, tc.want)
		}
	}

	if _, err := pod.Target(t.Context(), c, "default", "nope"); err == nil {
		t.Error("missing pod: want an error")
	}
	if _, err := pod.Target(t.Context(), c, "default", "rs/agent"); err == nil || !strings.Contains(err.Error(), "unsupported kind") {
		t.Errorf("rs: err = %v", err)
	}
}

func TestWarnings(t *testing.T) {
	c := fake.NewClientset()
	c.Discovery().(*fakediscovery.FakeDiscovery).FakedServerVersion = &version.Info{GitVersion: "v1.30.0"}

	cl, err := pod.Snapshot(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}

	p := newPod("x", "", "1", "1Gi")
	p.Spec.SchedulerName = "volcano"

	ws := strings.Join(pod.Warnings(cl, p), "\n")
	if !strings.Contains(ws, `scheduler "volcano"`) {
		t.Errorf("no scheduler warning in %q", ws)
	}
	// Test binaries carry build info, so the compiled v1.37 scheduler is known.
	if !strings.Contains(ws, "cluster is v1.30.0") {
		t.Errorf("no version skew warning in %q", ws)
	}
}

func TestTargetWorstPod(t *testing.T) {
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "default"}, Spec: appsv1.DeploymentSpec{
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}}}
	onNode := func(name string, isReady bool, restarts int32) *v1.Pod {
		p := newPod(name, "n1", "1", "1Gi")
		p.Namespace, p.Labels = "default", map[string]string{"app": "web"}
		status := v1.ConditionFalse
		if isReady {
			status = v1.ConditionTrue
		}
		p.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: status}}
		p.Status.ContainerStatuses = []v1.ContainerStatus{{Name: "c", RestartCount: restarts}}
		return p
	}

	c := fake.NewClientset(deploy, onNode("web-ok", true, 0), onNode("web-flaky", false, 2), onNode("web-crashing", false, 14))
	p, err := pod.Target(t.Context(), c, "default", "deploy/web")
	if err != nil || p.Name != "web-crashing" {
		t.Errorf("broken pods: got %v, %v; want web-crashing", p, err)
	}

	c = fake.NewClientset(deploy, onNode("web-a", true, 0), onNode("web-b", true, 3))
	_, err = pod.Target(t.Context(), c, "default", "deploy/web")
	var h pod.Healthy
	if !errors.As(err, &h) || h.Pods != 2 || err.Error() != "all 2 pods ready" {
		t.Errorf("all ready: err = %v", err)
	}
}
