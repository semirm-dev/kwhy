package pod

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	resourcehelper "k8s.io/component-helpers/resource"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/apis/config/latest"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/nodevolumelimits"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	schedmetrics "k8s.io/kubernetes/pkg/scheduler/metrics"
)

const fitPlugin = "NodeResourcesFit"

var (
	// ErrDRA is returned for pods with resource claims: the DynamicResources plugin needs a DRA manager kwhy doesn't build.
	ErrDRA = errors.New("pod uses resource claims (DRA), which kwhy pod doesn't support yet")

	registerMetrics sync.Once
)

// Check is one failing scheduler check on one node.
type Check struct {
	Plugin  string   `json:"plugin"`
	Reasons []string `json:"reasons"`
	// Detail holds requested/allocatable numbers for NodeResourcesFit, e.g. "cpu 7.5/8 +2".
	Detail string `json:"detail,omitempty"`
}

// Node is one node's result.
type Node struct {
	Name string `json:"name"`
	// Failed is every failing check; empty means the pod fits.
	Failed []Check `json:"failed"`
	// First is what the real scheduler reports for this node: its first failing check.
	First *Check `json:"first,omitempty"`
}

// Result is every filter plugin run on every node.
type Result struct {
	// Plugins are the filter plugins of the default profile, in the order the scheduler runs them.
	Plugins []string `json:"plugins"`
	// Nodes are sorted by name.
	Nodes []Node `json:"nodes"`
	// Preempt are the nodes the pod would fit on if every lower-priority pod were preempted, when it fits none
	// now and may preempt. The scheduler also weighs PodDisruptionBudgets and picks the fewest victims.
	Preempt []string `json:"preempt,omitempty"`
	// Problems are why a pod on a node isn't running or ready (Diagnose); empty for an unscheduled pod.
	Problems []Problem `json:"problems,omitempty"`
	// Sources say where the fields failing checks read are set; filled by the caller (kwhy value), may be empty.
	Sources []Source `json:"sources,omitempty"`
}

// Evaluate runs the default scheduler profile's filters for p on every node of cl. The scheduler stops at a
// node's first failing filter; Evaluate runs each filter plugin in a framework of its own, so a node shows
// every failing check.
func Evaluate(ctx context.Context, cl *Cluster, p *v1.Pod) (Result, error) {
	if len(p.Spec.ResourceClaims) > 0 {
		return Result{}, ErrDRA
	}

	registerMetrics.Do(schedmetrics.Register) // NewFramework panics without it
	snap := cache.NewSnapshot(cl.pods, cl.nodes)
	inf := informers.NewSharedInformerFactory(cl.client, 0)

	full, err := newFramework(ctx, snap, inf, cl.client, cl.profile, "", false)
	if err != nil {
		return Result{}, err
	}

	var r Result
	for _, pl := range full.ListPlugins().Filter.Enabled {
		r.Plugins = append(r.Plugins, pl.Name)
	}

	pre := sets.New[string]()
	for _, pl := range full.ListPlugins().PreFilter.Enabled {
		pre.Insert(pl.Name)
	}

	each := make([]framework.Framework, len(r.Plugins))
	for i, name := range r.Plugins {
		if each[i], err = newFramework(ctx, snap, inf, cl.client, cl.profile, name, pre.Has(name)); err != nil {
			return Result{}, err
		}
	}

	// Plugins register their listers while being built, so start the informers after.
	inf.Start(ctx.Done())
	inf.WaitForCacheSync(ctx.Done())

	infos, err := snap.NodeInfos().List()
	if err != nil {
		return Result{}, fmt.Errorf("list nodes: %w", err)
	}

	first, err := run(ctx, full, p, infos)
	if err != nil {
		return Result{}, err
	}

	byNode := map[string]*Node{}
	for _, ni := range infos {
		name := ni.Node().Name
		n := &Node{Name: name, Failed: []Check{}}
		if s := first[name]; s != nil {
			n.First = &Check{Plugin: s.Plugin(), Reasons: s.Reasons()}
		}
		byNode[name] = n
	}

	for i, fw := range each {
		failed, err := run(ctx, fw, p, infos)
		if err != nil {
			return Result{}, err
		}

		for _, ni := range infos {
			s := failed[ni.Node().Name]
			if s == nil {
				continue
			}

			c := Check{Plugin: r.Plugins[i], Reasons: s.Reasons()}
			if c.Plugin == fitPlugin {
				c.Detail = fitDetail(p, ni, c.Reasons)
			}
			n := byNode[ni.Node().Name]
			n.Failed = append(n.Failed, c)
		}
	}

	fit := false
	for _, n := range byNode {
		r.Nodes = append(r.Nodes, *n)
		fit = fit || len(n.Failed) == 0
	}
	slices.SortFunc(r.Nodes, func(a, b Node) int { return strings.Compare(a.Name, b.Name) })

	if !fit && (p.Spec.PreemptionPolicy == nil || *p.Spec.PreemptionPolicy != v1.PreemptNever) {
		keep := slices.DeleteFunc(slices.Clone(cl.pods), func(q *v1.Pod) bool { return priority(q) < priority(p) })
		if len(keep) < len(cl.pods) {
			if r.Preempt, err = fitting(ctx, cl, keep, p); err != nil {
				return Result{}, err
			}
		}
	}

	return r, nil
}

// fitting are the nodes p fits on, as the scheduler decides, when only pods remain on the nodes.
func fitting(ctx context.Context, cl *Cluster, pods []*v1.Pod, p *v1.Pod) ([]string, error) {
	snap := cache.NewSnapshot(pods, cl.nodes)
	inf := informers.NewSharedInformerFactory(cl.client, 0)

	fw, err := newFramework(ctx, snap, inf, cl.client, cl.profile, "", false)
	if err != nil {
		return nil, err
	}

	inf.Start(ctx.Done())
	inf.WaitForCacheSync(ctx.Done())

	infos, err := snap.NodeInfos().List()
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	failed, err := run(ctx, fw, p, infos)
	if err != nil {
		return nil, err
	}

	var names []string
	for _, ni := range infos {
		if failed[ni.Node().Name] == nil {
			names = append(names, ni.Node().Name)
		}
	}
	slices.Sort(names)

	return names, nil
}

func priority(p *v1.Pod) int32 {
	if p.Spec.Priority == nil {
		return 0
	}

	return *p.Spec.Priority
}

// newFramework builds the default profile without DynamicResources. With filter set, the framework runs only
// that filter plugin, and only its own PreFilter if it has one, so a rejection is never blamed on another plugin.
func newFramework(ctx context.Context, snap *cache.Snapshot, inf informers.SharedInformerFactory, c kubernetes.Interface,
	base *config.KubeSchedulerProfile, filter string, preFilter bool) (framework.Framework, error) {
	if base == nil {
		cfg, err := latest.Default()
		if err != nil {
			return nil, fmt.Errorf("default scheduler config: %w", err)
		}
		base = &cfg.Profiles[0]
	}

	prof := *base
	// MultiPoint.Disabled doesn't remove a plugin MultiPoint enables, so drop it from the list.
	prof.Plugins.MultiPoint.Enabled = slices.DeleteFunc(slices.Clone(prof.Plugins.MultiPoint.Enabled),
		func(p config.Plugin) bool { return p.Name == "DynamicResources" })

	if filter != "" {
		only := config.PluginSet{Enabled: []config.Plugin{{Name: filter}}, Disabled: []config.Plugin{{Name: "*"}}}
		prof.Plugins.Filter = only
		prof.Plugins.PreFilter = config.PluginSet{Disabled: []config.Plugin{{Name: "*"}}}
		if preFilter {
			prof.Plugins.PreFilter = only
		}
	}

	f, err := frameworkruntime.NewFramework(ctx, plugins.NewInTreeRegistry(), &prof,
		frameworkruntime.WithSnapshotSharedLister(snap),
		frameworkruntime.WithClientSet(c),
		frameworkruntime.WithInformerFactory(inf),
		frameworkruntime.WithSharedCSIManager(nodevolumelimits.NewCSIManager(inf.Storage().V1().CSINodes().Lister())))
	if err != nil {
		return nil, fmt.Errorf("scheduler framework %s: %w", filter, err)
	}

	return f, nil
}

// run does what the scheduler's findNodesThatFitPod does: PreFilter, then Filter on each node PreFilter
// left. It returns the failing status per node name.
func run(ctx context.Context, fw framework.Framework, p *v1.Pod, infos []fwk.NodeInfo) (map[string]*fwk.Status, error) {
	st := framework.NewCycleState()
	res, s, unscheduled := fw.RunPreFilterPlugins(ctx, st, p)
	if !s.IsSuccess() && !s.IsRejected() {
		return nil, s.AsError()
	}

	failed := map[string]*fwk.Status{}
	for _, ni := range infos {
		name := ni.Node().Name
		switch {
		case !s.IsSuccess():
			failed[name] = s
		case !res.AllNodes() && !res.NodeNames.Has(name):
			// Same status and message as the scheduler gives nodes a PreFilter result leaves out.
			failed[name] = fwk.NewStatus(fwk.UnschedulableAndUnresolvable,
				fmt.Sprintf("node(s) didn't satisfy plugin(s) %v", sets.List(unscheduled))).WithPlugin(strings.Join(sets.List(unscheduled), ","))
		default:
			fs := fw.RunFilterPlugins(ctx, st, p, ni)
			if !fs.IsSuccess() && !fs.IsRejected() {
				return nil, fs.AsError()
			}
			if !fs.IsSuccess() {
				failed[name] = fs
			}
		}
	}

	return failed, nil
}

// fitDetail gives requested/allocatable and the pod's request for the cpu and memory reasons of
// NodeResourcesFit, e.g. "cpu 7.5/8 +2".
func fitDetail(p *v1.Pod, ni fwk.NodeInfo, reasons []string) string {
	want := resourcehelper.PodRequests(p, resourcehelper.PodResourcesOptions{})
	used, alloc := ni.GetRequested(), ni.GetAllocatable()

	var parts []string
	for _, r := range reasons {
		switch r {
		case "Insufficient cpu":
			parts = append(parts, fmt.Sprintf("cpu %s/%s +%s", cores(used.GetMilliCPU()), cores(alloc.GetMilliCPU()), cores(want.Cpu().MilliValue())))
		case "Insufficient memory":
			parts = append(parts, fmt.Sprintf("memory %s/%s +%s", bytes(used.GetMemory()), bytes(alloc.GetMemory()), bytes(want.Memory().Value())))
		}
	}

	return strings.Join(parts, ", ")
}

func cores(milli int64) string {
	return strconv.FormatFloat(float64(milli)/1000, 'f', -1, 64)
}

// bytes prints whole or one-decimal Gi, or Mi below 1Gi.
func bytes(b int64) string {
	if b < 1<<30 {
		return strconv.FormatInt(b>>20, 10) + "Mi"
	}

	return strconv.FormatFloat(math.Round(float64(b)/(1<<30)*10)/10, 'f', -1, 64) + "Gi"
}
