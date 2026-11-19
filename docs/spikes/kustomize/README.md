# Spike: Kustomize field attribution (2026-11-19)

**Question:** can kwhy name the kustomization entry and `file:line` that sets each rendered field of a
Kustomize overlay, in-process and offline, fast enough for the CLI?

**Answer: yes, with leave-one-out builds.** Build the overlay once with `krusty` on an in-memory copy of
the tree. Then, for each entry of every `kustomization.yaml` in the stack (each patch, `images` and
`replicas` item, `namespace`/`namePrefix`/`nameSuffix`, `commonLabels` key, generator and literal,
component), build again with that one entry removed. Fields that change were last written by that
entry. Fields no entry moves come from the resource file, located through the origin annotation.
No Kustomize feature is reimplemented; stepping through patches in order wouldn't work anyway, since
Kustomize doesn't apply transformers in file order.

Run: `go run . [-v] <root> <overlay dir relative to root>`, e.g. `go run . -v fixture overlays/prod`.

## Results

"Line holds value" checks that the attributed line's text shows the field's value (or part of it, for
a `namePrefix` or image tag). It's a proxy for precision; leave-one-out itself defines "last writer".

| Overlay (commit) | Entries | Fields | Line holds value | Per build |
|---|---|---|---|---|
| `fixture/overlays/prod` (hand-written, known answers) | 14 | 25 | 20/20 | ~7ms |
| podinfo `kustomize` (f72ffd4) | 0 | 78 | 78/78 | 2ms |
| flux2-kustomize-helm-example `apps/production` (4f1a5b1) | 2 | 32 | 32/32 | 5ms |
| flux2-kustomize-helm-example `infrastructure/controllers` | 0 | 48 | 48/48 | 2ms |
| microservices-demo `tests/memorystore-with-all-components` (c7cb6d5) | 16 | 704 | 704/704 | 50ms |
| microservices-demo `tests/service-mesh-istio-with-all-components` | 14 | 775 | 775/775 | 40ms |
| argo-cd `manifests/cluster-install` (232fa2d) | 2 | 18,652 | 18,652/18,652 | 215ms |
| argo-cd `manifests/ha/cluster-install` | 50 | 19,027 | 19,021/19,027 | 500ms |
| kubeflow/manifests `example` (65afa7c) | | | build fails: remote bases | |

The fixture's expected answers all hold: memory from `overlays/prod/memory.yaml:12` overriding staging's
patch and the base; replicas, image tag, namespace and namePrefix from their entries; cpu from the
`value:` line of an inline JSON6902 patch; the annotation from the component's patch file; the
ConfigMap's hashed name from the generator, its literal and the namePrefix.

The 6 argo-cd HA misses: `defaultMode: 0755` renders as `493` (right line, the check compares text);
two JSON6902 `add /rules/0` ops on an unnamed list (which op made which item depends on order); one
initContainer field that falls back to the entry's line.

**Cost:** one build per entry plus one. argo-cd HA: 51 builds, 25s single-threaded. Builds are
independent, so they parallelise like Helm tracing chunks (~4s on 8 cores, not measured). Typical
overlays (≤20 entries, ≤50ms builds) finish in under a second.

## What it took (rules for v0.2)

1. **Document identity:** kind + origin annotation + position among those. `OrgId()` is just the final
   id after a build (Kustomize drops its rename bookkeeping), so removing `namePrefix` would make every
   field look changed. Same lesson as Helm decision #8.
2. **Named list items keyed by name** (`containers[name=api]`, `env[name=PORT]`), as strategic merge
   does. By index, a patch inserting one env var made every later env var look set by that patch (46
   false attributions on microservices-demo).
3. **A resource that disappears** when an entry is removed was *added* by it (a component's resources,
   a generator). Record "added by", don't attribute its fields to the entry; their lines are in the
   resource file.
4. **A component and the patch inside it both move a field** (removing the component removes its
   patch). Report the innermost entry; the component is the route.
5. **Line lookup per entry type:** strategic-merge patch files and inline patches by path (documents
   filtered by kind; `apiVersion`/`kind`/`metadata.name` in a patch select the target, they set
   nothing); JSON6902 ops (inline or file) by the op whose path is the field or a parent of it, then
   inside its `value`; `replicas` → `count`, `images` → `newTag`/`digest`/`newName`; scalars → their
   value; generated ConfigMap data → the `KEY=` literal; multi-document resource files by kind +
   position.
6. **Override chain ("overrides …"):** a static lookup of other patches in the stack that set the same
   path, respecting each entry's `target:`. Reliable for strategic merge and name-keyed lists; not for
   JSON6902 ops that address list items by index (the index depends on earlier patches). If the chain
   must be exact everywhere: remove the winner, rebuild, find the next writer (more builds).

## Limits found

- **Remote bases** (`https://github.com/...?ref=`) need git and network; krusty can't fetch them into an
  in-memory filesystem. Decided 2026-11-19: v0.2 stays offline and errors out naming the remote bases;
  fetching comes later. The efficient way: clone each remote base once (shallow, at its ref) into a
  cache, put the clones into the in-memory copy and rewrite the entries to them, so leave-one-out builds
  don't re-clone. Needs a parser for Kustomize's remote URL forms (its own is internal) and `git`; opt-in.
- `helmCharts:` inside a kustomization (Kustomize's Helm inflator) wasn't tested.
- Removing an entry can fail the build (a `behavior: merge` generator whose base generator is gone): no
  attribution from that entry; the fixture's generator is still attributed through its literal.
