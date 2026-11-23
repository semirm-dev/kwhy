# Spec 03: `kwhy pod`

Explain why a pod is not scheduled: every failing check on every node, from the real scheduler code.
Read-only and diagnosis only: it says what's wrong and never suggests or tries changes.

## Problem

The `FailedScheduling` event summarises nodes into one line, and the scheduler stops at the **first**
failing filter per node. Fixing that reason often exposes the next one. Engineers end up reading node
allocations, taints and affinity terms by hand.

## Competition (checked 2026-11-18)

- **kubectl-why-pending** (SaiRohithGuntupally, Go, ★7, active Sep 2026): polished plain-English diagnosis
  with fixes, strong on on-prem causes (fragmentation, device plugins). **Reimplements** the predicates;
  it does not import the scheduler.
- **k8sgpt**: LLM explanations, not exact.
- **kube-scheduler-simulator**: reproduces scheduling, not a live diagnosis tool.

**Differentiation, in priority order:**
1. **Exact parity**: runs the real in-tree filter plugins, so answers match the scheduler, including new
   plugins and edge cases a reimplementation misses.
2. **The full node × check matrix**, not one diagnosis.
3. **Link to source**: if the pod comes from a Helm/Kustomize app in the current repo, point at the line
   that sets the problematic field (`kwhy value`).

This is the most crowded of the three features, so it ships last (v0.4) and leans on 3.

## Scope

**v0.4 (built 2026-11-20):**
```
kwhy pod <name|pod/name|deploy/name|sts/name|job/name> [-n ns] [--context ctx] [--output human|markdown|json]
```
- Read-only. Lists nodes, pods (all namespaces, for allocated resources), namespaces, PVs, PVCs, StorageClasses,
  CSINodes, CSIDrivers, VolumeAttachments and CSIStorageCapacities once; builds the scheduler snapshot and serves the
  rest to the plugins' listers from an in-memory fake clientset, so plugins never touch the cluster.
- Accepts a workload (`deploy/x`, `sts/x`, `ds/x`, `rs/x`, `job/x`, `cj/x`; a CronJob is followed to its newest Job, or to its job template if it has none): picks its oldest pod not yet on a node, or checks a new pod
  from its template if none is waiting. A pod already on a node gets one line ("scheduled on X"), in JSON `"scheduled": "X"` without plugins or nodes, and exit 0.
- **v0.4.1 (built 2026-11-20):** the source link. With `--chart` (plus `-f`, `--set`, `--version`, `--release`
  as deployed), kwhy renders the chart, finds the workload that owns the pod (a ReplicaSet is followed to its
  Deployment), and runs `kwhy value` on the pod spec fields the failing checks read: requests for the resources
  a node lacks (or the limit, when the container sets no request: Kubernetes copies it), tolerations, node selector and affinity, pod (anti-)affinity, spread constraints, host ports,
  volumes. Only fields set by a values leaf get a `Source:` line. The release defaults to the pod's
  `app.kubernetes.io/instance` label; the render uses the cluster's version for `.Capabilities`.
  `pod/` never imports `value/`: `pod.Reads` says which fields a check reads, `cmd/kwhy` joins the two.

**Non-goals:** out-of-tree scheduler plugins (they fail by name), scoring/placement ("why did it land on node X"),
and an exact preemption decision (the hint ignores PodDisruptionBudgets and nominated pods; backlog). DRA, autoscaler
events, `--scheduler-config` and the preemption hint were non-goals for v0.4 and shipped later (v0.6–v0.8).

## Output (human)

```
ml/train-b977cbd9c-8ct8w Pending 3s · 0 of 3 nodes fit · every check, every node

NODE                    TaintToleration  NodeAffinity  NodeResourcesFit
kwhy-pod-control-plane  ✗                ✗             ✗ cpu 0.95/32 +40
kwhy-pod-worker         ✓                ✓             ✗ cpu 0.1/32 +40
kwhy-pod-worker2        ✓                ✗             ✗ cpu 0.1/32 +40

Reasons:
  TaintToleration: node(s) had untolerated taint(s)
  NodeAffinity: node(s) didn't match Pod's node affinity/selector
  NodeResourcesFit: Insufficient cpu
```
One column per filter plugin that fails on some node, named as the scheduler names it, so the reasons are the
plugins' own words, never paraphrased. Resource cells carry numbers (requested/allocatable +pod's request) for cpu
and memory. Nodes with identical results are grouped (`cpu-* (9)`, or `9 nodes` without a common prefix).
JSON (`kwhy.pod/v1`) lists every node ungrouped, with all failing checks and the scheduler's first.

With `--chart` (v0.4.1) a `Source:` block follows, one line per field a failing check reads:
```
Source:
  NodeResourcesFit  .spec.containers[0].resources.requests.memory = 2Gi  ← resources.requests.memory (values-prod.yaml:4)
```

## v0.5: not running (built 2026-11-20)

v0.4 explains only pods the scheduler can't place. Most "my pod is down" cases happen after placement, so v0.5
diagnoses a pod on a node from its status and events: image pull errors, crash loops (exit code, restarts), OOM
kills (the limit it hit), missing Secrets and ConfigMaps, failed mounts, failing probes, failed init containers,
evictions. Each problem names the container and kubelet's own reason, never a guess at why an app exits, and with
`--chart` the values line behind the field involved (`killed at limit 512Mi ← resources.limits.memory`).
Also a `node` problem when the pod's node isn't Ready or is under pressure (best effort: needs `get nodes`), and
`--logs N` (opt-in) for the last run of a crashed container. Checked on kind against kubelet for pull, crash, OOM,
config, mount, probe and init failures (`pod/kind_running_test.go`).
**v0.6 (built 2026-11-20):** the workload's controller speaks too (`create`: pods it can't create, `rollout`:
progress deadline exceeded, `job`: a failed Job), pods stuck `terminating`, `ds/` targets, Cluster Autoscaler and
Karpenter events on a Pending pod (`autoscaler`), and a preemption hint (nodes it would fit on without
lower-priority pods).
`--scheduler-config FILE` (2026-11-20) runs a profile of the cluster's `KubeSchedulerConfiguration` (decoded with
the scheduler's own scheme and defaults) instead of the default one. What's still missing:
`docs/plans/backlog.md`.

## Design (validated in `docs/spikes/pod`)

- Import `k8s.io/kubernetes/pkg/scheduler/...` at a pinned version (spike: v1.37.2, 31 `replace`
  directives generated from upstream `go.mod` by a script in the repo).
- `schedmetrics.Register()` before building frameworks, or `NewFramework` panics.
- Default profile from `apis/config/latest.Default()`. The spike removed `DynamicResources` from
  `MultiPoint.Enabled` (it needs a `SharedDRAManager`, and `MultiPoint.Disabled` doesn't remove a plugin listed as
  enabled); since v0.8 kwhy builds that manager instead (see Risks).
- **All filters:** `RunFilterPlugins` returns on the first failure, so build one framework per filter
  plugin (`Filter.Enabled=[name]`, `Disabled=[*]`), run PreFilter, then Filter on every node. The spike
  shows the gain: on `gpu-a2` the scheduler reported only NodeAffinity, but NodeResourcesFit also failed.
- **PreFilter per plugin too** (found while building): the spike restricted only Filter, so every framework ran
  every PreFilter and one plugin's rejection (a missing claim in VolumeBinding) was blamed on all 13 filters.
  Each framework enables only its own plugin's PreFilter, or none. A PreFilter result that leaves a node out gets
  the scheduler's own `node(s) didn't satisfy plugin(s) [...]` status.
- **Framework options:** besides the snapshot, client and informer factory, `NodeVolumeLimits` needs the shared
  CSI manager (`nodevolumelimits.NewCSIManager`), or it panics on any pod with volumes.
- **Version skew:** warn when the cluster's minor version differs from the compiled scheduler (read from the
  binary's build info) by more than one; warn when `pod.spec.schedulerName` isn't `default-scheduler`.

Spike numbers: 52 MB binary, 0.7s for 4 nodes with every default filter plugin. In kwhy (with Helm): the
stripped linux/amd64 binary grew from 81.8 MB to 130 MB. `BenchmarkEvaluate` (1,000 nodes, 30,000 pods,
anti-affinity, spread, tolerations): 0.28s on 32 cores, 0.42s on 2. A run against a 3-node kind cluster: ~0.1s.

## Acceptance criteria (v0.4)

- **Parity test on kind:** for a fixture set of unschedulable pods (resources, taints, affinity,
  anti-affinity, topology spread, ports, volume zone), the first failing check per node in kwhy equals the
  real scheduler's. The scheduler's message (`PodScheduled` condition, same text as the `FailedScheduling`
  event) names reasons, not plugins, so the test rebuilds its `0/N nodes are available: ...` histogram from
  kwhy's first check per node and compares the text. Passing on kind 1.35.8 and 1.37.0 (`pod/kind_test.go`,
  `.github/workflows/pod.yml`).
- Every node row shows all failing checks.
- 1,000 nodes in under 5s.

## Risks

| Risk | Mitigation |
|---|---|
| `k8s.io/kubernetes` import churn (packages moved in 1.33–1.37) | Pin; the `pod/` package is the only importer; upgrade per minor with the parity suite |
| Large dependency tree and binary size | Separate Go module if needed; strip symbols |
| Clusters with custom scheduler plugins | Detect and warn; out of scope |
| DRA becoming common (GPU clusters) | Done 2026-11-20: a `SharedDRAManager` built as `scheduler.go` does, over the snapshot's ResourceClaims, ResourceSlices and DeviceClasses; each per-plugin framework gets its own profile name, since DynamicResources indexes the shared pod informer under it |
| Competitor already published | Lead with parity + source linking, not with "explains Pending pods" |
