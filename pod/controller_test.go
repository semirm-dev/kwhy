package pod_test

import (
	"bytes"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/semirm-dev/kwhy/pod"
)

func TestController(t *testing.T) {
	sel := &metav1.LabelSelector{MatchLabels: map[string]string{"app": "web"}}
	d := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: "ml", UID: "d1"}, Spec: appsv1.DeploymentSpec{Selector: sel},
		Status: appsv1.DeploymentStatus{Conditions: []appsv1.DeploymentCondition{{Type: appsv1.DeploymentProgressing, Status: v1.ConditionFalse,
			Reason: "ProgressDeadlineExceeded", Message: `ReplicaSet "web-2" has timed out progressing.`}}}}
	rsFailed := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "web-2", Namespace: "ml", Labels: sel.MatchLabels,
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web", UID: "d1", Controller: new(true)}}},
		Status: appsv1.ReplicaSetStatus{Conditions: []appsv1.ReplicaSetCondition{{Type: appsv1.ReplicaSetReplicaFailure, Status: v1.ConditionTrue,
			Reason: "FailedCreate", Message: `pods "web-2-x" is forbidden: exceeded quota: compute, requested: cpu=2, limited: cpu=1`}}}}
	rsEvents := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "web-3", Namespace: "ml", UID: "rs3", Labels: sel.MatchLabels,
		OwnerReferences: []metav1.OwnerReference{{Kind: "Deployment", Name: "web", UID: "d1", Controller: new(true)}}}}
	other := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "ml", Labels: sel.MatchLabels},
		Status: appsv1.ReplicaSetStatus{Conditions: []appsv1.ReplicaSetCondition{{Type: appsv1.ReplicaSetReplicaFailure, Status: v1.ConditionTrue}}}}
	ev := func(uid, msg string) *v1.Event {
		return &v1.Event{ObjectMeta: metav1.ObjectMeta{Name: uid + ".ev", Namespace: "ml"}, InvolvedObject: v1.ObjectReference{UID: types.UID(uid)},
			Type: v1.EventTypeWarning, Reason: "FailedCreate", Message: msg}
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "migrate", Namespace: "ml", UID: "j1"},
		Status: batchv1.JobStatus{Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: v1.ConditionTrue,
			Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit"}}}}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "ml", UID: "s1"}}

	c := fake.NewClientset(d, rsFailed, rsEvents, other, job, sts,
		ev("rs3", `pods "web-3-y" is forbidden: violates PodSecurity "restricted:latest"`),
		ev("s1", `create Pod db-0 in StatefulSet db failed error: pods "db-0" is forbidden: exceeded quota`))

	for _, tc := range []struct {
		ref  string
		want []string // kind: detail substring
	}{
		{"deploy/web", []string{"rollout: has timed out progressing", "create: replicaset web-2: pods \"web-2-x\" is forbidden: exceeded quota",
			"create: replicaset web-3: pods \"web-3-y\" is forbidden: violates PodSecurity"}},
		{"job/migrate", []string{"job: backoff limit"}},
		{"sts/db", []string{"create: statefulset db: create Pod db-0"}},
		{"web-1", nil},
	} {
		ps, err := pod.Controller(t.Context(), c, "ml", tc.ref)
		if err != nil {
			t.Fatalf("%s: %v", tc.ref, err)
		}
		if len(ps) != len(tc.want) {
			t.Fatalf("%s: %+v, want %d problems", tc.ref, ps, len(tc.want))
		}
		for i, w := range tc.want {
			kind, detail, _ := strings.Cut(w, ": ")
			if ps[i].Kind != kind || !strings.Contains(ps[i].Detail, detail) {
				t.Errorf("%s[%d] = %s %q, want %s ~%q", tc.ref, i, ps[i].Kind, ps[i].Detail, kind, detail)
			}
		}
	}
}

func TestWriteController(t *testing.T) {
	w := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "deploy/web", Namespace: "ml"}}
	r := pod.Result{Problems: []pod.Problem{{Kind: pod.Create, Reason: "FailedCreate", Detail: "replicaset web-2: exceeded quota"}}}

	var b bytes.Buffer
	if err := pod.WriteHuman(&b, w, r); err != nil {
		t.Fatal(err)
	}

	want := `ml/deploy/web · no pod to check · 1 problem from its controller

CONTAINER     PROBLEM  DETAIL
(controller)  create   replicaset web-2: exceeded quota
`
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}
}
