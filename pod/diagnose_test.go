package pod_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/semirm-dev/kwhy/pod"
)

// running is a pod on n1 with containers app and sidecar, not ready unless every status says so.
func running(statuses ...v1.ContainerStatus) *v1.Pod {
	p := newPod("api-1", "n1", "1", "1Gi")
	p.Spec.Containers = []v1.Container{
		{Name: "app", Image: "acme/api:9.9", Resources: v1.ResourceRequirements{Limits: v1.ResourceList{v1.ResourceMemory: resource.MustParse("512Mi")}}},
		{Name: "sidecar", Image: "proxy:1"},
	}
	p.Status.Phase = v1.PodRunning
	p.Status.ContainerStatuses = statuses

	isReady := v1.ConditionTrue
	for _, cs := range statuses {
		if !cs.Ready {
			isReady = v1.ConditionFalse
		}
	}
	p.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: isReady}}

	return p
}

func ok(name string) v1.ContainerStatus {
	return v1.ContainerStatus{Name: name, Ready: true, State: v1.ContainerState{Running: &v1.ContainerStateRunning{}}}
}

func waiting(name, reason, msg string) v1.ContainerStatus {
	return v1.ContainerStatus{Name: name, Image: "acme/api:9.9", State: v1.ContainerState{Waiting: &v1.ContainerStateWaiting{Reason: reason, Message: msg}}}
}

func warning(p *v1.Pod, container, reason, msg string) *v1.Event {
	path := ""
	if container != "" {
		path = "spec.containers{" + container + "}"
	}

	return &v1.Event{Name: p.Name + "." + reason, Namespace: p.Namespace,
		InvolvedObject: v1.ObjectReference{UID: p.UID, FieldPath: path}, Type: v1.EventTypeWarning, Reason: reason, Message: msg,
		LastTimestamp: metav1.NewTime(time.Now())}
}

func diagnose(t *testing.T, p *v1.Pod, objs ...runtime.Object) []pod.Problem {
	t.Helper()

	ps, err := pod.Diagnose(t.Context(), fake.NewClientset(objs...), p)
	if err != nil {
		t.Fatal(err)
	}

	return ps
}

func TestDiagnose(t *testing.T) {
	crashed := waiting("app", "CrashLoopBackOff", "back-off 5m0s restarting failed container")
	crashed.RestartCount = 14
	crashed.LastTerminationState.Terminated = &v1.ContainerStateTerminated{Reason: "Error", ExitCode: 1}

	oomed := crashed
	oomed.LastTerminationState.Terminated = &v1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}

	notReady := ok("app")
	notReady.Ready = false

	recovered := ok("app")
	recovered.RestartCount = 2
	recovered.LastTerminationState.Terminated = &v1.ContainerStateTerminated{Reason: "OOMKilled", ExitCode: 137}

	pull := running(waiting("app", "ImagePullBackOff", `Back-off pulling image "acme/api:9.9"`), ok("sidecar"))
	probe := running(notReady, ok("sidecar"))
	mount := running(waiting("app", "ContainerCreating", ""), waiting("sidecar", "ContainerCreating", ""))
	evicted := running()
	evicted.Status.Phase, evicted.Status.Reason, evicted.Status.Message = v1.PodFailed, "Evicted", "The node was low on resource: memory."
	rejected := running()
	rejected.Status.Phase, rejected.Status.Reason, rejected.Status.Message = v1.PodFailed, "OutOfcpu", "Pod was rejected: Node didn't have enough resource: cpu"
	rejected.Status.Conditions = nil

	for _, tc := range []struct {
		name string
		p    *v1.Pod
		objs []runtime.Object
		want []string // kind, container, detail substring
	}{
		{"pull", pull, []runtime.Object{warning(pull, "app", "Failed", `Failed to pull image "acme/api:9.9": not found`)},
			[]string{pod.Pull, "app", "acme/api:9.9"}},
		{"crash, events expired", running(crashed, ok("sidecar")), nil, []string{pod.Crash, "app", "exit 1, 14 restarts"}},
		{"oom", running(oomed, ok("sidecar")), nil, []string{pod.OOM, "app", "killed at limit 512Mi, 14 restarts"}},
		{"config", running(waiting("app", "CreateContainerConfigError", `secret "db" not found`), ok("sidecar")), nil,
			[]string{pod.Config, "app", `secret "db" not found`}},
		{"probe", probe, []runtime.Object{warning(probe, "app", "Unhealthy", "Readiness probe failed: connection refused")},
			[]string{pod.Probe, "app", "Readiness probe failed"}},
		{"mount", mount, []runtime.Object{warning(mount, "", "FailedMount", `configmap "cfg" not found`)},
			[]string{pod.Mount, "", `configmap "cfg" not found`}},
		{"evicted", evicted, nil, []string{pod.Evicted, "", "low on resource: memory"}},
		{"rejected by kubelet", rejected, nil, []string{pod.Other, "", "didn't have enough resource: cpu"}},
		{"recovered, earlier oom", running(recovered, ok("sidecar")), nil, []string{pod.OOM, "app", "killed at limit 512Mi, 2 restarts"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ps := diagnose(t, tc.p, tc.objs...)
			if len(ps) != 1 {
				t.Fatalf("problems = %+v, want 1", ps)
			}
			got := ps[0]
			if got.Kind != tc.want[0] || got.Container != tc.want[1] || !strings.Contains(got.Detail, tc.want[2]) {
				t.Errorf("got %+v, want %v", got, tc.want)
			}
		})
	}
}

// Init containers come first, a fine sidecar is no problem, and a healthy pod has none.
func TestDiagnoseOrderAndHealthy(t *testing.T) {
	p := running(waiting("app", "PodInitializing", ""), ok("sidecar"))
	initFail := waiting("migrate", "CrashLoopBackOff", "")
	initFail.LastTerminationState.Terminated = &v1.ContainerStateTerminated{Reason: "Error", ExitCode: 2}
	p.Spec.InitContainers = []v1.Container{{Name: "migrate"}}
	p.Status.InitContainerStatuses = []v1.ContainerStatus{initFail}

	ps := diagnose(t, p)
	if len(ps) != 1 || ps[0].Container != "migrate" || !ps[0].Init || !strings.HasPrefix(ps[0].Detail, "exit 2") {
		t.Errorf("init: %+v", ps)
	}

	if ps := diagnose(t, running(ok("app"), ok("sidecar"))); len(ps) != 0 {
		t.Errorf("healthy: %+v", ps)
	}

	recovered := ok("app")
	recovered.LastTerminationState.Terminated = &v1.ContainerStateTerminated{Reason: "OOMKilled"}
	if ps := diagnose(t, running(recovered, ok("sidecar"))); len(ps) != 1 || !ps[0].Earlier {
		t.Errorf("earlier oom: %+v, want one problem marked earlier", ps)
	}

	if ps := diagnose(t, running(waiting("app", "ImagePullBackOff", ""), waiting("sidecar", "ImagePullBackOff", ""))); !slices.EqualFunc(ps,
		[]string{"app", "sidecar"}, func(p pod.Problem, n string) bool { return p.Container == n }) {
		t.Errorf("two failing containers: %+v", ps)
	}
}

func TestAddLogs(t *testing.T) {
	p := running(ok("app"), ok("sidecar"))
	ps := []pod.Problem{{Container: "app", Kind: pod.Crash}, {Container: "app", Kind: pod.Pull}, {Container: "sidecar", Kind: pod.OOM, Earlier: true}}

	pod.AddLogs(t.Context(), fake.NewClientset(p), p, ps, 20)

	// The fake client answers every log request with "fake logs".
	if !slices.Equal(ps[0].Logs, []string{"fake logs"}) || ps[1].Logs != nil || ps[2].Logs != nil {
		t.Errorf("logs: crash %v, pull %v, earlier oom %v; want only the crash", ps[0].Logs, ps[1].Logs, ps[2].Logs)
	}
}

func TestDiagnoseNode(t *testing.T) {
	n := node("n1", "4", "8Gi", nil)
	n.Status.Conditions = []v1.NodeCondition{
		{Type: v1.NodeReady, Status: v1.ConditionUnknown, Reason: "NodeStatusUnknown", Message: "Kubelet stopped posting node status."},
		{Type: v1.NodeDiskPressure, Status: v1.ConditionTrue, Message: "disk full"},
		{Type: v1.NodeMemoryPressure, Status: v1.ConditionFalse},
	}
	p := running(ok("app"), ok("sidecar"))

	ps := diagnose(t, p, n)
	if len(ps) != 1 || ps[0].Kind != pod.NodeBad || ps[0].Reason != "NodeStatusUnknown" ||
		ps[0].Detail != "node n1: Ready=Unknown (Kubelet stopped posting node status.); DiskPressure=True (disk full)" {
		t.Errorf("problems = %+v", ps)
	}

	// A node kwhy can't read (RBAC, gone) is no problem.
	if ps := diagnose(t, p); len(ps) != 0 {
		t.Errorf("no node: %+v", ps)
	}
}

func TestDiagnoseTerminating(t *testing.T) {
	p := running(ok("app"), ok("sidecar"))
	p.DeletionTimestamp = &metav1.Time{Time: time.Now().Add(-10 * time.Minute)}
	p.DeletionGracePeriodSeconds = new(int64(30))
	p.Finalizers = []string{"example.com/backup"}

	ps := diagnose(t, p)
	if len(ps) != 1 || ps[0].Kind != pod.Stuck || ps[0].Detail != "deleting for 10m; finalizers: example.com/backup" {
		t.Errorf("finalizer: %+v", ps)
	}

	p.Finalizers = nil
	if ps := diagnose(t, p); len(ps) != 1 || !strings.Contains(ps[0].Detail, "kubelet on n1 hasn't confirmed") {
		t.Errorf("no finalizer: %+v", ps)
	}

	// Within a minute of the grace period's end, deleting is normal.
	p.DeletionTimestamp = &metav1.Time{Time: time.Now().Add(-10 * time.Second)}
	if pod.Terminating(p) {
		t.Error("10s past the grace period counted as stuck")
	}
}

func TestAutoscaler(t *testing.T) {
	p := newPod("train", "", "2", "16Gi")
	normal := func(reason, msg string, ago time.Duration) *v1.Event {
		return &v1.Event{Name: reason, Namespace: p.Namespace, InvolvedObject: v1.ObjectReference{UID: p.UID},
			Type: v1.EventTypeNormal, Reason: reason, Message: msg, LastTimestamp: metav1.NewTime(time.Now().Add(-ago))}
	}

	ps, err := pod.Autoscaler(t.Context(), fake.NewClientset(
		normal("NotTriggerScaleUp", "pod didn't trigger scale-up: 2 max node group size reached", 5*time.Minute),
		normal("TriggeredScaleUp", "pod triggered scale-up: [{gpu-pool 2->3 (max: 5)}]", time.Minute),
		normal("Scheduled", "unrelated", 0),
	), p)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].Reason != "TriggeredScaleUp" || !strings.Contains(ps[0].Detail, "gpu-pool 2->3") {
		t.Errorf("problems = %+v, want the newest autoscaler event", ps)
	}

	if ps, _ := pod.Autoscaler(t.Context(), fake.NewClientset(), p); ps != nil {
		t.Errorf("no autoscaler: %+v", ps)
	}
}
