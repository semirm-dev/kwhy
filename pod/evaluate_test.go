package pod_test

import (
	"errors"
	"slices"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/semirm-dev/kwhy/pod"
)

var gpuTaint = v1.Taint{Key: "nvidia.com/gpu", Effect: v1.TaintEffectNoSchedule}

func node(name, cpu, mem string, labels map[string]string, taints ...v1.Taint) *v1.Node {
	rl := v1.ResourceList{v1.ResourceCPU: resource.MustParse(cpu), v1.ResourceMemory: resource.MustParse(mem), v1.ResourcePods: resource.MustParse("110")}

	return &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}, Spec: v1.NodeSpec{Taints: taints},
		Status: v1.NodeStatus{Capacity: rl, Allocatable: rl, Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}}}}
}

func newPod(name, nodeName, cpu, mem string) *v1.Pod {
	return &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ml", UID: types.UID("uid-" + name)}, Spec: v1.PodSpec{NodeName: nodeName,
		Containers: []v1.Container{{Name: "c", Resources: v1.ResourceRequirements{Requests: v1.ResourceList{
			v1.ResourceCPU: resource.MustParse(cpu), v1.ResourceMemory: resource.MustParse(mem)}}}}}}
}

func evaluate(t *testing.T, p *v1.Pod, objs ...runtime.Object) map[string]pod.Node {
	t.Helper()

	cl, err := pod.Snapshot(t.Context(), fake.NewClientset(objs...))
	if err != nil {
		t.Fatal(err)
	}

	r, err := pod.Evaluate(t.Context(), cl, p)
	if err != nil {
		t.Fatal(err)
	}

	nodes := map[string]pod.Node{}
	for _, n := range r.Nodes {
		nodes[n.Name] = n
	}

	return nodes
}

func plugins(cs []pod.Check) []string {
	var names []string
	for _, c := range cs {
		names = append(names, c.Plugin)
	}

	return names
}

// The spike's fixture: on gpu-a2 the scheduler names only NodeAffinity, but NodeResourcesFit fails too.
func TestEvaluateEveryCheck(t *testing.T) {
	train := newPod("train", "", "2", "16Gi")
	train.Spec.NodeSelector = map[string]string{"gpu": "true"}
	train.Spec.Tolerations = []v1.Toleration{{Key: "nvidia.com/gpu", Operator: v1.TolerationOpExists, Effect: v1.TaintEffectNoSchedule}}
	train.Spec.Affinity = &v1.Affinity{NodeAffinity: &v1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{
		NodeSelectorTerms: []v1.NodeSelectorTerm{{MatchExpressions: []v1.NodeSelectorRequirement{{
			Key: "topology.kubernetes.io/zone", Operator: v1.NodeSelectorOpIn, Values: []string{"a"}}}}}}}}

	nodes := evaluate(t, train,
		node("gpu-a1", "8", "64Gi", map[string]string{"gpu": "true", "topology.kubernetes.io/zone": "a"}, gpuTaint),
		node("gpu-a2", "8", "64Gi", map[string]string{"gpu": "true", "topology.kubernetes.io/zone": "b"}, gpuTaint),
		node("cpu-1", "4", "16Gi", map[string]string{"topology.kubernetes.io/zone": "a"}),
		node("infra-1", "4", "16Gi", map[string]string{"gpu": "true", "topology.kubernetes.io/zone": "a"},
			v1.Taint{Key: "node-role.kubernetes.io/control-plane", Effect: v1.TaintEffectNoSchedule}),
		newPod("busy-a1", "gpu-a1", "7", "8Gi"),
		newPod("busy-a2", "gpu-a2", "1", "60Gi"),
		train,
	)

	want := map[string][]string{
		"gpu-a1":  {"NodeResourcesFit"},
		"gpu-a2":  {"NodeAffinity", "NodeResourcesFit"},
		"cpu-1":   {"NodeAffinity"},
		"infra-1": {"TaintToleration"},
	}
	for name, w := range want {
		if got := plugins(nodes[name].Failed); !slices.Equal(got, w) {
			t.Errorf("%s failed %v, want %v", name, got, w)
		}
	}

	if f := nodes["gpu-a2"].First; f == nil || f.Plugin != "NodeAffinity" {
		t.Errorf("gpu-a2 first = %+v, want NodeAffinity", f)
	}

	// The pending pod itself holds nothing: gpu-a1 has 7 of 8 cpu used by busy-a1 only.
	if d := nodes["gpu-a1"].Failed[0].Detail; d != "cpu 7/8 +2" {
		t.Errorf("gpu-a1 detail = %q", d)
	}
	if d := nodes["gpu-a2"].Failed[1].Detail; d != "memory 60Gi/64Gi +16Gi" {
		t.Errorf("gpu-a2 detail = %q", d)
	}
}

// Plugins whose PreFilter skips the pod (no affinity, no spread) pass, and a plain pod fits a plain node.
func TestEvaluateFits(t *testing.T) {
	nodes := evaluate(t, newPod("web", "", "1", "1Gi"), node("n1", "4", "8Gi", nil))
	if n := nodes["n1"]; len(n.Failed) != 0 || n.First != nil {
		t.Errorf("n1 = %+v, want a fit", n)
	}
}

// A PreFilter rejection belongs to its own plugin only: a missing claim fails VolumeBinding, not TaintToleration.
func TestEvaluatePreFilterOwnPlugin(t *testing.T) {
	p := newPod("db", "", "1", "1Gi")
	p.Spec.Volumes = []v1.Volume{{Name: "data", VolumeSource: v1.VolumeSource{
		PersistentVolumeClaim: &v1.PersistentVolumeClaimVolumeSource{ClaimName: "missing"}}}}

	nodes := evaluate(t, p, node("n1", "4", "8Gi", nil))

	got := plugins(nodes["n1"].Failed)
	if !slices.Contains(got, "VolumeBinding") {
		t.Errorf("failed %v, want VolumeBinding among them", got)
	}
	for _, name := range []string{"NodeResourcesFit", "TaintToleration", "NodeAffinity", "NodeName", "NodePorts"} {
		if slices.Contains(got, name) {
			t.Errorf("failed %v: %s blamed for another plugin's PreFilter", got, name)
		}
	}
}

func TestEvaluateDRA(t *testing.T) {
	p := newPod("gpu", "", "1", "1Gi")
	p.Spec.ResourceClaims = []v1.PodResourceClaim{{Name: "gpu"}}

	cl, err := pod.Snapshot(t.Context(), fake.NewClientset())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := pod.Evaluate(t.Context(), cl, p); !errors.Is(err, pod.ErrDRA) {
		t.Errorf("err = %v, want ErrDRA", err)
	}
}
