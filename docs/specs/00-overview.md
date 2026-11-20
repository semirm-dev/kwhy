# kwhy: overview

> Working name. `kwhy` is free on Krew and unused on GitHub (checked 2026-11-18). `kubectl-why` is taken
> (rameshsurapathi/kubectl-why) and `blame` is taken on Krew (live-object field managers), so avoid both.

## Vision

One CLI that answers the three "why" questions every Kubernetes engineer asks, using the real Helm,
Kustomize and scheduler code instead of guesses:

| Command | Question | Works on | Spec |
|---|---|---|---|
| `kwhy value` | Where does this rendered value come from? | Git repo, offline | [01-value.md](01-value.md) |
| `kwhy change` | What will merging this change actually do to running workloads? | Git repo (+ optional cluster) | [02-change.md](02-change.md) |
| `kwhy pod` | Why is this pod not running? | Live cluster | [03-pod.md](03-pod.md) |

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
- **Distribution:** `go install`, GitHub Releases binaries and Homebrew (goreleaser, from v0.2). Krew from v0.4 when `pod` ships: the same binary installed as `kubectl-kwhy`.
- **Use the real libraries in-process**: Helm v4 (`helm.sh/helm/v4`), Kustomize (`sigs.k8s.io/kustomize/api/krusty`),
  kube-scheduler framework. Never shell out to `helm`/`kubectl` in the product (the spikes did, for speed).
- **Three output formats** for every command: human (default), `--output markdown` (PR comments),
  `--output json` (agents, CI). JSON is a stable, versioned contract from v0.1.
- **Offline by default.** Only `pod` and the optional `change --context` read a cluster, and only read.
- **No telemetry.**

## Code layout (feature-based, see go-guidelines)

```
cmd/kwhy/              wiring only: flags → features
render/                Helm rendering to documents with Source info (shared by value, change; Kustomize shelved)
value/                 tracing (markers, perturbation), merge replay
change/                git worktree checkouts, base/head diff, impact rules, report
pod/                   cluster snapshot, per-filter scheduler runs
internal/k8s/          tiny helpers shared by change and pod (GVK keys, pod template access)
docs/specs, docs/spikes
```

`pod/` is the only package importing `k8s.io/kubernetes`. If its `replace` directives become a burden,
it moves to its own Go module without changing the CLI.

## Roadmap

| Version | Ships | Gate to start the next one |
|---|---|---|
| v0.1 | `value` read-only, Helm only | Correct answers on the 6 spike charts plus 4 more in CI golden tests |
| v0.2 | `value chart` subcommand; release binaries (goreleaser; Homebrew once a tap exists). Kustomize shelved (2026-11-20) until Helm is tested and polished | Helm tested and polished |
| v0.3 | `change` (Helm only, offline; proof of concept done 2026-11-20), then `--fail-on`, `json`, `--context`, GitHub Action | Rules suite green on kind in CI |
| v0.4 | `pod`; Krew manifest (`kubectl-kwhy`) | Parity test against real scheduler events on kind |
| later | `value kustomize` (redesign; first attempt on branch `feat/v0.2-kustomize`), Argo CD / Flux app discovery, MCP server, Terraform for `value`, remote Kustomize bases (fetched, opt-in) | |

## Decisions (2026-11-18)

1. Name: **kwhy**. Standalone binary `kwhy` first; Krew plugin `kwhy` (`kubectl-kwhy`) from v0.4.
2. License: **Apache-2.0**.
3. Versions: **Helm v4 only**; Kubernetes **N-2** for `pod` (default, revisit before v0.4).
4. Repo: **github.com/semirm-dev/kwhy**.
