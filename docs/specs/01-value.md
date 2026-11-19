# Spec 01: `kwhy value`

Trace any field of rendered Kubernetes config back to the source line that sets it, then change it there.

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
- Reviewer asking "if I change this key, what else moves?"
- Automation (rightsizers, Renovate-style bots, AI agents) that knows the desired rendered value and needs
  the safe source edit.

## Scope

**v0.1 (read-only, Helm):**
```
kwhy value <kind>/<name> <path> --chart <dir|oci://|repo/chart> [-f values.yaml ...] [--set k=v ...]
           [--namespace ns] [--release name] [--version x.y.z] [--output human|markdown|json]
kwhy value <kind>/<name> --all          # every field of the resource with its source
kwhy value --key resources.cpu ...      # reverse: which rendered fields does this values key affect
```
`<path>` uses a jq-like syntax: `.spec.template.spec.containers[name=api].resources.requests.memory`
(`[name=x]` selects list items by name, since indexes shift between renders).

**v0.2:** `kwhy value ... --set-value <path>=<value> [--in <file>]` write-back; then Kustomize.

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
   first, then widened.
5. **Template attribution** is file-level from Helm's `# Source:` header. A line hint inside the template
   (searching for `.Values.<key>`) may be shown, labelled as a hint, never used for edits.

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

## Write-back (`--set-value`, v0.2)

1. Only for `direct` answers. For `composed`/`derived`, print the leaves involved and refuse with an
   explanation, so the tool never guesses.
2. Target file: the file that currently wins (top of the override chain). If that is the chart's own
   `values.yaml`, write a new key into the highest-precedence user file instead; never edit vendored
   charts. `--in <file>` overrides the choice.
3. **Warn when the winning file is shared** (also passed for another environment in a known app config).
   Suggest `--in values-prod.yaml`.
4. Edit with `yaml.v3` Node API: keep comments, anchors, key order, quoting style and indentation.
5. **Verify by re-rendering:** exactly the requested field changes, nothing else. Otherwise roll back the
   edit and print the unexpected diffs.

## Kustomize (v0.2), open risk, needs a spike first

Markers do not apply: patches are literal YAML, not templates. Proposed approach, **not yet validated**:
build the overlay stack in-process with `krusty`, applying resources/patches incrementally (base,
then each patch in order) and recording which step last changed each field. Fallback if the API can't
be stepped: static analysis of strategic-merge and JSON6902 patches targeting the resource, last writer
wins. Spike this before planning v0.2.

## Acceptance criteria (v0.1)

- Golden tests on 10 public charts (the 6 above + kube-prometheus-stack, redis, keycloak, external-dns):
  precision 1.00 on sampled fields; recall ≥ 0.95 including the perturbation fallback.
- Every answer includes `kind`, source `file:line` (or `chart`/`random`), and the override chain.
- Unknown path → error listing the closest valid paths. Unknown resource → list resources in the render.
- JSON schema versioned (`"schema": "kwhy.value/v1"`).
- p95 under 2s for a single field on argo-cd-sized charts (in-process rendering).

## Risks

| Risk | Mitigation |
|---|---|
| Helm library API churn (v4) | Pin; isolate in `render/` |
| `tpl` and `lookup` functions (lookup needs a cluster) | `lookup` returns empty offline, same as `helm template`; document it |
| Subchart values and `global` propagation | Include in merge replay; golden tests on umbrella charts (argo-cd has subcharts) |
| Marker collisions with real values (e.g. a port 900001) | Check leaves for collisions before choosing the base; shift the base if needed |
| Large charts (kube-prometheus-stack) are slow | Chunking + in-process rendering; measure in v0.1 |
