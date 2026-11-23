# kwhy

[![ci](https://github.com/semirm-dev/kwhy/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/semirm-dev/kwhy/actions/workflows/ci.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/semirm-dev/kwhy)](go.mod)
[![Go Reference](https://pkg.go.dev/badge/github.com/semirm-dev/kwhy.svg)](https://pkg.go.dev/github.com/semirm-dev/kwhy)
[![License](https://img.shields.io/github/license/semirm-dev/kwhy)](LICENSE)

kwhy answers three "why" questions about Kubernetes config. It doesn't guess and doesn't re-implement rules: the
answers come from Helm, Kustomize and the kube-scheduler themselves, compiled in.

| Command | Question | Needs |
|---|---|---|
| `kwhy value` | Where does this rendered value come from? | a chart or overlay, offline |
| `kwhy change` | What will merging this PR do to running workloads? | a git repo; a cluster optional |
| `kwhy pod` | Why isn't this pod running? | a cluster, read-only |

One static binary. Offline by default, read-only when it talks to a cluster, no telemetry.

## Install

Download the archive for your platform (macOS, Linux or Windows; amd64 or arm64) from
[GitHub Releases](https://github.com/semirm-dev/kwhy/releases), unpack it and put `kwhy` on your `PATH`.

<details>
<summary>Verify a release (SBOM and cosign signature, from v0.8)</summary>

```sh
cosign verify-blob --certificate checksums.txt.pem --signature checksums.txt.sig \
  --certificate-identity-regexp 'https://github.com/semirm-dev/kwhy/' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt
```
</details>

## Try it in 5 minutes

Clone this repo for its test chart. Everything below is real output.

```sh
git clone https://github.com/semirm-dev/kwhy && cd kwhy
```

**1. Where does a value come from?** The chart's default says 512Mi, two values files override it. Which one wins?

```console
$ kwhy value chart testdata/charts/mini deploy/rel-api \
    '.spec.template.spec.containers[name=api].resources.requests.memory' \
    -f testdata/values-prod.yaml -f testdata/values-anchors.yaml
4Gi  testdata/values-anchors.yaml:2  resources.requests.memory
     ├─ overrides  testdata/values-prod.yaml:4  = 2Gi
     └─ default    mini/values.yaml:7           = 512Mi
     template      mini/templates/deployment.yaml
```

Don't know the resource name? Guess: the error lists everything the chart renders. `--all` explains every field.

**2. What will a PR do?** Copy the chart into a throwaway git repo, change one ConfigMap value, and ask:

```console
$ mkdir /tmp/demo && cp -r testdata/charts/mini /tmp/demo/chart && cp testdata/values-prod.yaml /tmp/demo/
$ cd /tmp/demo && git init -q && git add . && git commit -qm base
$ perl -pi -e 's/logLevel: info/logLevel: debug/' chart/values.yaml && git commit -qam 'debug logs'
$ kwhy change HEAD~1..HEAD --chart chart -f values-prod.yaml
Merging this change will:
↻ restart  Deployment default/rel-api  3 pods  pod template
3 pods restarted
```

A log level change restarts 3 pods, because the chart hashes the ConfigMap into a pod annotation. `-o markdown`
makes it a PR comment, `--fail-on restart` fails CI with exit code 3, and `--context prod` reads live replica
counts and estimates the rollout time.

**3. Why isn't a pod running?** On any cluster (here `kind create cluster`):

```console
$ kubectl create deployment big --image=registry.k8s.io/pause:3.10
$ kubectl set resources deploy big --requests=cpu=64
$ kwhy pod deploy/big
default/big-6c4db49874-fpchk Pending 50s · 0 of 1 nodes fit · every check, every node

NODE                NodeResourcesFit
kind-control-plane  ✗ cpu 0.95/32 +64

Reasons:
  NodeResourcesFit: Insufficient cpu

$ kubectl create deployment crash --image=busybox:1.37 -- sh -c 'echo connecting to db; echo fatal: password rejected; exit 1'
$ kwhy pod deploy/crash --logs 5
default/crash-85c79cf665-zggn2 on kind-control-plane · Running · not ready · 1 container, 1 failing

CONTAINER  PROBLEM  DETAIL
busybox    crash    exit 1, 3 restarts, last 14s ago

Logs (busybox, last run):
  connecting to db
  fatal: password rejected
```

## `kwhy value`: where does this value come from?

<img src="docs/demo/value.svg" alt="kwhy value tracing requests.memory to values-prod.yaml:31 and what it overrides" width="100%">

<sub>Illustration; the console blocks on this page are real output.</sub>

A rendered field can come from chart defaults, several `-f` files, `--set` flags or template logic. kwhy names the
line that wins and everything it overrides. It follows Helm's own merge rules: later files win, maps merge, lists
replace whole, `null` deletes, subchart defaults mount under their name or alias, and YAML anchors point at the line
where the value is written.

When a field isn't a plain copy of one value, the answer says so instead of guessing:

```console
$ kwhy value chart testdata/charts/mini deploy/rel-api '.spec.template.spec.containers[name=api].env[name=LEADER_ELECTION].value' -f testdata/values-prod.yaml
true  derived  ← replicaCount (testdata/values-prod.yaml:1)
      template  mini/templates/deployment.yaml
```

Charts can be a directory, a `.tgz`, `oci://` or `repo/chart`. `-o markdown` is for PR comments, `-o json` for
scripts and agents.

**How it works:** kwhy doesn't read templates. It renders the chart with Helm, changes values on purpose and
watches which output lines change. The [illustrated walkthrough](https://claude.ai/artifact/DUEg6FMnDfw65yfdUWJj4v)
shows every step. The first question about a chart traces it whole (0.02s to 21s, see
[below](#how-we-know-its-right)); later questions about the same chart and values come from a cache (files unused for 30 days are removed).

**Kustomize:** `kwhy value kustomize <overlay> <kind>/<name> [<path>|--all]` names the kustomization
entry, and the line, that sets a field:

```console
$ cd testdata/kustomize/fixture
$ kwhy value kustomize overlays/prod deploy/api '.spec.template.spec.containers[name=api].resources.requests.memory'
2Gi  overlays/prod/memory.yaml:12  patches[0] path=memory.yaml
     template  base/deployment.yaml
```

It builds the overlay, then again with each entry removed, and again with the values of the kustomizations and
patches edited: the line whose edit changes a field wrote it, even when it sets the value the field already had. It
names a line only when that holds, otherwise just the entry. Remote bases, `helmCharts:` and plugins
stop with a message.

## `kwhy change`: what will this PR do?

Diff tools show the rendered diff. `kwhy change` says what the diff *does*:

<img src="docs/demo/change.svg" alt="kwhy change summarising which workloads restart, roll in order, see no effect or fail" width="100%">

<sub>Illustration. Real output, for a chart deployed to kind:</sub>

```console
$ kwhy change main~1..main --chart chart -n kwhy-demo --context kind-kwhy --fail-on fail
Merging this change will:
⏱ ordered    StatefulSet kwhy-demo/postgres                      3 pods  one at a time, ~30s
! no effect  ConfigMap kwhy-demo/cfg → Deployment kwhy-demo/api          env var: running pods keep the OLD value until they restart
✗ will fail  Job kwhy-demo/migrate                                       spec.template is immutable
✗ will fail  StatefulSet kwhy-demo/cache                                 spec.serviceName is immutable
3 pods restarted · est. 30s · 2 apply errors
kwhy: --fail-on: will fail Job kwhy-demo/migrate
```

It renders both refs in temporary git worktrees. Sources: a chart (`--chart`), plain YAML or a Kustomize overlay
(`--manifests`), several targets in a `.kwhy.yaml`, or every Argo CD `Application` and Flux `HelmRelease` under
`--apps DIR` that points into the repo. App paths are read as Argo CD and Flux read them, from the repo root and
never above it, and a ref with a symlink out of the repo, or a Kustomize base outside the checkout (through `..` or a
symlink), is refused, so a pull request can't point kwhy at the runner's files. A file under `--apps` that stops
decoding after an earlier document gets a warning naming it, since apps after the bad document aren't read. Offline, pod counts come from `replicas`. With `--context` it reads the
cluster (get and list only) for live replicas, HPA ranges, DaemonSet node counts, StorageClass expansion, the
namespace's ResourceQuota and a rollout time estimate.

### In a pull request

```yaml
on: pull_request
permissions: {contents: read, pull-requests: write}
jobs:
  kwhy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
        with: {fetch-depth: 0}   # kwhy needs both refs
      - uses: semirm-dev/kwhy@v0.13.0
        with:
          chart: charts/api
          values: values-prod.yaml
          fail-on: fail,data-loss
```

It posts one comment per PR and updates it on every push. Other inputs: `manifests`, `config`, `apps`, `set`,
`release`, `namespace`, `kube-version`, `base`, `head`, `version`, `comment`. Linux, macOS and Windows runners.

## `kwhy pod`: why isn't it running?

It says what's wrong; fixing it stays with you. Give it a pod or a workload (`deploy/`, `sts/`, `ds/`, `rs/`, `job/`,
`cj/`); for a workload it picks the worst pod, or says `all N pods ready`.

**Pending:** kwhy runs the real kube-scheduler filters on every node and shows *every* failing check. The scheduler's
own event only names the first one per node, so fixing that one often just reveals the next:

```console
$ kwhy pod deploy/train -n ml
ml/train-b977cbd9c-8ct8w Pending 3s · 0 of 3 nodes fit · every check, every node

NODE                    TaintToleration  NodeAffinity  NodeResourcesFit
kwhy-pod-control-plane  ✗                ✗             ✗ cpu 0.95/32 +40
kwhy-pod-worker         ✓                ✓             ✗ cpu 0.1/32 +40
kwhy-pod-worker2        ✓                ✗             ✗ cpu 0.1/32 +40

Reasons:
  TaintToleration: node(s) had untolerated taint(s)
  NodeAffinity: node(s) didn't match Pod's node affinity/selector
  NodeResourcesFit: Insufficient cpu
```

`cpu 0.1/32 +40` is requested/allocatable on the node, plus the pod's request. It also shows what Cluster Autoscaler
or Karpenter last said about the pod. If the pod could preempt others, it runs the scheduler's own preemption as a
dry run: the node it would pick, the pods it would evict, and any PodDisruptionBudget that breaks.

**On a node but not running:** kwhy names each failing container's problem as kubelet reported it: `pull`, `crash`,
`oom`, `config` (missing Secret, ConfigMap or key), `mount`, `probe`, `evicted`, `sandbox` or `node`. It also covers
what the workload's controller reports: pods it can't create (quota, PodSecurity, webhooks), a stalled rollout, a
failed Job, or a pod stuck terminating on a finalizer. It never guesses why an app exits. `--logs N` (opt-in, logs
may hold secrets) shows the app's own words.

**Which values line caused it:** with `--chart` and the values the release was deployed with (or `--kustomize`), the
fields a failing check reads link back to their source:

```console
$ kwhy pod deploy/rel-api -n demo --chart testdata/charts/mini -f testdata/values-prod.yaml --set resources.requests.cpu=40 --release rel
...
Source:
  NodeResourcesFit  .spec.containers[0].resources.requests.cpu = 40  ← resources.requests.cpu (--set resources.requests.cpu=40)
```

## How we know it's right

- **`value`** is checked nightly against 10 public charts (`go test -tags accuracy ./value/`): it never named a wrong
  setting (precision 1.00) and found 98–100% of the settings that really change a field. Kustomize is checked
  the same way, by editing source lines and rebuilding, nightly on 12 public overlays: precision 1.00, recall
  87–100% (kubebuilder's prefixed names name only the `namePrefix` line).
- **`change`** has one fixture per rule in `testdata/change/`. CI applies base and head to kind (Kubernetes N-2..N)
  and compares the cluster with the prediction. Upgrades of ingress-nginx, argo-cd, cert-manager and grafana must
  not predict a false `will fail`.
- **`pod`** is checked on kind (Kubernetes 1.35–1.37) against the scheduler's own message, its real preemption
  pick, and what kubelet reports for pull, crash, OOM, config, mount, probe and init failures.

How long `value` takes for one field the first time (8-core M1 Pro):

| Chart | Values leaves | Time |
|---|---|---|
| podinfo, external-dns, ingress-nginx, grafana | 68–243 | 0.02–0.35s |
| postgresql, redis, cert-manager, keycloak | 189–569 | 0.9–1.7s |
| argo-cd | 758 | 3.8s |
| kube-prometheus-stack | 1247 | 21s |

How `pod` compares with other tools for Pending pods:

| | kwhy | Re-implemented checks (e.g. kubectl-why-pending) | LLM explainers (e.g. k8sgpt) | `kubectl describe` |
|---|---|---|---|---|
| Answer comes from | the scheduler's code, compiled in | a hand-written copy of the rules | a model reading events | the scheduler |
| Failing checks per node | all | depends on the tool | — | the first, summarised |
| Values line that set the field | yes, with `--chart` | no | no | no |
| Suggests fixes | no, deliberately | yes | yes | no |

## Reference

<details>
<summary><code>kwhy value chart</code> and <code>kwhy value kustomize</code> flags</summary>

`kwhy value chart <chart> <kind>/<name> [<path>]`

| Flag | Meaning |
|---|---|
| `--version V` | chart version, for repo and OCI charts |
| `-f, --values FILE` | values file or URL, repeatable, later wins |
| `--set K=V`, `--set-string`, `--set-json`, `--set-file`, `--set-literal` | as for `helm`, applied in Helm's order |
| `--release NAME`, `-n, --namespace NS` | release name (default `rel`) and namespace (default `default`) |
| `--kube-version V` | Kubernetes version charts see in `.Capabilities` (default `v1.37.0`) |
| `-o, --output` | `human` (default), `markdown` or `json` (`kwhy.value/v1`) |
| `--all` | every field of the resource instead of one path |
| `--no-cache` | trace again instead of reusing the cache (user cache dir, keyed by chart, values, options and binary) |
| `--chunk-size N` | values leaves marked per tracing render (default 10; tuning only) |
| `--repository-config`, `--repository-cache`, `--registry-config`, `--content-cache` | Helm's paths, same defaults as `helm` |

`kwhy value kustomize <overlay dir> <kind>/<name> [<path>]` takes `--all`, `-o`, `--no-cache` and `--root DIR` (the
directory holding the whole overlay stack, default the git root). For one field it also edits the line it is
about to name and rebuilds, and names the line only when the field moves; `--all` skips that rebuild and keeps the
line guard.

Exit codes: `0` ok, `1` runtime error, `2` usage error.
</details>

<details>
<summary>Answer kinds (<code>kwhy value</code>)</summary>

| Kind | Meaning | Example |
|---|---|---|
| `direct` | the field equals one values leaf | `replicas: 3` ← `replicaCount` |
| `composed` | leaves plus template text | `image: repo:tag` ← `image.repository` + `image.tag` |
| `derived` | template logic computes it from leaves | `leader-election: "true"` ← `replicaCount` |
| `encoded` | a leaf passed through `b64enc` | Secret data ← `auth.user` |
| `hashed` | checksum of other content | `checksum/config` annotation |
| `chart` | no values leaf affects it | `apiVersion`, fixed ports |
| `structural` | a leaf decides whether the field exists | `ingress.enabled` |
| `random` | differs between two identical renders | `randAlphaNum` passwords |
</details>

<details>
<summary><code>kwhy change</code> flags and verdicts</summary>

`kwhy change <base>..<head>`, run inside the git repo.

| Flag | Meaning |
|---|---|
| `--chart DIR` | chart directory in the repo |
| `--manifests DIR` | plain YAML, or a Kustomize overlay, in the repo |
| `--config FILE` | several targets: `targets: [{chart or manifests, values, set, setString, setJSON, release, namespace}]`; `.kwhy.yaml` is used when no source flag is given; its paths are relative to the repo root and may not leave it |
| `--apps DIR` | Argo CD `Application`s and Flux `HelmRelease`s under DIR, read at each ref |
| `-f`, `--set`, `--set-string`, `--set-json`, `--set-file`, `--set-literal` | as for `helm`; files in the repo are read at each ref |
| `--release NAME`, `-n, --namespace NS` | release name (default `rel`) and namespace (default `default`) |
| `--kube-version V` | Kubernetes version charts see in `.Capabilities` (default `v1.37.0`) |
| `-o, --output` | `human` (default), `markdown` (PR comment, under GitHub's 65,000 characters) or `json` (`kwhy.change/v1`) |
| `--fail-on V,...` | exit `3` if any of these verdicts occur: `fail`, `data-loss`, or any verdict with `-` for spaces |
| `--context NAME` | kube context to read live state from, read-only (default: offline) |

Verdicts: `restart` (says so when it pulls a mutable tag), `ordered`, `scale`, `resize` (PVC growth), `recreate`
(Argo `Replace`/`Force`), `runs as hook`, `no effect` / `delayed` / `picked up` for ConfigMap and Secret consumers,
`will fail` (immutable fields), `no endpoints` (a Service selector stops matching), `data` (a delete that loses
stored data), `create`, `delete`.

Exit codes: `0` ok, `1` runtime error, `2` usage error, `3` a `--fail-on` verdict occurred.
</details>

<details>
<summary><code>kwhy pod</code> flags</summary>

`kwhy pod <name|pod/x|deploy/x|sts/x|ds/x|rs/x|job/x|cj/x>`: a CronJob is followed to its newest Job.

| Flag | Meaning |
|---|---|
| `-n, --namespace NS` | namespace (default: the kube context's) |
| `--context NAME` | kube context to read from, read-only |
| `-o, --output` | `human` (default), `markdown` or `json` (`kwhy.pod/v1`, every node with all failing checks and the scheduler's first) |
| `--logs N` | last N log lines of a crashed or OOM-killed container's last run (default none) |
| `--chart CHART` | chart that deploys the pod: adds the values line behind each field a failing check reads |
| `-f`, `--set`, `--set-string`, `--set-json`, `--set-file`, `--set-literal`, `--version V` | as the release was deployed (with `--chart`) |
| `--release NAME` | Helm release name (default: the pod's `app.kubernetes.io/instance` label) |
| `--kustomize DIR`, `--root DIR` | the overlay that deploys the pod, and the directory holding its stack |
| `--no-cache` | with `--chart` or `--kustomize`: trace again |
| `--scheduler-config FILE` | the cluster's `KubeSchedulerConfiguration`: run the pod's profile instead of the default |

It reads the cluster once, with get and list only: nodes, pods, namespaces, volumes and claims, storage, CSI and DRA
objects, and PodDisruptionBudgets. A healthy pod gets one line (`ns/x is on node-1, Running and ready.`).

Exit codes: `0` ok (a diagnosis is an answer, not an error), `1` runtime error, `2` usage error.
</details>

## Known limits

- Kustomize: no remote bases or URLs (refused before anything is fetched), `helmCharts:` or plugins. `value kustomize` names no override chain, and a name built
  from `namePrefix` names only the `namePrefix` line, not the resource's own `name:` line. With `--all`, a field a
  `replacements:` entry writes with the value the resource's own line already has can name that resource line;
  ask for the field alone to have it confirmed by a rebuild.
- `lookup` returns empty, as with `helm template`.
- `change --apps` skips apps from other repos, Helm repositories and Flux `valuesFrom`. Offline, DaemonSet pod
  counts are unknown.
- `change` classifies Deployments, StatefulSets, DaemonSets, Jobs, CronJobs, bare Pods, Services, PVCs, ConfigMaps
  and Secrets.
- `pod` runs the scheduler compiled into kwhy (v1.37) and warns when the cluster is more than one minor version
  away. Out-of-tree scheduler plugins can't run. On clusters with over 100 preemption candidates the scheduler
  samples nodes, so it may pick another than the one kwhy names (kwhy says so).
- `pod --chart` links only fields a values leaf sets, not ones hardcoded in a template.

What's next is in the [backlog](docs/plans/backlog.md).

## Development

```sh
make build        # ./kwhy, version from git
make run ARGS="pod deploy/api -n shop"
make check        # what CI runs: gofmt, vet, race tests
make accuracy     # public charts and overlays (scripts/fetch-charts.sh, scripts/fetch-overlays.sh first)
make kind-test    # change and pod against kind clusters it creates; make kind-down removes them
```

`make` alone lists every target.

```
cmd/kwhy/    CLI wiring (cobra): flags in, features out
render/      Helm rendering, plain manifests and Kustomize builds, shared by value and change
value/       values leaves, override chains, tracing, cache; Kustomize leave-one-out attribution
change/      base/head render, targets (.kwhy.yaml, Argo CD, Flux), impact rules, live refinement, report
pod/         cluster snapshot, every filter per node, pod and controller diagnosis, report;
             the only package importing k8s.io/kubernetes
docs/        specs, plans/backlog.md (what's next), spikes
```

Design: [overview](docs/specs/00-overview.md) · [value](docs/specs/01-value.md) ·
[change](docs/specs/02-change.md) · [pod](docs/specs/03-pod.md)

## License

[Apache-2.0](LICENSE)
