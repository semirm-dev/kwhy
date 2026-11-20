package change_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/semirm-dev/kwhy/change"
)

func TestLive(t *testing.T) {
	meta := func(name string) metav1.ObjectMeta { return metav1.ObjectMeta{Name: name, Namespace: "default"} }
	pvc := func(name, sc string, phase corev1.PersistentVolumeClaimPhase) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{ObjectMeta: meta(name), Status: corev1.PersistentVolumeClaimStatus{Phase: phase},
			Spec: corev1.PersistentVolumeClaimSpec{StorageClassName: &sc, Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")}}}}
	}

	c := fake.NewClientset(
		&appsv1.Deployment{ObjectMeta: meta("api"), Spec: appsv1.DeploymentSpec{Replicas: new(int32(5))}},
		&autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: meta("api"),
			Spec:   autoscalingv2.HorizontalPodAutoscalerSpec{ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "Deployment", Name: "api"}, MinReplicas: new(int32(2)), MaxReplicas: 10},
			Status: autoscalingv2.HorizontalPodAutoscalerStatus{CurrentReplicas: 7}},
		&appsv1.DaemonSet{ObjectMeta: meta("agent"), Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 4}},
		pvc("fixed", "standard", corev1.ClaimBound),
		pvc("grows", "expandable", corev1.ClaimBound),
		pvc("pending", "standard", corev1.ClaimPending),
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "standard"}},
		&storagev1.StorageClass{ObjectMeta: metav1.ObjectMeta{Name: "expandable"}, AllowVolumeExpansion: new(true)},
	)

	const (
		deploy = `
kind: Deployment
metadata: {name: api}
spec:
  replicas: 2
  template: {spec: {containers: [{name: c, image: app:%s, readinessProbe: {initialDelaySeconds: 5}}]}}
`
		ds    = "kind: DaemonSet\nmetadata: {name: agent}\nspec: {template: {spec: {containers: [{name: c, image: a:%s}]}}}\n"
		sts   = "kind: StatefulSet\nmetadata: {name: db}\nspec: {replicas: 3, template: {spec: {containers: [{name: c, image: pg:%s}]}}}\n"
		claim = "kind: PersistentVolumeClaim\nmetadata: {name: %s}\nspec: {resources: {requests: {storage: %s}}}\n"
	)
	side := func(v, size string) string {
		parts := []string{fmt.Sprintf(deploy, v), fmt.Sprintf(ds, v), fmt.Sprintf(sts, v)}
		for _, n := range []string{"fixed", "grows", "pending"} {
			parts = append(parts, fmt.Sprintf(claim, n, size))
		}
		return strings.Join(parts, "---")
	}

	is := change.Diff(docs(t, side("1", "1Gi")), docs(t, side("2", "2Gi")))
	if err := change.Live(t.Context(), c, is); err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, i := range is {
		got = append(got, fmt.Sprintf("%s|%s|%d|%s|%d", i.Verdict, i.Resource, i.Pods, i.Detail, i.Seconds))
	}

	want := []string{
		"restart|DaemonSet default/agent|4|4 nodes, ~40s|40",
		// 7 pods (HPA), batches of 2 surge + 1 unavailable: 3 batches of 10s start + 5s delay + 10s period.
		"restart|Deployment default/api|7|image, HPA 2–10, ~2m|75",
		// Not in the cluster: keeps the manifest's 3, one at a time.
		"ordered|StatefulSet default/db|3|one at a time, ~30s|30",
		"resize|PersistentVolumeClaim default/grows|0|1Gi → 2Gi|0",
		"will fail|PersistentVolumeClaim default/fixed|0|StorageClass standard doesn't allow expansion|0",
		"will fail|PersistentVolumeClaim default/pending|0|claim isn't bound, its spec is immutable|0",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
