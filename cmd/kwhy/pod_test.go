package main

import (
	"testing"

	"helm.sh/helm/v4/pkg/cli"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/semirm-dev/kwhy/pod"
)

// The mini chart with values-prod.yaml asks for 2Gi; the only node has 1Gi, so NodeResourcesFit fails and the
// source is values-prod.yaml:4.
func TestPodSources(t *testing.T) {
	mem := corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("1Gi"), corev1.ResourceCPU: resource.MustParse("4"),
		corev1.ResourcePods: resource.MustParse("110")}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n1"}, Status: corev1.NodeStatus{Capacity: mem, Allocatable: mem}}
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "rel-api", Namespace: "default"}, Spec: appsv1.DeploymentSpec{
		Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "api"}},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "api"}},
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "api", Resources: corev1.ResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("2Gi")}}}}}}}}
	c := fake.NewClientset(node, deploy)

	p, err := pod.Target(t.Context(), c, "default", "deploy/rel-api")
	if err != nil {
		t.Fatal(err)
	}
	cl, err := pod.Snapshot(t.Context(), c)
	if err != nil {
		t.Fatal(err)
	}
	r, err := pod.Evaluate(t.Context(), cl, p)
	if err != nil {
		t.Fatal(err)
	}

	src := podChart{chart: mini, release: "rel", files: []string{prod}}
	got, err := podSources(t.Context(), c, p, r, src, "v1.37.0", cli.New())
	if err != nil {
		t.Fatal(err)
	}

	want := pod.Source{Plugin: "NodeResourcesFit", Field: ".spec.containers[0].resources.requests.memory", Value: "2Gi",
		From: "resources.requests.memory (" + prod + ":4)"}
	if len(got) != 1 || got[0] != want {
		t.Errorf("sources = %+v\nwant [%+v]", got, want)
	}

	// Without --release, the instance label is required.
	if _, err := podSources(t.Context(), c, p, r, podChart{chart: mini}, "v1.37.0", cli.New()); err == nil {
		t.Error("no release: want an error")
	}
}
