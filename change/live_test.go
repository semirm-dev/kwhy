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

// A Deployment's surge pods that don't fit the namespace's ResourceQuota are called out.
func TestLiveSurgeQuota(t *testing.T) {
	const deploy = `
kind: Deployment
metadata: {name: api}
spec:
  replicas: 4
  template: {spec: {containers: [{name: c, image: app:%s, resources: {requests: {cpu: 500m}}}]}}
`
	quota := func(used string) *corev1.ResourceQuota {
		return &corev1.ResourceQuota{ObjectMeta: metav1.ObjectMeta{Name: "compute", Namespace: "default"},
			Status: corev1.ResourceQuotaStatus{
				Hard: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse("2")},
				Used: corev1.ResourceList{corev1.ResourceRequestsCPU: resource.MustParse(used)}}}
	}

	for _, tc := range []struct {
		used, want string
	}{
		// 25% of 4 pods surge: one more pod of 500m, with 200m left.
		{"1800m", "image, ~20s, surge of 1 pod exceeds ResourceQuota compute (requests.cpu: 1800m of 2 used, needs +500m)"},
		{"1", "image, ~20s"},
	} {
		c := fake.NewClientset(quota(tc.used))
		is := change.Diff(docs(t, fmt.Sprintf(deploy, "1")), docs(t, fmt.Sprintf(deploy, "2")))
		if err := change.Live(t.Context(), c, is); err != nil {
			t.Fatal(err)
		}
		if got := is[0].Detail; got != tc.want {
			t.Errorf("used %s: detail %q, want %q", tc.used, is[0].Detail, tc.want)
		}
	}
}

// An image changed in the cluster but not in git is put back by the apply: a restart the diff doesn't show.
func TestDrift(t *testing.T) {
	const deploy = "kind: Deployment\nmetadata: {name: %s}\nspec: {replicas: 2, template: {spec: {containers: [{name: c, image: %s}]}}}\n"
	live := func(name, image string) *appsv1.Deployment {
		return &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: appsv1.DeploymentSpec{
			Replicas: new(int32(3)), Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "c", Image: image}}}}}}
	}
	c := fake.NewClientset(live("api", "app:1-hotfix"), live("web", "web:2"), live("same", "s:1"))

	base := docs(t, fmt.Sprintf(deploy, "api", "app:1")+"---"+fmt.Sprintf(deploy, "web", "web:1")+"---"+fmt.Sprintf(deploy, "same", "s:1"))
	head := docs(t, fmt.Sprintf(deploy, "api", "app:1")+"---"+fmt.Sprintf(deploy, "web", "web:3")+"---"+fmt.Sprintf(deploy, "same", "s:1"))
	is := change.Diff(base, head)

	drift, err := change.Drift(t.Context(), c, head, is)
	if err != nil {
		t.Fatal(err)
	}
	// web changes in git, so it's already a restart; same matches; only api drifted.
	if len(drift) != 1 || drift[0].Resource != "Deployment default/api" || drift[0].Pods != 3 ||
		drift[0].Detail != "live runs app:1-hotfix (changed outside git); applying puts back app:1" {
		t.Errorf("drift = %+v", drift)
	}
}
