# kwhy: overview

> Working name. `kwhy` is unused on GitHub (checked 2026-11-18). `kubectl-why` is taken
> (rameshsurapathi/kubectl-why) and so is `blame` (live-object field managers), so avoid both.

## Vision

One CLI that answers the three "why" questions every Kubernetes engineer asks, using the real Helm,
Kustomize and scheduler code instead of guesses:

| Command | Question | Works on | Spec |
|---|---|---|---|
| `kwhy value` | Where does this rendered value come from? | Git repo, offline | [01-value.md](01-value.md) |
| `kwhy change` | What will merging this change actually do to running workloads? | Git repo (+ optional cluster) | [02-change.md](02-change.md) |
| `kwhy pod` | Why is this pod not scheduled (v0.4), or not running (v0.5)? | Live cluster | [03-pod.md](03-pod.md) |

The features reinforce each other. `pod` says *"requests 2 CPU"* and `value` says *"set at
values-prod.yaml:31"*. `change` reuses the rendering engine `value` needs.

Positioning for the README: lead with `value`. It is the most original part (no field-level tool
exists) and the base the other two build on.

## Validation summary (spikes, 2026-11-18)

Spike code and inputs are in `docs/spikes/`.

| Feature | Question tested | Result | Verdict |
|---|---|---|---|
| value | Can marker substitution trace rendered fields to values leaves on real charts? | 6 charts (podinfo, ingress-nginx, cert-manager, grafana, argo-cd, bitnami postgresql): **precision 1.00 on all**, recall 0.96–1.00 against perturbation ground truth | Works. Remaining misses are template logic (`replicas > 1` → `true`), covered by a perturbation fallback |
| change | Do the classification rules match what a real cluster does? | 8/8 predictions matched on kind (env CM stale, volume CM synced in 53s, subPath stale, restarts, scale, ordered STS, 2 immutable failures) | Works. Real chart upgrade exposed one false positive (Helm hook Jobs), now a spec requirement |
| pod | Can the real kube-scheduler filter plugins run in-process, every filter on every node? | Built against k8s.io/kubernetes v1.37.2 (31 `replace` directives), 52 MB binary, 0.7s run; showed a second failing reason the scheduler hides | Works. Needs metrics registration, DRA disabled for v1, direct competitor exists |

## Shared design decisions

- **Go, single static binary `kwhy`**, standalone first. It doesn't need kubectl: `value` and `change` are offline, and `pod` reads kubeconfig via client-go (`--context`, `--namespace`).
- **Distribution:** GitHub Releases binaries (goreleaser, from v0.2). No `go install …@version`: importing the scheduler for `pod` needs `replace` directives, which `go install` refuses (dropped 2026-11-20).
- **Use the real libraries in-process**: Helm v4 (`helm.sh/helm/v4`), Kustomize (`sigs.k8s.io/kustomize/api/krusty`),
  kube-scheduler framework. Never shell out to `helm`/`kubectl` in the product (the spikes did, for speed).
- **Three output formats** for every command: human (default), `--output markdown` (PR comments),
  `--output json` (agents, CI). JSON is a stable, versioned contract from v0.1.
- **Offline by default.** Only `pod` and the optional `change --context` read a cluster, and only read.
- **No telemetry.**

## Code layout (feature-based, see go-guidelines)

```
cmd/kwhy/              wiring only: flags → features
render/                Helm rendering to documents with Source info (shared by value, change), plain manifests and Kustomize builds
value/                 tracing (markers, perturbation), merge replay
change/                git worktree checkouts, base/head diff, impact rules, report
pod/                   cluster snapshot, per-filter scheduler runs
docs/specs, docs/spikes
```

`pod/` is the only package importing `k8s.io/kubernetes`. If its `replace` directives become a burden,
it moves to its own Go module without changing the CLI.

## Roadmap

| Version | Ships | Gate to start the next one |
|---|---|---|
| v0.1 | `value` read-only, Helm only | Correct answers on the 6 spike charts plus 4 more in CI golden tests |
| v0.2 | `value chart` subcommand; release binaries (goreleaser). Kustomize shelved (2026-11-20) until Helm is tested and polished | Helm tested and polished |
| v0.3 | `change` (Helm only; built 2026-11-20): `--fail-on`, `json`, `--context`, GitHub Action. One chart per run; `.kwhy.yaml` and several charts dropped until needed | Rules suite green on kind in CI |
| v0.4 | `pod` (built 2026-11-20) | Parity test against the real scheduler on kind |
| v0.4.1 | `pod --chart` (built 2026-11-20): the values line that set each field a failing check reads, through `value` | |
| v0.5 | `pod` for pods on a node: pull errors, crash loops, OOM kills, config and mount errors, probes, evictions, node conditions (built 2026-11-20) | |
| v0.6 | `pod`: controller problems (create, rollout, job), terminating, `ds/`, autoscaler events, preemption hint (built 2026-11-20) | |
| v0.7 | `value`/`change`: all Helm `--set*` flags, trace cache, `change --manifests`, `no endpoints`, surge vs quota, Action on all runners (built 2026-11-20) | |
| v0.8 | `change` for several apps (`.kwhy.yaml`, Argo CD, Flux), `pod --scheduler-config`, `kwhy mcp`, signed releases with SBOMs (built 2026-11-20) | |
| v0.9 | Kustomize, experimental: `value kustomize`, `change` on overlays, `pod --kustomize` (built 2026-11-20) | |
| v0.10 | Kustomize no longer experimental; `change` for CronJob and bare Pod; `pod rs/`, `cj/`; scheduler's preemption dry run; cached Kustomize builds (built 2026-11-23) | |
| v0.11 | `value chart --all` reuses cached answers; `kwhy mcp` removed; `change` rejects a path at neither ref and checks each ref out once; inline values of one app no longer overwrite each other; Kustomize builds no longer hang when a worker fails; `pod` lists the cluster in parallel (built 2026-11-23) | |
| v0.12 | `change` refuses app paths and symlinks that leave the repo, and Kustomize remote bases and URLs before fetching; `--apps` works from a subdirectory; Ctrl-C stops Kustomize builds; `value kustomize` copies only the overlay's stack (built 2026-11-23) | |
| next | see `docs/plans/backlog.md` | |
| later | Terraform for `value`; the rest in `docs/plans/backlog.md` | |

## Decisions (2026-11-18)

1. Name: **kwhy**. Standalone binary `kwhy`.
2. License: **Apache-2.0**.
3. Versions: **Helm v4 only**; Kubernetes **N-2** for `pod`: its parity suite runs on 1.35–1.37 in CI (settled 2026-11-20).
4. Repo: **github.com/semirm-dev/kwhy**.
