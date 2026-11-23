package pod

import (
	"context"
	"fmt"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
)

// Controller problem kinds. The strings are part of the kwhy.pod/v1 JSON contract.
const (
	Create  = "create"  // the controller can't create pods (quota, PodSecurity, a webhook, ...)
	Rollout = "rollout" // a Deployment's rollout made no progress within its deadline
	JobFail = "job"     // the Job failed (BackoffLimitExceeded, DeadlineExceeded, ...)
)

// Controller says what the workload ref's controller reports: pods it can't create, a stuck rollout, a failed
// Job. A pod ref has none. These come from the controller's conditions and Warning events, in its own words.
func Controller(ctx context.Context, c kubernetes.Interface, ns, ref string) ([]Problem, error) {
	kind, name, ok := strings.Cut(ref, "/")
	if !ok {
		return nil, nil
	}

	var ps []Problem
	switch workloadKind(kind) {
	case "Deployment":
		d, err := c.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get deployment: %w", err)
		}
		for _, cond := range d.Status.Conditions {
			if cond.Type == appsv1.DeploymentProgressing && cond.Status == v1.ConditionFalse {
				ps = append(ps, Problem{Kind: Rollout, Reason: cond.Reason, Detail: cond.Message})
			}
		}

		rss, err := c.AppsV1().ReplicaSets(ns).List(ctx, metav1.ListOptions{LabelSelector: metav1.FormatLabelSelector(d.Spec.Selector)})
		if err != nil {
			return nil, fmt.Errorf("list replicasets: %w", err)
		}
		for _, rs := range rss.Items {
			if !metav1.IsControlledBy(&rs, d) {
				continue
			}
			found := false
			for _, cond := range rs.Status.Conditions {
				if cond.Type == appsv1.ReplicaSetReplicaFailure && cond.Status == v1.ConditionTrue {
					ps = append(ps, Problem{Kind: Create, Reason: cond.Reason, Detail: "replicaset " + rs.Name + ": " + cond.Message})
					found = true
				}
			}
			if !found {
				p, err := createEvents(ctx, c, ns, rs.UID, "replicaset "+rs.Name)
				if err != nil {
					return nil, err
				}
				ps = append(ps, p...)
			}
		}
	case "StatefulSet":
		s, err := c.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get statefulset: %w", err)
		}
		return createEvents(ctx, c, ns, s.UID, "statefulset "+name)
	case "DaemonSet":
		d, err := c.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get daemonset: %w", err)
		}
		return createEvents(ctx, c, ns, d.UID, "daemonset "+name)
	case "ReplicaSet":
		r, err := c.AppsV1().ReplicaSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get replicaset: %w", err)
		}
		for _, cond := range r.Status.Conditions {
			if cond.Type == appsv1.ReplicaSetReplicaFailure && cond.Status == v1.ConditionTrue {
				return []Problem{{Kind: Create, Reason: cond.Reason, Detail: "replicaset " + name + ": " + cond.Message}}, nil
			}
		}
		return createEvents(ctx, c, ns, r.UID, "replicaset "+name)
	case "CronJob":
		cj, j, err := lastJob(ctx, c, ns, name)
		if err != nil {
			return nil, err
		}
		if ps, err = createEvents(ctx, c, ns, cj.UID, "cronjob "+name); err != nil {
			return nil, err
		}
		if j != nil {
			p, err := Controller(ctx, c, ns, "job/"+j.Name)
			if err != nil {
				return nil, err
			}
			ps = append(ps, p...)
		}
	case "Job":
		j, err := c.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get job: %w", err)
		}
		for _, cond := range j.Status.Conditions {
			if cond.Type == batchv1.JobFailed && cond.Status == v1.ConditionTrue {
				ps = append(ps, Problem{Kind: JobFail, Reason: cond.Reason, Detail: cond.Message})
			}
		}
		p, err := createEvents(ctx, c, ns, j.UID, "job "+name)
		if err != nil {
			return nil, err
		}
		ps = append(ps, p...)
	}

	return ps, nil
}

// createEvents turns an object's FailedCreate Warning events into one create problem with the newest message.
func createEvents(ctx context.Context, c kubernetes.Interface, ns string, uid types.UID, what string) ([]Problem, error) {
	list, err := c.CoreV1().Events(ns).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.uid=" + string(uid)})
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}

	evs := about(list.Items, uid)
	msgs := messages(evs, "", "FailedCreate")
	if len(msgs) == 0 {
		return nil, nil
	}

	return []Problem{{Kind: Create, Reason: "FailedCreate", Detail: what + ": " + msgs[0], Events: msgs}}, nil
}

// workloadKind maps a ref's kind ("deploy", "sts", ...) to the API kind, "" for a pod or anything else.
func workloadKind(kind string) string {
	switch kind {
	case "deploy", "deployment", "deployments":
		return "Deployment"
	case "sts", "statefulset", "statefulsets":
		return "StatefulSet"
	case "ds", "daemonset", "daemonsets":
		return "DaemonSet"
	case "rs", "replicaset", "replicasets":
		return "ReplicaSet"
	case "job", "jobs":
		return "Job"
	case "cj", "cronjob", "cronjobs":
		return "CronJob"
	}

	return ""
}
