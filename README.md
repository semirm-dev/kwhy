# kwhy

Answers the "why" questions about Kubernetes config, using the real Helm and scheduler code instead of guesses.

| Command | Question | Works on | Status |
|---|---|---|---|
| `kwhy value` | Where does this rendered value come from, and how do I change it safely? | Git repo, offline | v0.1, in progress |
| `kwhy change` | What will merging this change actually do to running workloads? | Git repo (+ optional cluster) | planned, v0.3 |
| `kwhy pod` | Why is this pod not running, and what is the smallest fix? | Live cluster, read-only | planned, v0.4 |

> **Status:** early development. The CLI parses and validates its flags but no command prints answers yet.
> The examples below show the target output from the [specs](docs/specs), not the output of today's build.

## `kwhy value`: where does this value come from?

A rendered field can come from chart defaults, several `-f` files, `--set` flags or template logic.
`kwhy value` points at the exact line that wins, and at everything it overrides.

```console
$ kwhy value deploy/checkout '.spec.template.spec.containers[name=api].resources.requests.memory' \
    --chart ./charts/checkout -f values.yaml -f values-prod.yaml

2Gi  values-prod.yaml:31   checkout.resources.memory
     ├─ overrides  values.yaml:12              = 1Gi
     └─ default    charts/svc/values.yaml:40   = 512Mi
     template      charts/svc/templates/deployment.yaml
```

When a field isn't a plain copy of one value, the answer says so instead of guessing:

```console
$ kwhy value deploy/api '.spec.template.spec.containers[name=api].env[name=LEADER_ELECTION].value' ...

true  derived  ← replicaCount (values-prod.yaml:1)
```

Every answer has one of these kinds, in the human output and in `-o json` (`"schema": "kwhy.value/v1"`):

| Kind | Meaning | Example |
|---|---|---|
| `direct` | the field equals one values leaf | `replicas: 3` ← `replicaCount` |
| `composed` | one or more leaves plus template text | `image: repo:tag` ← `image.repository` + `image.tag` |
| `derived` | template logic computes it from leaves | `leader-election: "true"` ← `replicaCount` |
| `encoded` | a leaf passed through `b64enc` | Secret data ← `auth.user` |
| `hashed` | checksum of other content | `checksum/config` annotation |
| `chart` | no values leaf affects it | `apiVersion`, fixed ports |
| `structural` | a leaf decides whether the field exists | `ingress.enabled` |
| `random` | differs between two identical renders | `randAlphaNum` passwords |

Other forms:

```console
$ kwhy value deploy/api --all ...                 # every field of the resource with its source
$ kwhy value deploy/api --key resources ...       # reverse: which rendered fields this values key affects
$ kwhy value deploy/api <path> ... -o markdown    # for PR comments; -o json for CI and agents
```

### What works today

The engine underneath is in place: in-process Helm rendering (`render/`) and the override chain of every
values leaf (`value/`). For the fixture chart in `testdata/` with
`-f values-prod.yaml -f values-anchors.yaml --set image.tag=9.9`, it already knows:

```
replicaCount               3      values-prod.yaml:1=3        mini/values.yaml:1=1
resources.requests.memory  4Gi    values-anchors.yaml:2=4Gi   values-prod.yaml:4=2Gi   mini/values.yaml:7=512Mi
image.tag                  9.9    --set:0=9.9                 mini/values.yaml:4=
sub.greeting               hi     values-prod.yaml:8=hi       mini/charts/sub/values.yaml:1=hello
global.env                 prod   values-prod.yaml:10=prod    mini/values.yaml:17=dev
```

It follows Helm's own merge rules: later files win, maps merge, lists replace whole, `null` deletes,
subchart defaults mount under their name or alias, and YAML anchors point at the line where the value is written.

Next up: matching rendered fields to these leaves (marker tracing), then the answer kinds and output.
Progress is tracked in the [v0.1 plan](docs/plans/2026-11-18-v0.1-value.md).

## `kwhy change`: what will this PR do? (planned, v0.3)

Diff tools show the full rendered diff. `kwhy change` says what the diff *does*, as one short PR comment:

```console
$ kwhy change main..feature -o markdown

Merging this PR will:
↻ restart    Deployment prod/checkout     12 pods   image tag
⏱ ordered    StatefulSet prod/postgres     3 pods   one at a time, ~9m
! no effect  ConfigMap prod/api-config → Deployment prod/api
             env var: running pods keep the OLD value until they restart
✗ will fail  StatefulSet prod/cache  spec.serviceName is immutable
⚠ data       PersistentVolumeClaim prod/data-postgres-0 deleted
45 pods restarted · est. 11 min · 1 apply error
```

## `kwhy pod`: why is it Pending? (planned, v0.4)

Runs the real kube-scheduler filters on every node, not just the first failing check the scheduler reports,
and tries the smallest change that makes the pod fit:

```console
$ kwhy pod deploy/trainer --what-if

Pending 14m · 0 of 12 nodes fit · every check, every node

NODE       CPU            MEMORY          TAINTS          AFFINITY  VOLUME
gpu-a1     ✗ 7.5/8 +2     ✓               ✓               ✓         ✓
gpu-a2     ✓              ✗ 61/64 +16Gi   ✓               ✗ zone b  ✓
cpu-* (9)  ✓              ✓               ✗ nvidia.com    ✗ gpu     ✓

Smallest fixes (re-ran the filters on a modified copy)
 → lower cpu request 2 → 1                 fits gpu-a1
   set at values-prod.yaml:31 (kwhy value) — run with --set-value to change it
```

## Install

```sh
go install github.com/semirm-dev/kwhy/cmd/kwhy@latest
```

Single static binary, offline by default, no telemetry. Release binaries, Homebrew (v0.2) and Krew as
`kubectl kwhy` (v0.4) come later.

## Flags (`kwhy value`)

| Flag | Meaning |
|---|---|
| `--chart REF` | chart directory, `.tgz`, `oci://` reference or `repo/chart` (required) |
| `--version V` | chart version, for repo and OCI charts |
| `-f, --values FILE` | values file, repeatable, later wins |
| `--set K=V` | set a value, repeatable, wins over files |
| `--release NAME`, `-n, --namespace NS` | release name (default `rel`) and namespace (default `default`) |
| `-o, --output` | `human` (default), `markdown` or `json` |
| `--all`, `--key PATH` | every field of the resource, or reverse lookup from a values key |
| `--repository-config`, `--repository-cache`, `--registry-config`, `--content-cache` | Helm's paths, same defaults as `helm` |

Exit codes: `0` ok, `1` runtime error, `2` usage error.

## Known limits

- `lookup` returns empty, as with `helm template` (offline).
- Kustomize tracing and `--set-value` write-back come in v0.2.
- `-f` takes local files only for now (Helm also accepts URLs).

## Development

```sh
gofmt -l .            # must print nothing
go vet ./...
go test -race ./...
```

```
cmd/kwhy/    CLI wiring (cobra): flags in, features out
render/      Helm rendering in-process, shared by value and change
value/       values leaves, override chains, tracing (v0.1)
change/      v0.3
pod/         v0.4, the only package importing k8s.io/kubernetes
docs/        specs, v0.1 plan, spikes
```

Design: [overview](docs/specs/00-overview.md) · [value](docs/specs/01-value.md) ·
[change](docs/specs/02-change.md) · [pod](docs/specs/03-pod.md)

## License

Apache-2.0
