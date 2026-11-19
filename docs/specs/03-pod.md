# Spec 03: `kwhy pod`

Explain why a pod is not scheduled: every failing check on every node, from the real scheduler code, and
the smallest change that would make it fit.

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
   that sets the problematic field (`kwhy value`), and offer the edit.

This is the most crowded of the three features, so it ships last (v0.4) and leans on 3.

## Scope

**v0.4:**
```
kwhy pod <name|kind/name> [-n ns] [--context ctx] [--what-if] [--output human|markdown|json]
```
- Read-only. Lists nodes, pods (all namespaces, for allocated resources), PVs, PVCs, StorageClasses,
  CSINodes; builds the scheduler snapshot.
- Accepts a workload (`deploy/x`): picks one Pending pod from it, or renders its template if none exists.

**Non-goals (v0.4):** DRA resource claims (detect and say unsupported), secondary schedulers with custom
plugins, scoring/placement ("why did it land on node X").

## Output (human)

```
Pending 14m · 0 of 12 nodes fit · every check, every node

NODE       CPU            MEMORY          TAINTS          AFFINITY  VOLUME
gpu-a1     ✗ 7.5/8 +2     ✓               ✓               ✓         ✓
gpu-a2     ✓              ✗ 61/64 +16Gi   ✓               ✗ zone b  ✓
cpu-* (9)  ✓              ✓               ✗ nvidia.com    ✗ gpu     ✓

Smallest fixes (re-ran the filters on a modified copy)
 → lower cpu request 2 → 1                 fits gpu-a1
   set at values-prod.yaml:31 (kwhy value) — run with --set-value to change it
```
Nodes with identical results are grouped (`cpu-* (9)`). Cells carry numbers
(requested/allocatable), not just plugin messages.

## Design (validated in `docs/spikes/pod`)

- Import `k8s.io/kubernetes/pkg/scheduler/...` at a pinned version (spike: v1.37.2, 31 `replace`
  directives generated from upstream `go.mod` by a script in the repo).
- `schedmetrics.Register()` before building frameworks, or `NewFramework` panics.
- Default profile from `apis/config/latest.Default()`. **Remove `DynamicResources` from
  `MultiPoint.Enabled`**: it needs a `SharedDRAManager`, and `MultiPoint.Disabled` doesn't remove a
  plugin that is listed as enabled (both found in the spike).
- **All filters:** `RunFilterPlugins` returns on the first failure, so build one framework per filter
  plugin (`Filter.Enabled=[name]`, `Disabled=[*]`), run PreFilter, then Filter on every node. The spike
  shows the gain: on `gpu-a2` the scheduler reported only NodeAffinity, but NodeResourcesFit also failed.
- **What-if:** mutate a copy of the pod and re-run: lower each failing resource request to the closest
  node's free amount; add a toleration for each untolerated taint; drop or relax one affinity term.
  Report the single changes that make at least one node fit, smallest first. Preemption ("evict
  lower-priority pods") uses the DefaultPreemption PostFilter dry-run, planned for after v0.4.
- **Autoscaler awareness:** if Cluster Autoscaler/Karpenter events show a scale-up for this pod, say
  "a node is being added" before the matrix.
- **Version skew:** warn when the cluster's minor version differs from the compiled scheduler by more than
  one; warn when `pod.spec.schedulerName` isn't `default-scheduler`; accept
  `--scheduler-config <KubeSchedulerConfiguration>` for clusters with custom profiles.

Spike numbers: 52 MB binary (compressible with `-ldflags="-s -w"` + UPX if needed), 0.7s for 4 nodes with
every default filter plugin. Needs a benchmark at 1,000 nodes / 30,000 pods before v0.4.

## Acceptance criteria (v0.4)

- **Parity test on kind:** for a fixture set of unschedulable pods (resources, taints, affinity,
  anti-affinity, topology spread, ports, volume zone), the first failing plugin per node in kwhy equals
  the plugin named in the real `FailedScheduling` event.
- Every node row shows all failing checks.
- `--what-if` suggestions are verified: applying each suggested change on kind makes the pod schedule.
- 1,000 nodes in under 5s.

## Risks

| Risk | Mitigation |
|---|---|
| `k8s.io/kubernetes` import churn (packages moved in 1.33–1.37) | Pin; the `pod/` package is the only importer; upgrade per minor with the parity suite |
| Large dependency tree and binary size | Separate Go module if needed; strip symbols |
| Clusters with custom scheduler plugins | Detect and warn; out of scope |
| DRA becoming common (GPU clusters) | Implement `SharedDRAManager` from listers in a later release |
| Competitor already in Krew | Lead with parity + source linking, not with "explains Pending pods" |
