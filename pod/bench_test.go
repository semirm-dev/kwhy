package pod_test

import (
	"fmt"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/semirm-dev/kwhy/pod"
)

// BenchmarkEvaluate is the spec's gate: 1,000 nodes and 30,000 pods in under 5s.
func BenchmarkEvaluate(b *testing.B) {
	var objs []runtime.Object
	for i := range 1000 {
		zone := fmt.Sprintf("zone-%d", i%3)
		var taints []v1.Taint
		if i%10 == 0 {
			taints = []v1.Taint{gpuTaint}
		}
		objs = append(objs, node(fmt.Sprintf("node-%04d", i), "16", "64Gi",
			map[string]string{"topology.kubernetes.io/zone": zone, "kubernetes.io/hostname": fmt.Sprintf("node-%04d", i)}, taints...))
	}
	for i := range 30000 {
		p := newPod(fmt.Sprintf("p-%05d", i), fmt.Sprintf("node-%04d", i%1000), "500m", "1Gi")
		p.Labels = map[string]string{"app": fmt.Sprintf("app-%d", i%50)}
		objs = append(objs, p)
	}

	target := newPod("train", "", "4", "8Gi")
	target.Labels = map[string]string{"app": "app-1"}
	target.Spec.Tolerations = []v1.Toleration{{Key: "nvidia.com/gpu", Operator: v1.TolerationOpExists}}
	target.Spec.Affinity = &v1.Affinity{PodAntiAffinity: &v1.PodAntiAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []v1.PodAffinityTerm{{
		TopologyKey: "kubernetes.io/hostname", LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "app-1"}}}}}}
	target.Spec.TopologySpreadConstraints = []v1.TopologySpreadConstraint{{MaxSkew: 1, TopologyKey: "topology.kubernetes.io/zone",
		WhenUnsatisfiable: v1.DoNotSchedule, LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "app-1"}}}}

	cl, err := pod.Snapshot(b.Context(), fake.NewClientset(objs...))
	if err != nil {
		b.Fatal(err)
	}

	for b.Loop() {
		if _, err := pod.Evaluate(b.Context(), cl, target); err != nil {
			b.Fatal(err)
		}
	}
}
