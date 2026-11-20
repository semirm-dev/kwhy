# Spec 01: `kwhy value`

Trace any field of rendered Kubernetes config back to the source line that sets it. Read-only: kwhy never changes values or files.

## Problem

A rendered Deployment field can come from chart defaults, several `-f` values files, `--set` flags,
template logic or a Kustomize patch. Finding the right line takes minutes to hours, and editing the wrong
file (shared `values.yaml`) silently changes other environments. Bots, rightsizers and AI agents have
the same problem and usually give up on Helm values.

Existing tools stop short: helm-valuetrace tracks which values file wins per key (not per rendered field),
kubescape removed its template-line mapping because it produced wrong-line edits, and Kustomize origin
annotations are per resource.

## Users and jobs

- Platform engineer debugging "why is prod running 2Gi?"
- Automation (rightsizers, Renovate-style bots, AI agents) that needs to know which source line sets a
  rendered value (`-o json`).

## Scope

**v0.1 (read-only, Helm):**
```
kwhy value chart <dir|oci://|repo/chart> <kind>/<name> <path> [-f values.yaml ...] [--set k=v ...]
           [--namespace ns] [--release name] [--version x.y.z] [--output human|markdown|json]
kwhy value chart <chart> <kind>/<name> --all        # every field of the resource with its source
```
`<path>` uses a jq-like syntax: `.spec.template.spec.containers[name=api].resources.requests.memory`
(`[name=x]` selects list items by name, since indexes shift between renders).

**Kustomize:** shelved (2026-11-20), see below.

**Non-goals (v0.x):** Terraform/HCL, Jsonnet, cdk8s, live cluster objects (use `kubectl blame` for
"who changed it in the cluster"), editing chart templates.

## Output (human)

```
2Gi  values-prod.yaml:31   checkout.resources.memory
     ├─ overrides  values.yaml:12              = 1Gi
     └─ default    charts/svc/values.yaml:40   = 512Mi
     template      charts/svc/templates/deployment.yaml   (from "# Source" header)
```

Every answer has one of these **kinds**, shown to the user and in JSON:

| Kind | Meaning | Example |
|---|---|---|
| `direct` | the field equals one values leaf | `replicas: 3` ← `replicaCount` |
| `composed` | the field contains one or more leaves plus template text | `image: repo:tag` ← `image.repository` + `image.tag` |
| `derived` | template logic computes it from leaves (no marker survives) | `leader-election: "true"` ← `replicas` |
| `encoded` | leaf passes through `b64enc` | Secret data ← `adminUser` |
| `hashed` | checksum of other content; points at what it hashes | `checksum/config` annotation |
| `chart` | no values leaf affects it; constant in the chart | `apiVersion`, fixed ports |
| `structural` | a leaf decides whether the field exists at all (bools, enums) | `ingress.enabled` |

## Algorithm (validated in `docs/spikes/value`)

1. **Load and merge** chart defaults (including subchart defaults under their key), each `-f` file and
   `--set`, with Helm's own merge semantics (maps merge, lists replace, `null` deletes), keeping
   `yaml.Node` positions for every leaf so the answer has `file:line`.
2. **Render baseline twice** (in-process Helm). Fields that differ between the two renders are
   non-deterministic (`randAlphaNum` passwords, generated names) and are reported as `random`, never traced.
3. **Marker pass.** Replace every *truthy* string and number leaf with a unique marker, in chunks of 10
   leaves per render (`--chunk-size`; 5–10 measured fastest):
   - strings → `zq<id>qz`; numbers → `900000+id`. **Numbers must stay below 1e6**: Helm handles numbers
     as float64 and prints ≥1e6 in scientific notation, which was the main recall bug in the spike.
   - Never mark falsy values (`""`, `0`) or bools: templates use them in `default` and `if`, so marking
     them would change which branch runs and produce wrong answers.
   - Search rendered strings, whole-number floats and **base64-decoded strings** for markers.
   - If a chunk fails to render or changes the document structure, bisect it down to the culprit leaves and
     mark those `structural` / `unmarkable` (spike: 0–9 per chart, mostly enum-like strings and Bitnami's
     image validation).
4. **Perturbation fallback**, only for the field the user asked about when step 3 found no marker:
   change each candidate leaf to a nearby valid value, re-render, and see which leaves move the field
   (`derived`). Candidates are limited to leaves under the same top-level key as the field's resource
   first, then widened. Fields that do have markers still test the empty-string leaves the marker pass
   skipped (`image.tag: ""` behind `default .Chart.AppVersion`): one render with all of them nudged is
   shared by every field, and only fields it moves are bisected.
5. **Template attribution** is file-level from Helm's `# Source:` header. A line hint inside the template
   (searching for `.Values.<key>`) may be shown, labelled as a hint, never as the answer.

Spike results, 40 sampled leaves per chart checked against perturbation ground truth:

| Chart | Precision | Recall | Unmarkable leaves |
|---|---|---|---|
| podinfo 6.17 | 1.00 | 1.00 | 0 |
| ingress-nginx 4.15.1 | 1.00 | 1.00 | 2 |
| cert-manager 1.21.3 | 1.00 | 1.00 | 4 |
| grafana 10.5.15 | 1.00 | 1.00 | 1 |
| argo-cd 10.18.1 | 1.00 | 0.99 | 3 |
| bitnami postgresql 18.12.20 | 1.00 | 0.96 | 9 |

The spike rendered via the `helm` CLI (2–28s per chart, dominated by process start). In-process rendering is
expected to be roughly 10× faster; that's a performance target for v0.1, not yet measured.

## Kustomize (shelved), validated in `docs/spikes/kustomize`

Shelved on 2026-11-20 until Helm is tested and polished; to be redesigned. The first implementation
is on branch `feat/v0.2-kustomize`: leave-one-out attribution kept hitting cases that name a wrong line
(override chains, generator order, deleted documents, name collisions). The notes below describe that attempt.

Markers do not apply: patches are literal YAML, not templates. **Leave-one-out builds** instead: build
the overlay with `krusty` on an in-memory copy, then once more per entry of every kustomization in the
stack with that entry removed; fields that change were last written by it. Fields no entry moves come
from the resource file (origin annotation). The spec's earlier ideas (stepping through patches in
order, static patch analysis) are dropped: Kustomize doesn't apply transformers in file order, and
static analysis means reimplementing it.

Spike: 7 overlays from 5 repos (podinfo, flux example, microservices-demo with all components, argo-cd
plain and HA), ~39,000 fields; the attributed line shows the value for all but 6. One build per entry
plus one: under a second for typical overlays, 25s single-threaded for argo-cd HA (50 entries, 500ms
builds), to be parallelised. The rules it took (document identity, name-keyed lists, "added by" for
resources, components as routes, line lookup per entry type, `target:`-aware override chains) are in
the spike README.

**Supported by the first attempt:** local overlay stacks (overlays on bases in the same repo, nested, with
components); strategic-merge and JSON6902 patches, from files or inline; `images`, `replicas`,
`namespace`, `namePrefix`/`nameSuffix`, `commonLabels`/`commonAnnotations`/`labels`,
`configMapGenerator`/`secretGenerator` and their literals. Same command style and `kwhy.value/v1`
output as Helm: `kwhy value kustomize <overlay dir> <kind>/<name> [<path>]`, `--all`.

**Not supported by the first attempt** (for the first three kwhy stops with a message naming what it found, never a partial answer):

| Missing | Why | Later? |
|---|---|---|
| Remote bases (`https://github.com/...?ref=`) | need git and network; kwhy is offline | yes: clone each once into a cache, map the clones into the in-memory copy, opt-in since it breaks "offline" (spike README) |
| Helm inside Kustomize (`helmCharts:`) | untested; would need kwhy's Helm tracing inside the Kustomize build to name values settings | maybe; ~30 min spike first |
| Plugins and KRM functions (exec or container generators/transformers) | kwhy would have to run code from the repo | no, on purpose |
| Override chains through JSON6902 ops that address list items by index | which item an op hit depends on earlier patches | the winning line is shown; uncertain "overrides" lines are left out. Exact chains would cost extra builds (remove the winner, rebuild) |

## Removed: `--key` (2026-11-19)

`kwhy value --key <values.path>` answered the reverse question: which rendered fields does this values
setting end up in. Removed because no one needed it; rebuild it if a need shows up. The last commit with
it is `bbe29d3`. How it worked:

- CLI: `--key` replaced `<path>`/`--all` (exactly one of the three); `<kind>/<name>` was optional and
  filtered the result to that resource. The key was checked against `Values.Leaves` by `PathString`
  before tracing (`unknown values key …`).
- `value.Affects(idx, leafID)`: the fields whose marker hits (`Index.Hits`) include the leaf, sorted by
  document and path. Marker hits only, so fields a leaf reaches through template logic (`derived`) or
  as a bool/zero were not listed.
- Empty-string leaves aren't marked by `Trace`, so `--key` on one first ran `value.TraceLeaf`, which
  marked that leaf alone in one render (`tracer.mark` with a single leaf) and added its hits.
- Each field found was then explained with `Explain` as usual.
- Kustomize equivalent, if wanted: the leave-one-out builds already know which fields each entry
  moves.

## Acceptance criteria (v0.1)

- Golden tests on 10 public charts (the 6 above + kube-prometheus-stack, redis, keycloak, external-dns):
  precision 1.00 on sampled fields; recall ≥ 0.95 including the perturbation fallback.
- Every answer includes `kind`, source `file:line` (or `chart`/`random`), and the override chain.
- Unknown path → error listing the closest valid paths. Unknown resource → list resources in the render.
- JSON schema versioned (`"schema": "kwhy.value/v1"`).
- p95 under 5s for a single field on argo-cd-sized charts (in-process rendering, 8 cores). 2s is the
  later goal; most of what's left is inside Helm (manifest splitting, chart loading per render).

## Risks

| Risk | Mitigation |
|---|---|
| Helm library API churn (v4) | Pin; isolate in `render/` |
| `tpl` and `lookup` functions (lookup needs a cluster) | `lookup` returns empty offline, same as `helm template`; document it |
| Subchart values and `global` propagation | Include in merge replay; golden tests on umbrella charts (argo-cd has subcharts) |
| Marker collisions with real values (e.g. a port 900001) | Check leaves for collisions before choosing the base; shift the base if needed |
| Large charts (kube-prometheus-stack) are slow | Chunking + in-process rendering; measure in v0.1 |
