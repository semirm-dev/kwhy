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
		&storagev1.StorageClass{Name: "standard"},
		&storagev1.StorageClass{Name: "expandable", AllowVolumeExpansion: new(true)},
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
		return &corev1.ResourceQuota{Name: "compute", Namespace: "default",
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
		return &appsv1.Deployment{Name: name, Namespace: "default", Spec: appsv1.DeploymentSpec{
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

// Rollout time follows the strategy: Recreate is one batch, maxSurge and maxUnavailable size the batches.
func TestLiveEstimate(t *testing.T) {
	const (
		deploy = "kind: Deployment\nmetadata: {name: api}\nspec: {replicas: 4, strategy: %s, template: {spec: {containers: [{name: c, image: app:%s}]}}}\n"
		ds     = "kind: DaemonSet\nmetadata: {name: agent}\nspec: {updateStrategy: {rollingUpdate: {maxUnavailable: 2}}, template: {spec: {containers: [{name: c, image: a:%s}]}}}\n"
		sts    = "kind: StatefulSet\nmetadata: {name: db}\nspec: {replicas: 3, updateStrategy: {rollingUpdate: {partition: 1}}, template: {spec: {containers: [{name: c, image: pg:%s}]}}}\n"
	)
	c := fake.NewClientset(
		&appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: "agent", Namespace: "default"}, Status: appsv1.DaemonSetStatus{DesiredNumberScheduled: 4}},
		// An HPA runs 5: the partition of 1 leaves ordinals 1 to 4 to update.
		&autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default"},
			Spec:   autoscalingv2.HorizontalPodAutoscalerSpec{ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "StatefulSet", Name: "db"}, MaxReplicas: 5},
			Status: autoscalingv2.HorizontalPodAutoscalerStatus{CurrentReplicas: 5}},
	)

	for _, tc := range []struct {
		name, base, head string
		pods, seconds    int
	}{
		// Each batch is 10s of pod start.
		{"recreate is one batch", fmt.Sprintf(deploy, "{type: Recreate}", "1"), fmt.Sprintf(deploy, "{type: Recreate}", "2"), 4, 10},
		{"one at a time", fmt.Sprintf(deploy, "{rollingUpdate: {maxSurge: 0, maxUnavailable: 1}}", "1"),
			fmt.Sprintf(deploy, "{rollingUpdate: {maxSurge: 0, maxUnavailable: 1}}", "2"), 4, 40},
		{"full surge", fmt.Sprintf(deploy, "{rollingUpdate: {maxSurge: '100%'}}", "1"), fmt.Sprintf(deploy, "{rollingUpdate: {maxSurge: '100%'}}", "2"), 4, 10},
		{"daemonset maxUnavailable", fmt.Sprintf(ds, "1"), fmt.Sprintf(ds, "2"), 4, 20},
		{"HPA replicas above the partition", fmt.Sprintf(sts, "16"), fmt.Sprintf(sts, "17"), 4, 40},
	} {
		t.Run(tc.name, func(t *testing.T) {
			is := change.Diff(docs(t, tc.base), docs(t, tc.head))
			if err := change.Live(t.Context(), c, is); err != nil {
				t.Fatal(err)
			}
			if len(is) != 1 || is[0].Pods != tc.pods || is[0].Seconds != tc.seconds {
				t.Errorf("got %+v, want %d pods in %ds", is, tc.pods, tc.seconds)
			}
		})
	}
}

// A partition at or above the manifest's replicas updates no pod, unless an HPA runs more of them or the manifest
// leaves the count to the cluster. With replicas set and no HPA, apply resets the count: still no pod.
func TestLivePartitionAboveManifestReplicas(t *testing.T) {
	const sts = "kind: StatefulSet\nmetadata: {name: %s}\nspec: {%supdateStrategy: {rollingUpdate: {partition: 3}}, template: {spec: {containers: [{name: c, image: pg:%s}]}}}\n"
	live := func(name string) *appsv1.StatefulSet {
		return &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: appsv1.StatefulSetSpec{Replicas: new(int32(5))}}
	}
	c := fake.NewClientset(live("scaled"), live("unset"), live("fixed"),
		&autoscalingv2.HorizontalPodAutoscaler{ObjectMeta: metav1.ObjectMeta{Name: "scaled", Namespace: "default"},
			Spec:   autoscalingv2.HorizontalPodAutoscalerSpec{ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: "StatefulSet", Name: "scaled"}, MaxReplicas: 5},
			Status: autoscalingv2.HorizontalPodAutoscalerStatus{CurrentReplicas: 5}})
	side := func(v string) string {
		return fmt.Sprintf(sts, "scaled", "replicas: 3, ", v) + "---" + fmt.Sprintf(sts, "unset", "", v) + "---" + fmt.Sprintf(sts, "fixed", "replicas: 3, ", v)
	}

	is := change.Diff(docs(t, side("16")), docs(t, side("17")))
	if err := change.Live(t.Context(), c, is); err != nil {
		t.Fatal(err)
	}

	var got []string
	for _, i := range is {
		got = append(got, fmt.Sprintf("%s|%s|%d|%s", i.Verdict, i.Resource, i.Pods, i.Detail))
	}
	want := []string{
		"ordered|StatefulSet default/scaled|2|one at a time, ordinals 3 and up, HPA 0–5, ~20s",
		"ordered|StatefulSet default/unset|2|one at a time, ordinals 3 and up, ~20s",
		"no effect|StatefulSet default/fixed|0|partition 3: no pod is updated",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
