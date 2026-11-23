package pod

import (
	"context"
	"fmt"
	"slices"
	"strings"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// reads are the pod spec fields each filter plugin checks, as path prefixes. NodeResourcesFit and NodePorts
// are matched in Reads, since their fields sit inside container lists.
var reads = map[string][]string{
	"NodeName":           {".spec.nodeName"},
	"TaintToleration":    {".spec.tolerations"},
	"NodeAffinity":       {".spec.nodeSelector", ".spec.affinity.nodeAffinity"},
	"InterPodAffinity":   {".spec.affinity.podAffinity", ".spec.affinity.podAntiAffinity"},
	"PodTopologySpread":  {".spec.topologySpreadConstraints"},
	"VolumeBinding":      {".spec.volumes"},
	"VolumeZone":         {".spec.volumes"},
	"VolumeRestrictions": {".spec.volumes"},
	"NodeVolumeLimits":   {".spec.volumes"},
}

// Source is where a field a failing check reads is set, found by kwhy value.
type Source struct {
	// Plugin names the failing check (unscheduled pod); Problem the problem kind and container (pod on a node).
	Plugin  string `json:"plugin,omitempty"`
	Problem string `json:"problem,omitempty"`
	// Field is the pod spec path, e.g. .spec.containers[0].resources.requests.cpu.
	Field string `json:"field"`
	Value any    `json:"value"`
	// From names the values leaves and where they are set, e.g. "resources.requests.cpu (values-prod.yaml:31)".
	From string `json:"from"`
}

func (s Source) label() string {
	return or(s.Plugin, s.Problem)
}

// Reads says whether check c reads the pod spec field at path (".spec.containers[0].resources.requests.cpu").
func Reads(c Check, path string) bool {
	containers := strings.HasPrefix(path, ".spec.containers[") || strings.HasPrefix(path, ".spec.initContainers[")

	switch c.Plugin {
	case fitPlugin:
		// Limits count too: Kubernetes defaults a missing request to the limit. Callers drop a limit when its
		// container also sets the request.
		for _, r := range c.Reasons {
			res, ok := strings.CutPrefix(r, "Insufficient ")
			if ok && containers && (strings.HasSuffix(path, ".resources.requests."+res) || strings.HasSuffix(path, ".resources.limits."+res)) {
				return true
			}
		}
		return false
	case "NodePorts":
		return containers && strings.HasSuffix(path, ".hostPort")
	}

	return slices.ContainsFunc(reads[c.Plugin], func(prefix string) bool { return under(path, prefix) })
}

// problemReads are the container fields each problem kind involves.
var problemReads = map[string][]string{
	Pull:   {"image"},
	Crash:  {"command", "args", "image"},
	OOM:    {"resources.limits.memory"},
	Config: {"env", "envFrom"},
	Probe:  {"readinessProbe", "livenessProbe", "startupProbe"},
}

// ReadsProblem says whether problem pr involves the pod spec field at path. i is the position of pr's container
// in the spec's containers (or initContainers) list, -1 if unknown.
func ReadsProblem(pr Problem, path string, i int) bool {
	switch pr.Kind {
	case Mount:
		return under(path, ".spec.volumes")
	case Evicted:
		return (strings.HasPrefix(path, ".spec.containers[") || strings.HasPrefix(path, ".spec.initContainers[")) &&
			strings.Contains(path, ".resources.")
	}

	if i < 0 {
		return false
	}

	list := "containers"
	if pr.Init {
		list = "initContainers"
	}
	prefix := fmt.Sprintf(".spec.%s[%d].", list, i)

	return slices.ContainsFunc(problemReads[pr.Kind], func(f string) bool { return under(path, prefix+f) })
}

// under says whether path is prefix or a field inside it.
func under(path, prefix string) bool {
	return path == prefix || strings.HasPrefix(path, prefix+".") || strings.HasPrefix(path, prefix+"[")
}

// Owner is the workload that creates p, as kind and name ("Deployment", "api"); a ReplicaSet is followed to its
// Deployment and a Job to its CronJob. A pod nothing owns is its own owner.
func Owner(ctx context.Context, c kubernetes.Interface, p *v1.Pod) (kind, name string, err error) {
	ref := metav1.GetControllerOf(p)
	if ref == nil {
		return "Pod", p.Name, nil
	}

	if ref.Kind == "Job" {
		j, err := c.BatchV1().Jobs(p.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return "", "", fmt.Errorf("get job: %w", err)
		}
		if cj := metav1.GetControllerOf(j); cj != nil && cj.Kind == "CronJob" {
			return cj.Kind, cj.Name, nil
		}
	}
	if ref.Kind != "ReplicaSet" {
		return ref.Kind, ref.Name, nil
	}

	rs, err := c.AppsV1().ReplicaSets(p.Namespace).Get(ctx, ref.Name, metav1.GetOptions{})
	if err != nil {
		return "", "", fmt.Errorf("get replicaset: %w", err)
	}

	if d := metav1.GetControllerOf(rs); d != nil {
		return d.Kind, d.Name, nil
	}

	return "ReplicaSet", rs.Name, nil
}
