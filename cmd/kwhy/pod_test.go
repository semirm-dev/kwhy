package main

import (
	"testing"

	"helm.sh/helm/v4/pkg/cli"
	"helm.sh/helm/v4/pkg/cli/values"
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

	src := podChart{chart: mini, release: "rel", values: values.Options{ValueFiles: []string{prod}}}
	got, err := podSources(t.Context(), c, p, r, src, "v1.37.0", cli.New())
	if err != nil {
		t.Fatal(err)
	}

	want := pod.Source{Plugin: "NodeResourcesFit", Field: ".spec.containers[0].resources.requests.memory", Value: "2Gi",
		From: "resources.requests.memory (" + prod + ":4)"}
	if len(got) != 1 || got[0] != want {
		t.Errorf("sources = %+v\nwant [%+v]", got, want)
	}

	// Only a limit: Kubernetes defaults the request to it, so the limit is the source.
	limitOnly := podChart{chart: mini, release: "rel", values: values.Options{Values: []string{"resources.requests=null", "resources.limits.memory=2Gi"}}}
	got, err = podSources(t.Context(), c, p, r, limitOnly, "v1.37.0", cli.New())
	if err != nil {
		t.Fatal(err)
	}
	want = pod.Source{Plugin: "NodeResourcesFit", Field: ".spec.containers[0].resources.limits.memory", Value: "2Gi",
		From: "resources.limits.memory (--set resources.limits.memory=2Gi)"}
	if len(got) != 1 || got[0] != want {
		t.Errorf("limit only: sources = %+v\nwant [%+v]", got, want)
	}

	// Request and limit: only the request is the source.
	both := podChart{chart: mini, release: "rel", values: values.Options{ValueFiles: []string{prod}, Values: []string{"resources.limits.memory=4Gi"}}}
	got, err = podSources(t.Context(), c, p, r, both, "v1.37.0", cli.New())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Field != ".spec.containers[0].resources.requests.memory" {
		t.Errorf("request and limit: sources = %+v, want only the request", got)
	}

	// Without --release, the instance label is required.
	if _, err := podSources(t.Context(), c, p, r, podChart{chart: mini}, "v1.37.0", cli.New()); err == nil {
		t.Error("no release: want an error")
	}
}

// An OOM kill names the limit, even when the container also sets a request.
func TestPodSourcesProblem(t *testing.T) {
	deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "rel-api", Namespace: "default"}}
	c := fake.NewClientset(deploy)

	p := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "rel-api-1", Namespace: "default",
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "rel-api", Controller: new(true)}}}}
	r := pod.Result{Problems: []pod.Problem{{Container: "api", Kind: pod.OOM}}}

	src := podChart{chart: mini, release: "rel", values: values.Options{ValueFiles: []string{prod}, Values: []string{"resources.limits.memory=4Gi"}}}
	got, err := podSources(t.Context(), c, p, r, src, "v1.37.0", cli.New())
	if err != nil {
		t.Fatal(err)
	}

	want := pod.Source{Problem: "oom (api)", Field: ".spec.containers[0].resources.limits.memory", Value: "4Gi",
		From: "resources.limits.memory (--set resources.limits.memory=4Gi)"}
	if len(got) != 1 || got[0] != want {
		t.Errorf("sources = %+v\nwant [%+v]", got, want)
	}
}
