# kwhy: what's next

The open work and the decisions any future change must respect. What's built is in the README and
`docs/specs/`; how it was built is in git history.

## Where each command stands (2026-11-23, v0.9.0 plus unreleased work)

| Command | Helm | Plain YAML | Kustomize | Checked by | Gaps |
|---|---|---|---|---|---|
| `value` | done: every spec item, all `--set*` flags, cache | n/a | experimental, cached | 10 charts nightly (precision 1.00, recall 0.98–1.00); 7 overlays, on demand | #1–3 |
| `change` | done: every spec rule, `--context`, Action, `.kwhy.yaml`, Argo CD / Flux read at each ref, CronJob and bare Pod | done | experimental | 25 kind fixtures on 1.35/1.37, 4 real chart upgrades | |
| `pod` | Pending (real scheduler, DRA, custom profiles) and not-running (kubelet, controllers) done; `--chart`; `rs/`, `cj/`; scheduler's own preemption dry run | n/a | `--kustomize`, experimental | kind parity suite on 1.35–1.37, preemption included | |

## Open

| # | What | Why it matters | Size |
|---|---|---|---|
| 1 | **Kustomize: wider testing before dropping "experimental"**: more real overlays in `value/kustomize_accuracy_test.go`, and a nightly run like Helm's | 7 overlays so far; Helm has 10 charts nightly | M |
| 2 | Kustomize remote bases: clone each once into a cache at its ref, map the clones into the in-memory copy; opt-in, since it breaks "offline" | stops with a message today | M |
| 3 | Kustomize `helmCharts:` inside a kustomization | stops with a message today | M, spike first |

## Decisions to keep

- **Never name a wrong line.** Helm accuracy (10 charts, nightly) must stay at precision 1.00. Kustomize names a
  line only when that line shows the value; otherwise the entry. No override chains for Kustomize: they named
  wrong lines in the first attempt (shelved 2026-11-20, redesigned and shipped experimental in v0.9).
- **Accepted, not fixed:** Helm bisection groups where two leaves cancel (postgresql loses one hit; recall
  0.98–1.00). Ruling it out needs each leaf tested alone for every field.
- **Booleans aren't credited** behind a field that already has a match: flipping one switches template blocks,
  which would credit structure. Empty strings and zeros are.
- **Read-only and offline by default.** `pod` and `change --context` only get and list; logs only with `--logs`.
  Kubelet's and controllers' own words, never a guess at why an app exits; no fixes suggested.
- **Exact scheduler parity:** `pod` runs the real scheduler (k8s.io/kubernetes, pinned with
  `scripts/k8s-replace.sh`); a new Kubernetes version means bumping it and running the kind parity suite.
  Each per-check framework gets its own profile name, since DynamicResources indexes the shared pod informer by it.
  Preemption is the scheduler's `DefaultPreemption` as a dry run (`DryRunPreemption`, `SelectCandidate`); never
  call `Preempt()`, which evicts. Its random pick between tied nodes is reported as a tie, not as a node.
- **No `go install …@version`:** the scheduler import needs `replace` directives, which `go install` refuses.
  Install is GitHub Releases binaries.
- **JSON contracts** (`kwhy.value/v1`, `kwhy.change/v1`, `kwhy.pod/v1`) change additively only.
- Feature packages don't import each other; `cmd/kwhy` joins them (e.g. `pod` sources through `value`).
