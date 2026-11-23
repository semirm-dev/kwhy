# Spec 02: `kwhy change`

Explain what merging a change will do to running workloads, as a short PR comment instead of a raw diff.

## Problem

argocd-diff-preview, Thule and `flux diff` post the full rendered diff. Reviewers still have to work out
which workloads restart and how many pods, which StatefulSets roll slowly one pod at a time, which
ConfigMap edits never reach running pods, and which changes the API server will reject. Those are the
mistakes that cause incidents after a "harmless" merge.

## Users and jobs

- Reviewer of a GitOps PR: "is this safe to merge now?"
- Author: "did my ConfigMap change actually take effect?"
- CI: block or label PRs that will fail to apply or restart stateful workloads.

## Scope

**v0.3 (built 2026-11-20, Helm only):**
```
kwhy change <base>..<head> --chart <dir> [-f values.yaml ...] [--set k=v ...] [--release rel] [-n ns]
            [--kube-version v] [--context <kube-context>] [-o human|markdown|json] [--fail-on fail,data-loss]
```
- Renders base and head with the shared `render/` engine in `git worktree` checkouts in a temp dir,
  removed afterwards. Chart and values paths inside the repo are read at each ref.
- `--context` (optional, read-only: get and list) adds live replica counts, HPA ranges, DaemonSet node counts,
  StorageClass expansion and rollout time estimates (`change/live.go`).
- `--fail-on` exits 3; `-o json` is `kwhy.change/v1`; markdown stays under GitHub's comment limit.
- GitHub Action (`action.yml`) that posts or updates one PR comment (markdown output).
- One chart per run (several apps per run since v0.8, below).

**v0.7 (built 2026-11-20):** `--manifests DIR` for plain YAML instead of a chart (a Kustomize directory was
refused until v0.9), Helm's `--set-string`/`--set-json`/`--set-file`/`--set-literal`, a `no endpoints` verdict for a Service
whose selector matched rendered pods before and none after (checked on kind: its EndpointSlices empty), and with
`--context` a note when a Deployment's surge pods exceed the namespace's ResourceQuota. The Action takes
`manifests` and runs on Linux, macOS and Windows.

**v0.8 (built 2026-11-20):** several apps per run. `--config FILE` (default `.kwhy.yaml`) lists charts and
manifests directories with their values, release and namespace; `--apps DIR` builds the same list from Argo CD
`Application`s (`source`/`sources`, `helm.valueFiles`, `parameters`, `values`/`valuesObject`, `releaseName`,
destination namespace) and Flux `HelmRelease`s (`chart` from a `GitRepository`, `valuesFiles`, `values`,
`releaseName`, `targetNamespace`). Other repos, Helm repositories, `$ref` value files, Flux `valuesFrom` and
Kustomize are skipped with a warning. All targets' documents go through one diff. A path missing at a ref renders
as nothing, so a new app is `create`. The app definitions are read at each ref (since 2026-11-23): a PR that changes an
Application's parameters or adds an app renders the base with the base's definitions and the head with the head's.

**v0.9 (built 2026-11-20, experimental):** Kustomize overlays in `--manifests` (built with Kustomize's library), Argo CD apps
pointing at an overlay, and Flux `Kustomization` objects in `--apps`.

**Built 2026-11-23 (unreleased):** CronJob `schedule` or `jobTemplate` changed: `delayed` (the next run picks it up). Bare
Pod: an image change (with tolerations added and `activeDeadlineSeconds` added or lowered) is `restart`, in place;
any other spec change is `will fail` (kind fixtures `pod-image`, `pod-immutable`). Jobs, CronJobs and bare Pods are
consumers of a changed ConfigMap or Secret; a CronJob's is `picked up` by the next run.

**Non-goals:** applying anything, validating against admission webhooks/policies (Kyverno etc.), cost
estimates.

## Output (markdown, as posted to a PR)

```
Merging this PR will:
↻ restart    Deployment prod/checkout     12 pods   image
⏱ ordered    StatefulSet prod/postgres     3 pods   one at a time, ~9m
! no effect  ConfigMap prod/api-config → Deployment prod/api
             env var: running pods keep the OLD value until they restart
✗ will fail  StatefulSet prod/cache  spec.serviceName is immutable
⚠ data       StatefulSet prod/redis  deleted, and its PVCs with it
15 pods restarted · est. 9m · 1 apply error · 1 data warning
```

## Rules

**Validated on kind (8/8 matched, `docs/spikes/change`):**

| Change | Verdict | Observed |
|---|---|---|
| Workload `spec.template` changed | `restart` (N pods) | 3/3 new pods |
| Only `replicas` changed | `scale a → b` | 1 kept + 1 new |
| StatefulSet template changed | `ordered` restart | 2/2 replaced in order |
| ConfigMap consumed via `env`/`envFrom` changed, consumer not restarted | `no effect`: old value until restart | `MODE=v1` after apply |
| ConfigMap consumed as volume changed | `delayed`: file updates (~1 min), app must re-read | `v2` after 53s, no restart |
| ConfigMap consumed via `subPath` changed | `no effect`: never updates | still `v1` |
| ConfigMap changed and consumer template also changed (checksum annotation) | `picked up` via restart | grafana real chart |
| Job `spec.template` changed | `will fail` (immutable) | API rejected |
| StatefulSet field outside the mutable set changed | `will fail` | API rejected `serviceName` |

**Found by the real-chart run (built, guarded by `change/accuracy_test.go`):**
- **Hooks are recreated, not patched.** Objects with `helm.sh/hook` (any delete policy) or
  `argocd.argoproj.io/hook` run as hooks, so a changed hook Job is `runs as hook`, not `will fail`.
  The spike flagged ingress-nginx's admission Jobs as failing; that was wrong.
- Argo sync options `Replace=true` / `Force=true` (annotation `argocd.argoproj.io/sync-options`) turn
  immutable-field failures into delete+recreate: report `recreate` instead of `will fail`.
- Deduplicate consumers (grafana's Secret was reported twice).

**Validated on kind 1.35 and 1.37 by the rules suite (`change/kind_test.go`, `testdata/change/`, 25 fixtures):**
- Everything in the first table, plus: immutable workload `spec.selector`, Service `clusterIP`, ConfigMap with
  `immutable: true`; `OnDelete` (StatefulSet, DaemonSet); StatefulSet `partition`; DaemonSet restart one pod
  per node; `secretKeyRef` and projected-volume consumers; StatefulSet deletion with
  `persistentVolumeClaimRetentionPolicy.whenDeleted: Delete` takes its claims.
- PVC: any spec change except `resources.requests` (and `volumeAttributesClassName`) is rejected; storage can't
  shrink; growing needs a bound claim and a StorageClass with `allowVolumeExpansion`. Offline it's `resize`
  "if the StorageClass allows expansion"; `--context` turns it into `will fail` when the class can't expand or
  the claim isn't bound.
- **Service type to or from `ExternalName` is accepted** (the API drops or allocates the clusterIP), so it's
  not a rule. Pinned by a unit test.
- Not checkable with kubectl, unit-tested only: hooks (Helm runs them), Argo `Replace`/`Force` (Argo does),
  deleting a PVC or Namespace, CRD deletion.

**Also built:**
- Restarts that pull a mutable tag (`:latest` or untagged, no digest) say so: the rollout may change the code
  even when the diff doesn't show it.
- Rollout estimate (with `--context`): pods from the HPA's current replicas, else the live object;
  batches from `maxSurge`/`maxUnavailable` (StatefulSet: `maxUnavailable`, default 1); each batch takes ~10s
  start plus the slowest readiness probe's `initialDelaySeconds + periodSeconds × successThreshold` plus
  `minReadySeconds`. Workloads roll in parallel, so the summary shows the slowest. `podManagementPolicy` only
  affects scaling, not rolling updates, so the estimate ignores it.

## Acceptance criteria (v0.3) — met 2026-11-20

- A kind-based CI suite with one fixture per rule (`.github/workflows/change.yml`, Kubernetes 1.35–1.37).
  Each rule's prediction (with `--context` against the same cluster) is checked against the cluster after
  `kubectl apply`. The suite is the source of truth for rules.
- Real-chart regression set: upgrades from the previous minor of ingress-nginx, argo-cd, cert-manager and
  grafana with no false `will fail` (`TestAccuracyUpgrades`, in `ci.yml`; charts in `testdata/charts/prev`).
- Markdown output stays under 65,000 characters: past it, failures, data warnings and recreates stay in view and
  the rest folds into a collapsed list, cut to fit.
- `--fail-on` exit codes for CI.

## Risks

| Risk | Mitigation |
|---|---|
| Rendering needs the same inputs Argo/Flux use (values from app spec, Helm parameters) | `.kwhy.yaml` first; Argo/Flux discovery as a later feature |
| Server-side defaulting makes base/head differ in ways the live cluster doesn't | Compare rendered↔rendered only (both sides undefaulted); optional live diff later |
| Rules drift across Kubernetes versions | Kind suite runs against N-2..N in CI |
| Overlap with diff tools | Complementary: link to the full diff; never re-print it |
