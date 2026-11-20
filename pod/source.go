package pod

import (
	"context"
	"fmt"
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
	Plugin string `json:"plugin"`
	// Field is the pod spec path, e.g. .spec.containers[0].resources.requests.cpu.
	Field string `json:"field"`
	Value any    `json:"value"`
	// From names the values leaves and where they are set, e.g. "resources.requests.cpu (values-prod.yaml:31)".
	From string `json:"from"`
}

// Reads says whether check c reads the pod spec field at path (".spec.containers[0].resources.requests.cpu").
func Reads(c Check, path string) bool {
	containers := strings.HasPrefix(path, ".spec.containers[") || strings.HasPrefix(path, ".spec.initContainers[")

	switch c.Plugin {
	case fitPlugin:
		// shortcut: requests only; a request defaulted from limits isn't linked.
		for _, r := range c.Reasons {
			if res, ok := strings.CutPrefix(r, "Insufficient "); ok && containers && strings.HasSuffix(path, ".resources.requests."+res) {
				return true
			}
		}
		return false
	case "NodePorts":
		return containers && strings.HasSuffix(path, ".hostPort")
	}

	for _, prefix := range reads[c.Plugin] {
		if path == prefix || strings.HasPrefix(path, prefix+".") || strings.HasPrefix(path, prefix+"[") {
			return true
		}
	}

	return false
}

// Owner is the workload that creates p, as kind and name ("Deployment", "api"); a ReplicaSet is followed to its
// Deployment. A pod nothing owns is its own owner.
func Owner(ctx context.Context, c kubernetes.Interface, p *v1.Pod) (kind, name string, err error) {
	ref := metav1.GetControllerOf(p)
	if ref == nil {
		return "Pod", p.Name, nil
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
