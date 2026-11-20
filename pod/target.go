package pod

import (
	"context"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/version"
	"k8s.io/client-go/kubernetes"
)

// templateUID marks a pod Target built from a workload's template.
const templateUID = "kwhy-template"

// Target finds the pod to explain. ref is a pod (x, pod/x) or a workload (deploy/x, sts/x, job/x); for a workload
// it is the oldest pod not yet on a node, or, when every pod has one, a new pod built from the workload's template.
func Target(ctx context.Context, c kubernetes.Interface, ns, ref string) (*v1.Pod, error) {
	kind, name, ok := strings.Cut(ref, "/")
	if !ok {
		kind, name = "pod", ref
	}

	var (
		sel  *metav1.LabelSelector
		tmpl v1.PodTemplateSpec
	)
	switch kind {
	case "pod", "pods", "po":
		p, err := c.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get pod: %w", err)
		}
		return p, nil
	case "deploy", "deployment", "deployments":
		d, err := c.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get deployment: %w", err)
		}
		sel, tmpl = d.Spec.Selector, d.Spec.Template
	case "sts", "statefulset", "statefulsets":
		s, err := c.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get statefulset: %w", err)
		}
		sel, tmpl = s.Spec.Selector, s.Spec.Template
	case "job", "jobs":
		j, err := c.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get job: %w", err)
		}
		sel, tmpl = j.Spec.Selector, j.Spec.Template
	default:
		return nil, fmt.Errorf("unsupported kind %q: want pod, deploy, sts or job", kind)
	}

	pods, err := c.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: metav1.FormatLabelSelector(sel)})
	if err != nil {
		return nil, fmt.Errorf("list pods of %s: %w", ref, err)
	}

	var pending []v1.Pod
	for _, p := range pods.Items {
		if p.Spec.NodeName == "" && p.DeletionTimestamp == nil {
			pending = append(pending, p)
		}
	}
	if len(pending) > 0 {
		p := slices.MinFunc(pending, func(a, b v1.Pod) int { return a.CreationTimestamp.Compare(b.CreationTimestamp.Time) })
		return &p, nil
	}

	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: templateUID, Labels: tmpl.Labels},
		Spec:       tmpl.Spec,
	}, nil
}

// Warnings says when the answer may differ from what the cluster's scheduler decides.
func Warnings(cl *Cluster, p *v1.Pod) []string {
	var ws []string
	if s := p.Spec.SchedulerName; s != "" && s != v1.DefaultSchedulerName {
		ws = append(ws, fmt.Sprintf("pod uses scheduler %q; kwhy runs the default scheduler's checks", s))
	}

	if compiled := schedulerVersion(); compiled != nil {
		if server, err := version.ParseGeneric(cl.Version); err == nil && server.Major() == compiled.Major() {
			if d := int(server.Minor()) - int(compiled.Minor()); d > 1 || d < -1 {
				ws = append(ws, fmt.Sprintf("cluster is %s, kwhy runs the %s scheduler; checks may differ", cl.Version, compiled))
			}
		}
	}

	return ws
}

// schedulerVersion is the k8s.io/kubernetes version compiled in, nil if the build info doesn't say.
func schedulerVersion() *version.Version {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return nil
	}

	for _, d := range bi.Deps {
		if d.Path == "k8s.io/kubernetes" {
			v, err := version.ParseGeneric(d.Version)
			if err != nil {
				return nil
			}
			return v
		}
	}

	return nil
}
