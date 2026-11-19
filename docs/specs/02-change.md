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

**v0.3:**
```
kwhy change <base>..<head> [--app <path> ...] [--config .kwhy.yaml] [--context <kube-context>]
            [--output human|markdown|json] [--fail-on fail,data-loss]
```
- Renders base and head with the shared `render/` engine (`git worktree` checkouts in a temp dir).
- What to render comes from `--app` (chart dir + values files, or a kustomization dir) or `.kwhy.yaml`.
  Auto-discovery of Argo CD `Application` and Flux `HelmRelease`/`Kustomization` comes later.
- `--context` (optional, read-only) adds live replica counts, HPA ranges and rollout time estimates.
- GitHub Action that posts or updates one PR comment (markdown output).

**Non-goals:** applying anything, validating against admission webhooks/policies (Kyverno etc.), cost
estimates.

## Output (markdown, as posted to a PR)

```
Merging this PR will:
↻ restart    Deployment prod/checkout     12 pods   image tag
⏱ ordered    StatefulSet prod/postgres     3 pods   one at a time, ~9m
! no effect  ConfigMap prod/api-config → Deployment prod/api
             env var: running pods keep the OLD value until they restart
✗ will fail  StatefulSet prod/cache  spec.serviceName is immutable
⚠ data       PersistentVolumeClaim prod/data-postgres-0 deleted
45 pods restarted · est. 11 min · 1 apply error
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

**Found by the real-chart run, required in v0.3:**
- **Hooks are recreated, not patched.** Objects with `helm.sh/hook` (any delete policy) or
  `argocd.argoproj.io/hook` run as hooks, so a changed hook Job is `runs as hook`, not `will fail`.
  The spike flagged ingress-nginx's admission Jobs as failing; that was wrong.
- Argo sync options `Replace=true` / `Force=true` (annotation `argocd.argoproj.io/sync-options`) turn
  immutable-field failures into delete+recreate: report `recreate` instead of `will fail`.
- Deduplicate consumers (grafana's Secret was reported twice).

**Specified, not yet validated (validate in the v0.3 kind suite):**
- Immutable: workload `spec.selector`; Service `clusterIP` and type changes from/to `ExternalName`;
  ConfigMap/Secret with `immutable: true`; PVC spec except `resources.requests.storage` increases
  (and only if the StorageClass has `allowVolumeExpansion`).
- `data` warnings: deleting a PVC, a StatefulSet whose `persistentVolumeClaimRetentionPolicy.whenDeleted`
  is `Delete`, or a Namespace; CRD deletions (cascade to all custom resources).
- DaemonSet template change: one pod per node (`--context` for node count), respecting `maxUnavailable`.
- Projected volumes, `secretKeyRef`, image-only changes with mutable tags (`:latest` + `Always` restarts on
  any rollout, invisible in the diff: warn).
- Rollout estimate (with `--context`): replicas (HPA current, not manifest), `maxSurge`/`maxUnavailable`,
  `minReadySeconds`, readiness probe timing, StatefulSet `podManagementPolicy` and `partition`.

## Acceptance criteria (v0.3)

- A kind-based CI suite with one fixture per rule (spike fixtures as the start). Each rule's prediction
  is checked against the cluster after `kubectl apply`. The suite is the source of truth for rules.
- Real-chart regression set: chart version bumps for ingress-nginx, argo-cd, cert-manager, grafana with no
  false `will fail` (the hook bug must stay fixed).
- Markdown output stays under 65,000 characters (GitHub comment limit) with a collapsed full list.
- `--fail-on` exit codes for CI.

## Risks

| Risk | Mitigation |
|---|---|
| Rendering needs the same inputs Argo/Flux use (values from app spec, Helm parameters) | `.kwhy.yaml` first; Argo/Flux discovery as a later feature |
| Server-side defaulting makes base/head differ in ways the live cluster doesn't | Compare rendered↔rendered only (both sides undefaulted); optional live diff later |
| Rules drift across Kubernetes versions | Kind suite runs against N-2..N in CI |
| Overlap with diff tools | Complementary: link to the full diff; never re-print it |
