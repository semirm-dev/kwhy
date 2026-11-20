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

// IsTemplate says whether p was built by Target from a workload's template rather than read from the cluster.
func IsTemplate(p *v1.Pod) bool {
	return p.UID == templateUID
}

// Healthy is returned by Target for a workload whose pods are all running and ready.
type Healthy struct{ Pods int }

func (h Healthy) Error() string {
	if h.Pods == 1 {
		return "its pod is ready"
	}

	return fmt.Sprintf("all %d pods ready", h.Pods)
}

// Target finds the pod to explain. ref is a pod (x, pod/x) or a workload (deploy/x, sts/x, ds/x, job/x). For a workload
// it is the oldest pod not yet on a node, else the not-ready pod with the most restarts, else a pod stuck
// terminating, else Healthy; a workload
// with no pods at all gets a new pod built from its template.
func Target(ctx context.Context, c kubernetes.Interface, ns, ref string) (*v1.Pod, error) {
	kind, name, ok := strings.Cut(ref, "/")
	if !ok {
		kind, name = "pod", ref
	}

	var (
		sel  *metav1.LabelSelector
		tmpl v1.PodTemplateSpec
	)
	if kind == "pod" || kind == "pods" || kind == "po" {
		p, err := c.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get pod: %w", err)
		}
		return p, nil
	}

	owner := workloadKind(kind)
	switch owner {
	case "Deployment":
		d, err := c.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get deployment: %w", err)
		}
		sel, tmpl = d.Spec.Selector, d.Spec.Template
	case "StatefulSet":
		s, err := c.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get statefulset: %w", err)
		}
		sel, tmpl = s.Spec.Selector, s.Spec.Template
	case "DaemonSet":
		d, err := c.AppsV1().DaemonSets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get daemonset: %w", err)
		}
		sel, tmpl = d.Spec.Selector, d.Spec.Template
	case "Job":
		j, err := c.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return nil, fmt.Errorf("get job: %w", err)
		}
		sel, tmpl = j.Spec.Selector, j.Spec.Template
	default:
		return nil, fmt.Errorf("unsupported kind %q: want pod, deploy, sts, ds or job", kind)
	}

	pods, err := c.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: metav1.FormatLabelSelector(sel)})
	if err != nil {
		return nil, fmt.Errorf("list pods of %s: %w", ref, err)
	}

	var live, pending, broken, stuck []v1.Pod
	for _, p := range pods.Items {
		switch {
		case Terminating(&p):
			stuck = append(stuck, p)
		case p.DeletionTimestamp != nil || p.Status.Phase == v1.PodSucceeded:
		case p.Spec.NodeName == "":
			pending = append(pending, p)
		case !ready(&p):
			broken = append(broken, p)
		}
		if p.DeletionTimestamp == nil {
			live = append(live, p)
		}
	}

	switch {
	case len(pending) > 0:
		p := slices.MinFunc(pending, func(a, b v1.Pod) int { return a.CreationTimestamp.Compare(b.CreationTimestamp.Time) })
		return &p, nil
	case len(broken) > 0:
		p := slices.MaxFunc(broken, func(a, b v1.Pod) int { return restarts(&a) - restarts(&b) })
		return &p, nil
	case len(stuck) > 0:
		return &stuck[0], nil
	case len(live) > 0:
		return nil, Healthy{Pods: len(live)}
	}

	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: templateUID, Labels: tmpl.Labels,
			OwnerReferences: []metav1.OwnerReference{{Kind: owner, Name: name, Controller: new(true)}}},
		Spec: tmpl.Spec,
	}, nil
}

// ready says whether p's Ready condition is true.
func ready(p *v1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == v1.PodReady {
			return c.Status == v1.ConditionTrue
		}
	}

	return false
}

// restarts sums the restart counts of p's containers.
func restarts(p *v1.Pod) int {
	n := 0
	for _, cs := range append(p.Status.InitContainerStatuses, p.Status.ContainerStatuses...) {
		n += int(cs.RestartCount)
	}

	return n
}

// Warnings says when the answer may differ from what the cluster's scheduler decides.
func Warnings(cl *Cluster, p *v1.Pod) []string {
	var ws []string
	if s := p.Spec.SchedulerName; s != "" && s != v1.DefaultSchedulerName && (cl.profile == nil || cl.profile.SchedulerName != s) {
		ws = append(ws, fmt.Sprintf("pod uses scheduler %q; kwhy runs the default scheduler's checks (give its --scheduler-config)", s))
	}

	if compiled := schedulerVersion(); compiled != nil {
		if server, err := version.ParseGeneric(cl.Version); err == nil && server.Major() == compiled.Major() {
			if d := int(server.Minor()) - int(compiled.Minor()); d > 1 || d < -1 {
				ws = append(ws, fmt.Sprintf("cluster is %s, kwhy runs the v%s scheduler; checks may differ", cl.Version, compiled))
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
