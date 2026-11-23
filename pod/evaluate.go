package pod

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	resourcehelper "k8s.io/component-helpers/resource"
	corev1helpers "k8s.io/component-helpers/scheduling/corev1"
	resourceslicetracker "k8s.io/dynamic-resource-allocation/resourceslice/tracker"
	"k8s.io/klog/v2"
	fwk "k8s.io/kube-scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/apis/config/latest"
	"k8s.io/kubernetes/pkg/scheduler/backend/cache"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultpreemption"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/dynamicresources"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/feature"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/nodevolumelimits"
	"k8s.io/kubernetes/pkg/scheduler/framework/preemption"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	schedmetrics "k8s.io/kubernetes/pkg/scheduler/metrics"
	"k8s.io/kubernetes/pkg/scheduler/util/assumecache"
)

const fitPlugin = "NodeResourcesFit"

var (
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
	// Preempt are the nodes where the scheduler's preemption (DefaultPreemption) would make room, when the pod fits
	// none now and may preempt.
	Preempt []string `json:"preempt,omitempty"`
	// PreemptNode is the one of Preempt the scheduler picks; empty when several tie by its rules and it picks one
	// of those at random.
	PreemptNode string `json:"preemptNode,omitempty"`
	// Victims are the pods (namespace/name) it evicts on PreemptNode.
	Victims []string `json:"victims,omitempty"`
	// PDBViolations is how many of Victims' PodDisruptionBudgets the eviction breaks. The scheduler breaks them
	// only when no node in Preempt avoids it.
	PDBViolations int64 `json:"pdbViolations,omitempty"`
	// PreemptSampled is set when the scheduler checks only a random sample of these nodes, so it may pick another.
	PreemptSampled bool `json:"preemptSampled,omitempty"`
	// Problems are why a pod on a node isn't running or ready (Diagnose); empty for an unscheduled pod.
	Problems []Problem `json:"problems,omitempty"`
	// Sources say where the fields failing checks read are set; filled by the caller (kwhy value), may be empty.
	Sources []Source `json:"sources,omitempty"`
}

// Evaluate runs the default scheduler profile's filters for p on every node of cl. The scheduler stops at a
// node's first failing filter; Evaluate runs each filter plugin in a framework of its own, so a node shows
// every failing check.
func Evaluate(ctx context.Context, cl *Cluster, p *v1.Pod) (Result, error) {
	registerMetrics.Do(schedmetrics.Register) // NewFramework panics without it
	snap := cache.NewSnapshot(cl.pods, cl.nodes)
	inf := informers.NewSharedInformerFactory(cl.client, 0)
	dra, err := draManager(ctx, cl, inf)
	if err != nil {
		return Result{}, err
	}

	nom := nominator{}
	for _, q := range cl.nominated {
		pi, err := framework.NewPodInfo(q)
		if err != nil {
			return Result{}, err
		}
		nom[q.Status.NominatedNodeName] = append(nom[q.Status.NominatedNodeName], pi)
	}

	full, err := newFramework(ctx, snap, inf, cl.client, dra, nom, cl.profile, "", false)
	if err != nil {
		return Result{}, err
	}

	var dp *defaultpreemption.DefaultPreemption
	if slices.ContainsFunc(full.ListPlugins().PostFilter.Enabled, func(pl config.Plugin) bool { return pl.Name == defaultpreemption.Name }) {
		if dp, err = newPreemption(ctx, full, cl.profile); err != nil {
			return Result{}, err
		}
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
		if each[i], err = newFramework(ctx, snap, inf, cl.client, dra, nom, cl.profile, name, pre.Has(name)); err != nil {
			return Result{}, err
		}
	}

	// Plugins register their listers while being built, so start the informers after.
	inf.Start(ctx.Done())
	for typ, ok := range inf.WaitForCacheSync(ctx.Done()) {
		if !ok {
			return Result{}, fmt.Errorf("sync %v cache: %w", typ, context.Cause(ctx))
		}
	}

	infos, err := snap.NodeInfos().List()
	if err != nil {
		return Result{}, fmt.Errorf("list nodes: %w", err)
	}

	first, st, err := run(ctx, full, p, infos)
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
		failed, _, err := run(ctx, fw, p, infos)
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

	if !fit && dp != nil {
		if err := simulatePreemption(ctx, dp, st, p, infos, first, &r); err != nil {
			return Result{}, err
		}
	}

	return r, nil
}

// simulatePreemption runs the scheduler's preemption as a dry run, as Evaluator.evaluate does, without evicting anything.
func simulatePreemption(ctx context.Context, dp *defaultpreemption.DefaultPreemption, st fwk.CycleState, p *v1.Pod,
	infos []fwk.NodeInfo, first map[string]*fwk.Status, r *Result) error {
	if ok, _ := dp.PodEligibleToPreemptOthers(ctx, p, first[p.Status.NominatedNodeName]); !ok {
		return nil
	}

	var potential []fwk.NodeInfo
	for _, ni := range infos {
		if first[ni.Node().Name].Code() == fwk.Unschedulable {
			potential = append(potential, ni)
		}
	}
	if len(potential) == 0 {
		return nil
	}

	pdbs, err := dp.Evaluator.PdbLister.List(labels.Everything())
	if err != nil {
		return fmt.Errorf("list poddisruptionbudgets: %w", err)
	}

	// Every potential node, not the scheduler's random sample, so the result doesn't change from run to run.
	cands, _, err := dp.Evaluator.DryRunPreemption(ctx, st, p, potential, pdbs, 0, int32(len(potential)))
	if err != nil && len(cands) == 0 {
		return fmt.Errorf("preemption: %w", err)
	}
	if len(cands) == 0 {
		return nil
	}

	for _, c := range cands {
		r.Preempt = append(r.Preempt, c.Name())
	}
	slices.Sort(r.Preempt)
	_, sample := dp.GetOffsetAndNumCandidates(int32(len(potential)))
	r.PreemptSampled = int(sample) < len(potential)

	best := dp.Evaluator.SelectCandidate(ctx, cands)
	if slices.ContainsFunc(cands, func(c preemption.Candidate) bool { return c.Name() != best.Name() && rank(c) == rank(best) }) {
		return nil
	}
	r.PreemptNode, r.PDBViolations = best.Name(), best.Victims().NumPDBViolations
	for _, v := range best.Victims().Pods {
		r.Victims = append(r.Victims, v.Namespace+"/"+v.Name)
	}

	return nil
}

// rank is what the scheduler orders candidates by (pickOneNodeForPreemption): PDB violations, highest victim
// priority, sum of priorities, number of victims, earliest start. Candidates of equal rank tie.
func rank(c preemption.Candidate) [5]int64 {
	v := c.Victims()
	// A pod without a start time counts as starting now, after every pod that has one.
	k := [5]int64{v.NumPDBViolations, int64(corev1helpers.PodPriority(v.Pods[0])), 0, int64(len(v.Pods)), math.MaxInt64}
	for _, q := range v.Pods {
		k[2] += int64(corev1helpers.PodPriority(q))
		if int64(corev1helpers.PodPriority(q)) == k[1] && q.Status.StartTime != nil {
			k[4] = min(k[4], q.Status.StartTime.UnixNano())
		}
	}

	return k
}

// newPreemption builds the scheduler's DefaultPreemption plugin over fw, with the profile's arguments.
func newPreemption(ctx context.Context, fw framework.Framework, base *config.KubeSchedulerProfile) (*defaultpreemption.DefaultPreemption, error) {
	prof, err := profile(base)
	if err != nil {
		return nil, err
	}

	var args runtime.Object
	for _, pc := range prof.PluginConfig {
		if pc.Name == defaultpreemption.Name {
			args = pc.Args
		}
	}

	dp, err := defaultpreemption.New(ctx, args, fw, feature.NewSchedulerFeaturesFromGates(utilfeature.DefaultFeatureGate))
	if err != nil {
		return nil, fmt.Errorf("scheduler preemption: %w", err)
	}

	return dp, nil
}

// nominator hands the scheduler the pods already nominated to each node; filters count those of equal or higher
// priority there.
type nominator map[string][]fwk.PodInfo

func (n nominator) AddNominatedPod(klog.Logger, fwk.PodInfo, *fwk.NominatingInfo) {}
func (n nominator) DeleteNominatedPodIfExists(*v1.Pod)                            {}
func (n nominator) UpdateNominatedPod(klog.Logger, *v1.Pod, fwk.PodInfo)          {}
func (n nominator) NominatedPodsForNode(node string) []fwk.PodInfo                { return n[node] }

// newFramework builds the default profile without DynamicResources. With filter set, the framework runs only
// that filter plugin, and only its own PreFilter if it has one, so a rejection is never blamed on another plugin.
func newFramework(ctx context.Context, snap *cache.Snapshot, inf informers.SharedInformerFactory, c kubernetes.Interface,
	dra fwk.SharedDRAManager, nom nominator, base *config.KubeSchedulerProfile, filter string, preFilter bool) (framework.Framework, error) {
	def, err := profile(base)
	if err != nil {
		return nil, err
	}

	prof := *def

	if filter != "" {
		// Frameworks share one informer factory, and DynamicResources indexes pods under the profile's name.
		prof.SchedulerName += "/" + filter
		only := config.PluginSet{Enabled: []config.Plugin{{Name: filter}}, Disabled: []config.Plugin{{Name: "*"}}}
		prof.Plugins.Filter = only
		prof.Plugins.PreFilter = config.PluginSet{Disabled: []config.Plugin{{Name: "*"}}}
		if preFilter {
			prof.Plugins.PreFilter = only
		}
	}

	f, err := frameworkruntime.NewFramework(ctx, plugins.NewInTreeRegistry(), &prof,
		frameworkruntime.WithSnapshotSharedLister(snap),
		frameworkruntime.WithMutableSnapshotLister(snap),
		frameworkruntime.WithPodNominator(nom),
		frameworkruntime.WithClientSet(c),
		frameworkruntime.WithInformerFactory(inf),
		frameworkruntime.WithSharedDRAManager(dra),
		frameworkruntime.WithSharedCSIManager(nodevolumelimits.NewCSIManager(inf.Storage().V1().CSINodes().Lister())))
	if err != nil {
		return nil, fmt.Errorf("scheduler framework %s: %w", filter, err)
	}

	return f, nil
}

// profile is base, or the default scheduler profile when base is nil.
func profile(base *config.KubeSchedulerProfile) (*config.KubeSchedulerProfile, error) {
	if base != nil {
		return base, nil
	}

	cfg, err := latest.Default()
	if err != nil {
		return nil, fmt.Errorf("default scheduler config: %w", err)
	}

	return &cfg.Profiles[0], nil
}

// draManager builds the scheduler's DRA manager over the claims, slices and device classes of the snapshot, as
// scheduler.go does, so the DynamicResources plugin can allocate devices for pods with resource claims.
func draManager(ctx context.Context, cl *Cluster, inf informers.SharedInformerFactory) (fwk.SharedDRAManager, error) {
	claims := assumecache.NewAssumeCache(klog.FromContext(ctx), inf.Resource().V1().ResourceClaims().Informer(), "ResourceClaim", "", nil)
	tracker, err := resourceslicetracker.StartTracker(ctx, resourceslicetracker.Options{
		SliceInformer: inf.Resource().V1().ResourceSlices(),
		KubeClient:    cl.client,
	})
	if err != nil {
		return nil, fmt.Errorf("resource slice tracker: %w", err)
	}

	return dynamicresources.NewDRAManager(ctx, claims, tracker, inf), nil
}

// run does what the scheduler's findNodesThatFitPod does: PreFilter, then Filter on each node PreFilter
// left, counting nominated pods. It returns the failing status per node name and the cycle state.
func run(ctx context.Context, fw framework.Framework, p *v1.Pod, infos []fwk.NodeInfo) (map[string]*fwk.Status, fwk.CycleState, error) {
	st := framework.NewCycleState()
	res, s, unscheduled := fw.RunPreFilterPlugins(ctx, st, p)
	if !s.IsSuccess() && !s.IsRejected() {
		return nil, nil, s.AsError()
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
			fs := fw.RunFilterPluginsWithNominatedPods(ctx, st, p, ni)
			if !fs.IsSuccess() && !fs.IsRejected() {
				return nil, nil, fs.AsError()
			}
			if !fs.IsSuccess() {
				failed[name] = fs
			}
		}
	}

	return failed, st, nil
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
