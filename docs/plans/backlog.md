# kwhy: what's next

The open work and the decisions any future change must respect. What's built is in the README and
`docs/specs/`; how it was built is in git history.

## Where each command stands (2026-11-20, v0.9.0)

| Command | Helm | Plain YAML | Kustomize | Checked by | Gaps |
|---|---|---|---|---|---|
| `value` | done: every spec item, all `--set*` flags, cache | n/a | experimental | 10 charts nightly (precision 1.00, recall 0.98–1.00); 7 overlays, on demand | #3–6 |
| `change` | done: every spec rule, `--context`, Action, `.kwhy.yaml`, Argo CD / Flux | done | experimental | 23 kind fixtures on 1.35/1.37, 4 real chart upgrades | #8, #9 |
| `pod` | Pending (real scheduler, DRA, custom profiles) and not-running (kubelet, controllers) done; `--chart` | n/a | `--kustomize`, experimental | kind parity suite on 1.35–1.37 | #7, #10 |

## Open

| # | What | Why it matters | Size |
|---|---|---|---|
| 1 | **Krew index PR**: fork kubernetes-sigs/krew-index, add `plugins/kwhy.yaml` from a release's `kwhy.yaml` asset, open the PR | `kubectl krew install kwhy` | S, owner |
| 2 | **Homebrew tap**: create `semirm-dev/homebrew-tap` and a `HOMEBREW_TAP_TOKEN` secret that can push to it; the next release publishes the cask (skipped until then) | `brew install --cask semirm-dev/tap/kwhy` | S, owner |
| 3 | **Kustomize: wider testing before dropping "experimental"**: more real overlays in `value/kustomize_accuracy_test.go`, and a nightly run like Helm's | 7 overlays so far; Helm has 10 charts nightly | M |
| 4 | Kustomize remote bases: clone each once into a cache at its ref, map the clones into the in-memory copy; opt-in, since it breaks "offline" | stops with a message today | M |
| 5 | Kustomize `helmCharts:` inside a kustomization | stops with a message today | M, spike first |
| 6 | Kustomize trace cache (like Helm's), keyed by the stack's files | argo-cd HA takes ~8s per question | S |
| 7 | `kwhy pod` preemption hint that weighs PodDisruptionBudgets and nominated pods | the hint may name a node the scheduler wouldn't preempt on | M |
| 8 | `change --apps` reads Argo CD / Flux definitions at each ref, not only the working tree | an app whose definition changes in the PR uses the head's | S |
| 9 | `change`: CronJob (next run picks it up) and bare Pod (mostly immutable: `will fail`) verdicts; CronJob/Job as ConfigMap and Secret consumers | these changes get no line today | S |
| 10 | `pod cj/` and `rs/` targets | today you name one of their pods | S |

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
- **No `go install …@version`:** the scheduler import needs `replace` directives, which `go install` refuses.
  Install is Releases, Krew and Homebrew.
- **JSON contracts** (`kwhy.value/v1`, `kwhy.change/v1`, `kwhy.pod/v1`) change additively only.
- Feature packages don't import each other; `cmd/kwhy` joins them (e.g. `pod` sources through `value`).
