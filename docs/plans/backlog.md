# kwhy: what's next

The open work and the decisions any future change must respect. What's built is in the README and
`docs/specs/`; how it was built is in git history.

## Where each command stands (2026-11-23, v0.9.0 plus unreleased work)

| Command | Helm | Plain YAML | Kustomize | Checked by | Gaps |
|---|---|---|---|---|---|
| `value` | done: every spec item, all `--set*` flags, cache | n/a | done, cached | 10 charts nightly (precision 1.00, recall 0.98–1.00); 12 overlays nightly (precision 1.00, recall 0.87–1.00) | #1–4 |
| `change` | done: every spec rule, `--context`, Action, `.kwhy.yaml`, Argo CD / Flux read at each ref, CronJob and bare Pod | done | done | 25 kind fixtures on 1.35/1.37, 4 real chart upgrades | |
| `pod` | Pending (real scheduler, DRA, custom profiles) and not-running (kubelet, controllers) done; `--chart`; `rs/`, `cj/`; scheduler's own preemption dry run | n/a | `--kustomize` | kind parity suite on 1.35–1.37, preemption included | |

## Open

| # | What | Why it matters | Size |
|---|---|---|---|
| 1 | **Kustomize names behind `namePrefix`**: a prefixed name (and every reference Kustomize renames with it) names only the `namePrefix` line, not the resource's own `name:` line; kubebuilder's recall is 0.87, gated at 0.85. Fix: one more marked build that renames every resource (`name: apip9_7_`) and, with the same mark, every line that only picks it as a target (a patch's own `metadata.name`, `target: name:`, `replicas`/`replacements` selectors, inline patches too), or Kustomize fails the build on the first unmatched patch. Credit a line only when its mark shows up in a field. Not by name matching: that can name an unrelated document that shares the name | recall for prefixed names and their references | M |
| 2 | Kustomize remote bases: clone each once into a cache at its ref, map the clones into the in-memory copy; opt-in, since it breaks "offline" | stops with a message today | M |
| 3 | Kustomize `helmCharts:` inside a kustomization | stops with a message today | M, spike first |
| 4 | Fallback if a Kustomize wrong line ever turns up: `value kustomize <field>` edits the line it is about to name and rebuilds (one build per field; too slow for `--all`) | a backstop for "never name a wrong line" | S |

## Decisions to keep

- **Never name a wrong line.** Helm accuracy (10 charts, nightly) must stay at precision 1.00. Kustomize names a
  line only when that line shows the value; otherwise the entry. No override chains for Kustomize: they named
  wrong lines in the first attempt (shelved 2026-11-20, redesigned and shipped experimental in v0.9; label
  dropped 2026-11-23 after the 12-overlay check).
- **Kustomize ground truth is an edit and a rebuild** (2026-11-23): leave-one-out misses an entry that writes the value
  the field already had, so `LoadOverlay` also edits the value lines of every kustomization and patch. Strings
  share one build, each with its own mark (`p9_<n>_`), split by file then line if it fails; numbers and booleans get
  a build each. Keys that only pick a target (`name`, `kind`, `path`, `op`, ...) and resource paths are skipped.
  `value/kustomize_accuracy_test.go` measures precision and recall the same way, as Helm's test does.
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
- **No MCP server for now** (removed 2026-11-23): `kwhy mcp` (v0.8) lagged the CLI (no Kustomize, no `pod --chart`).
  It is in git history if it comes back; agents can run the CLI with `-o json`.
- Feature packages don't import each other; `cmd/kwhy` joins them (e.g. `pod` sources through `value`).
