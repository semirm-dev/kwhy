package pod_test

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/semirm-dev/kwhy/pod"
)

func TestReads(t *testing.T) {
	cpu := pod.Check{Plugin: "NodeResourcesFit", Reasons: []string{"Insufficient cpu"}}

	for _, tc := range []struct {
		c    pod.Check
		path string
		want bool
	}{
		{cpu, ".spec.containers[0].resources.requests.cpu", true},
		{cpu, ".spec.initContainers[1].resources.requests.cpu", true},
		{cpu, ".spec.containers[0].resources.requests.memory", false}, // not short of memory
		{cpu, ".spec.containers[0].resources.limits.cpu", true},       // defaults the request when none is set
		{pod.Check{Plugin: "TaintToleration"}, ".spec.tolerations[0].key", true},
		{pod.Check{Plugin: "NodeAffinity"}, `.spec.nodeSelector["kwhy/pool"]`, true},
		{pod.Check{Plugin: "NodeAffinity"}, ".spec.affinity.nodeAffinity.requiredDuringSchedulingIgnoredDuringExecution.nodeSelectorTerms[0].matchExpressions[0].values[0]", true},
		{pod.Check{Plugin: "NodeAffinity"}, ".spec.affinity.podAntiAffinity.x", false},
		{pod.Check{Plugin: "NodePorts"}, ".spec.containers[0].ports[0].hostPort", true},
		{pod.Check{Plugin: "NodePorts"}, ".spec.containers[0].ports[0].containerPort", false},
		{pod.Check{Plugin: "VolumeZone"}, ".spec.volumes[0].persistentVolumeClaim.claimName", true},
		{pod.Check{Plugin: "TaintToleration"}, ".spec.tolerationsExtra", false},
	} {
		if got := pod.Reads(tc.c, tc.path); got != tc.want {
			t.Errorf("Reads(%s, %s) = %v, want %v", tc.c.Plugin, tc.path, got, tc.want)
		}
	}
}

func TestOwner(t *testing.T) {
	controller := func(kind, name string) []metav1.OwnerReference {
		return []metav1.OwnerReference{{Kind: kind, Name: name, Controller: new(true)}}
	}

	rs := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "api-7d9f", Namespace: "ml", OwnerReferences: controller("Deployment", "api")}}
	c := fake.NewClientset(rs)

	fromRS := newPod("api-7d9f-x", "", "1", "1Gi")
	fromRS.OwnerReferences = controller("ReplicaSet", "api-7d9f")
	fromSTS := newPod("db-0", "", "1", "1Gi")
	fromSTS.OwnerReferences = controller("StatefulSet", "db")
	bare := newPod("debug", "", "1", "1Gi")

	for _, tc := range []struct {
		p          *v1.Pod
		kind, name string
	}{
		{fromRS, "Deployment", "api"},
		{fromSTS, "StatefulSet", "db"},
		{bare, "Pod", "debug"},
	} {
		kind, name, err := pod.Owner(t.Context(), c, tc.p)
		if err != nil || kind != tc.kind || name != tc.name {
			t.Errorf("%s: %s/%s, %v; want %s/%s", tc.p.Name, kind, name, err, tc.kind, tc.name)
		}
	}

	// A template pod from Target names its workload.
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "idle", Namespace: "ml"},
		Spec: appsv1.DeploymentSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "idle"}}}}
	c = fake.NewClientset(d)
	p, err := pod.Target(t.Context(), c, "ml", "deploy/idle")
	if err != nil {
		t.Fatal(err)
	}
	if kind, name, err := pod.Owner(t.Context(), c, p); err != nil || kind != "Deployment" || name != "idle" {
		t.Errorf("template: %s/%s, %v", kind, name, err)
	}
}

func TestReadsProblem(t *testing.T) {
	for _, tc := range []struct {
		pr   pod.Problem
		path string
		i    int
		want bool
	}{
		{pod.Problem{Kind: pod.OOM, Container: "api"}, ".spec.containers[1].resources.limits.memory", 1, true},
		{pod.Problem{Kind: pod.OOM, Container: "api"}, ".spec.containers[0].resources.limits.memory", 1, false}, // another container
		{pod.Problem{Kind: pod.OOM, Container: "api"}, ".spec.containers[1].resources.requests.memory", 1, false},
		{pod.Problem{Kind: pod.Pull, Container: "api"}, ".spec.containers[0].image", 0, true},
		{pod.Problem{Kind: pod.Crash, Container: "api"}, ".spec.containers[0].args[0]", 0, true},
		{pod.Problem{Kind: pod.Config, Container: "api"}, ".spec.containers[0].env[2].valueFrom.secretKeyRef.name", 0, true},
		{pod.Problem{Kind: pod.Probe, Container: "api"}, ".spec.containers[0].readinessProbe.httpGet.path", 0, true},
		{pod.Problem{Kind: pod.Crash, Container: "migrate", Init: true}, ".spec.initContainers[0].command[0]", 0, true},
		{pod.Problem{Kind: pod.Crash, Container: "migrate", Init: true}, ".spec.containers[0].command[0]", 0, false},
		{pod.Problem{Kind: pod.Mount}, ".spec.volumes[0].configMap.name", -1, true},
		{pod.Problem{Kind: pod.Evicted}, ".spec.containers[0].resources.requests.memory", -1, true},
		{pod.Problem{Kind: pod.Sandbox}, ".spec.containers[0].image", 0, false},
		{pod.Problem{Kind: pod.Pull, Container: "gone"}, ".spec.containers[0].image", -1, false},
	} {
		if got := pod.ReadsProblem(tc.pr, tc.path, tc.i); got != tc.want {
			t.Errorf("ReadsProblem(%s %s, %s, %d) = %v, want %v", tc.pr.Kind, tc.pr.Container, tc.path, tc.i, got, tc.want)
		}
	}
}
