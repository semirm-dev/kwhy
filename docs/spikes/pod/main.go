// Spike: run the real kube-scheduler filter plugins in-process, every filter on every node.
package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/apis/config/latest"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	schedmetrics "k8s.io/kubernetes/pkg/scheduler/metrics"
)

func node(name string, cpu, mem string, labels map[string]string, taints ...v1.Taint) *v1.Node {
	rl := v1.ResourceList{v1.ResourceCPU: resource.MustParse(cpu), v1.ResourceMemory: resource.MustParse(mem), v1.ResourcePods: resource.MustParse("110")}
	return &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels}, Spec: v1.NodeSpec{Taints: taints},
		Status: v1.NodeStatus{Capacity: rl, Allocatable: rl, Conditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}}}}
}

func pod(name, nodeName, cpu, mem string) *v1.Pod {
	return &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ml", UID: types.UID("uid-" + name)}, Spec: v1.PodSpec{NodeName: nodeName,
		Containers: []v1.Container{{Name: "c", Resources: v1.ResourceRequirements{Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse(cpu), v1.ResourceMemory: resource.MustParse(mem)}}}}}}
}

var gpuTaint = v1.Taint{Key: "nvidia.com/gpu", Effect: v1.TaintEffectNoSchedule}

func newFramework(ctx context.Context, snap *cache.Snapshot, filter string) framework.Framework {
	cfg, err := latest.Default()
	must(err)
	prof := cfg.Profiles[0]
	// shortcut: DRA needs a SharedDRAManager; pods without resource claims don't need the plugin.
	var mp []config.Plugin
	for _, p := range prof.Plugins.MultiPoint.Enabled {
		if p.Name != "DynamicResources" {
			mp = append(mp, p)
		}
	}
	prof.Plugins.MultiPoint.Enabled = mp
	if filter != "" {
		prof.Plugins.Filter = config.PluginSet{Enabled: []config.Plugin{{Name: filter}}, Disabled: []config.Plugin{{Name: "*"}}}
	}
	client := fake.NewClientset()
	f, err := frameworkruntime.NewFramework(ctx, plugins.NewInTreeRegistry(), &prof,
		frameworkruntime.WithSnapshotSharedLister(snap),
		frameworkruntime.WithClientSet(client),
		frameworkruntime.WithInformerFactory(informers.NewSharedInformerFactory(client, 0)))
	must(err)
	return f
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// evaluate returns node -> failing reasons (all filters), plus the real scheduler's first-failure view.
func evaluate(ctx context.Context, snap *cache.Snapshot, p *v1.Pod) (map[string][]string, map[string]string) {
	full := newFramework(ctx, snap, "")
	var filters []string
	for _, pl := range full.ListPlugins().Filter.Enabled {
		filters = append(filters, pl.Name)
	}
	nodes, err := snap.NodeInfos().List()
	must(err)

	first := map[string]string{}
	st := framework.NewCycleState()
	if _, s, _ := full.RunPreFilterPlugins(ctx, st, p); !s.IsSuccess() {
		first["*"] = s.Message()
	}
	for _, n := range nodes {
		if s := full.RunFilterPlugins(ctx, st, p, n); !s.IsSuccess() {
			first[n.Node().Name] = s.Plugin() + ": " + strings.Join(s.Reasons(), "; ")
		}
	}

	all := map[string][]string{}
	for _, name := range filters {
		f := newFramework(ctx, snap, name)
		st := framework.NewCycleState()
		_, s, _ := f.RunPreFilterPlugins(ctx, st, p)
		for _, n := range nodes {
			if !s.IsSuccess() && !s.IsSkip() { // prefilter rejected the pod outright for this plugin
				all[n.Node().Name] = append(all[n.Node().Name], name+": "+s.Message())
				continue
			}
			if fs := f.RunFilterPlugins(ctx, st, p, n); !fs.IsSuccess() {
				all[n.Node().Name] = append(all[n.Node().Name], name+": "+strings.Join(fs.Reasons(), "; "))
			}
		}
	}
	return all, first
}

func report(title string, all map[string][]string, first map[string]string, nodes []string) {
	fmt.Println("###", title)
	for _, n := range nodes {
		if len(all[n]) == 0 {
			fmt.Printf("  %-8s FITS\n", n)
			continue
		}
		fmt.Printf("  %-8s all:   %s\n", n, strings.Join(all[n], " | "))
		fmt.Printf("  %-8s first: %s\n", "", first[n])
	}
}

func main() {
	schedmetrics.Register()
	ctx := context.Background()
	nodes := []*v1.Node{
		node("gpu-a1", "8", "64Gi", map[string]string{"gpu": "true", "topology.kubernetes.io/zone": "a"}, gpuTaint),
		node("gpu-a2", "8", "64Gi", map[string]string{"gpu": "true", "topology.kubernetes.io/zone": "b"}, gpuTaint),
		node("cpu-1", "4", "16Gi", map[string]string{"topology.kubernetes.io/zone": "a"}),
		node("infra-1", "4", "16Gi", map[string]string{"gpu": "true", "topology.kubernetes.io/zone": "a"}, v1.Taint{Key: "node-role.kubernetes.io/control-plane", Effect: v1.TaintEffectNoSchedule}),
	}
	running := []*v1.Pod{pod("busy-a1", "gpu-a1", "7", "8Gi"), pod("busy-a2", "gpu-a2", "1", "60Gi")}

	train := pod("train", "", "2", "16Gi")
	train.Spec.NodeSelector = map[string]string{"gpu": "true"}
	train.Spec.Tolerations = []v1.Toleration{{Key: "nvidia.com/gpu", Operator: v1.TolerationOpExists, Effect: v1.TaintEffectNoSchedule}}
	train.Spec.Affinity = &v1.Affinity{NodeAffinity: &v1.NodeAffinity{RequiredDuringSchedulingIgnoredDuringExecution: &v1.NodeSelector{
		NodeSelectorTerms: []v1.NodeSelectorTerm{{MatchExpressions: []v1.NodeSelectorRequirement{{Key: "topology.kubernetes.io/zone", Operator: v1.NodeSelectorOpIn, Values: []string{"a"}}}}}}}}

	names := []string{"gpu-a1", "gpu-a2", "cpu-1", "infra-1"}
	snap := cache.NewSnapshot(running, nodes)
	all, first := evaluate(ctx, snap, train)
	report("pod ml/train as submitted", all, first, names)

	// what-if: smallest change that makes some node fit
	whatIf := train.DeepCopy()
	whatIf.Spec.Containers[0].Resources.Requests[v1.ResourceCPU] = resource.MustParse("1")
	all, first = evaluate(ctx, cache.NewSnapshot(running, nodes), whatIf)
	report("what-if: cpu request 2 -> 1", all, first, names)
	keys := make([]string, 0)
	for k := range first {
		keys = append(keys, k)
	}
	sort.Strings(keys)
}
