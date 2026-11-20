package pod

import (
	"context"
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/pager"
)

// Cluster is what the scheduler sees, read once: nodes, the pods placed on them, and every object a filter
// plugin's listers read, served from memory so plugins never touch the real cluster.
type Cluster struct {
	// Version is the API server's version, e.g. v1.36.4.
	Version string

	client kubernetes.Interface
	nodes  []*v1.Node
	pods   []*v1.Pod
}

// Snapshot reads, with get and list only, every object the default filter plugins need.
func Snapshot(ctx context.Context, c kubernetes.Interface) (*Cluster, error) {
	v, err := c.Discovery().ServerVersion()
	if err != nil {
		return nil, fmt.Errorf("server version: %w", err)
	}

	lists := map[string]pager.ListPageFunc{
		"nodes": func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
			return c.CoreV1().Nodes().List(ctx, o)
		},
		"pods": func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
			return c.CoreV1().Pods("").List(ctx, o)
		},
		"namespaces": func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
			return c.CoreV1().Namespaces().List(ctx, o)
		},
		"persistentvolumes": func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
			return c.CoreV1().PersistentVolumes().List(ctx, o)
		},
		"persistentvolumeclaims": func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
			return c.CoreV1().PersistentVolumeClaims("").List(ctx, o)
		},
		"storageclasses": func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
			return c.StorageV1().StorageClasses().List(ctx, o)
		},
		"csinodes": func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
			return c.StorageV1().CSINodes().List(ctx, o)
		},
		"csidrivers": func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
			return c.StorageV1().CSIDrivers().List(ctx, o)
		},
		"volumeattachments": func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
			return c.StorageV1().VolumeAttachments().List(ctx, o)
		},
		"csistoragecapacities": func(ctx context.Context, o metav1.ListOptions) (runtime.Object, error) {
			return c.StorageV1().CSIStorageCapacities("").List(ctx, o)
		},
	}

	cl := &Cluster{Version: v.GitVersion}

	var objs []runtime.Object
	for kind, list := range lists {
		l, _, err := pager.New(list).List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", kind, err)
		}

		items, err := meta.ExtractList(l)
		if err != nil {
			return nil, fmt.Errorf("list %s: %w", kind, err)
		}

		objs = append(objs, items...)
	}

	for _, o := range objs {
		switch o := o.(type) {
		case *v1.Node:
			cl.nodes = append(cl.nodes, o)
		case *v1.Pod:
			// Only pods bound to a node and not finished hold resources there.
			if o.Spec.NodeName != "" && o.Status.Phase != v1.PodSucceeded && o.Status.Phase != v1.PodFailed {
				cl.pods = append(cl.pods, o)
			}
		}
	}
	cl.client = fake.NewClientset(objs...)

	return cl, nil
}
