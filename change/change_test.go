package change_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"

	"github.com/semirm-dev/kwhy/change"
	"github.com/semirm-dev/kwhy/render"
)

const (
	envCM = `
kind: ConfigMap
metadata: {name: cfg}
data: {MODE: %q}
`
	envUser = `
kind: Deployment
metadata: {name: api}
spec:
  replicas: 2
  template:
    metadata: {annotations: {sum: %q}}
    spec:
      containers: [{name: c, image: app:1, envFrom: [{configMapRef: {name: cfg}}], env: [{name: X, valueFrom: {configMapKeyRef: {name: cfg, key: MODE}}}]}]
`
	volUser = `
kind: Deployment
metadata: {name: files}
spec:
  template:
    spec:
      containers: [{name: c, image: app:1, volumeMounts: [{name: v, mountPath: /cfg%s}]}]
      volumes: [{name: v, configMap: {name: cfg}}]
`
	sts = `
kind: StatefulSet
metadata: {name: db, namespace: prod%s}
spec:
  replicas: 3
  serviceName: %s
  template: {spec: {containers: [{name: c, image: %s}]}}
`
	cmUsers = `
kind: CronJob
metadata: {name: backup}
spec: {jobTemplate: {spec: {template: {spec: {containers: [{name: c, envFrom: [{configMapRef: {name: cfg}}]}]}}}}}
---
kind: Job
metadata: {name: once}
spec: {template: {spec: {containers: [{name: c, envFrom: [{configMapRef: {name: cfg}}]}]}}}
---
kind: Pod
metadata: {name: debug}
spec: {containers: [{name: c, envFrom: [{configMapRef: {name: cfg}}]}]}
`
	job = `
kind: Job
metadata: {name: migrate%s}
spec:
  template: {spec: {containers: [{name: c, image: %s}]}}
`
)

func TestDiff(t *testing.T) {
	tests := []struct {
		name       string
		base, head string
		want       []string
	}{
		{
			name: "image change restarts deployment",
			base: "kind: Deployment\nmetadata: {name: api}\nspec: {replicas: 12, template: {spec: {containers: [{name: c, image: app:1}]}}}",
			head: "kind: Deployment\nmetadata: {name: api}\nspec: {replicas: 12, template: {spec: {containers: [{name: c, image: app:2}]}}}",
			want: []string{"restart|Deployment default/api|12|image"},
		},
		{
			name: "other template change restarts with default one replica",
			base: "kind: Deployment\nmetadata: {name: api}\nspec: {template: {metadata: {labels: {a: '1'}}}}",
			head: "kind: Deployment\nmetadata: {name: api}\nspec: {template: {metadata: {labels: {a: '2'}}}}",
			want: []string{"restart|Deployment default/api|1|pod template"},
		},
		{
			name: "statefulset rolls in order",
			base: fmt.Sprintf(sts, "", "db", "pg:16"),
			head: fmt.Sprintf(sts, "", "db", "pg:17"),
			want: []string{"ordered|StatefulSet prod/db|3|one at a time"},
		},
		{
			name: "statefulset partition rolls only ordinals above it",
			base: "kind: StatefulSet\nmetadata: {name: db}\nspec: {replicas: 3, updateStrategy: {rollingUpdate: {partition: 2}}, template: {spec: {containers: [{name: c, image: pg:16}]}}}",
			head: "kind: StatefulSet\nmetadata: {name: db}\nspec: {replicas: 3, updateStrategy: {rollingUpdate: {partition: 2}}, template: {spec: {containers: [{name: c, image: pg:17}]}}}",
			want: []string{"ordered|StatefulSet default/db|1|one at a time, ordinals 2 and up"},
		},
		{
			name: "statefulset partition at replicas updates nothing",
			base: "kind: StatefulSet\nmetadata: {name: db}\nspec: {replicas: 3, updateStrategy: {rollingUpdate: {partition: 3}}, template: {spec: {containers: [{name: c, image: pg:16}]}}}",
			head: "kind: StatefulSet\nmetadata: {name: db}\nspec: {replicas: 3, updateStrategy: {rollingUpdate: {partition: 3}}, template: {spec: {containers: [{name: c, image: pg:17}]}}}",
			want: []string{"no effect|StatefulSet default/db|0|partition 3: no pod is updated"},
		},
		{
			name: "statefulset OnDelete restarts nothing",
			base: "kind: StatefulSet\nmetadata: {name: db}\nspec: {updateStrategy: {type: OnDelete}, template: {spec: {containers: [{name: c, image: pg:16}]}}}",
			head: "kind: StatefulSet\nmetadata: {name: db}\nspec: {updateStrategy: {type: OnDelete}, template: {spec: {containers: [{name: c, image: pg:17}]}}}",
			want: []string{"no effect|StatefulSet default/db|0|OnDelete: pods keep the old template until someone deletes them"},
		},
		{
			name: "daemonset OnDelete restarts nothing",
			base: "kind: DaemonSet\nmetadata: {name: agent}\nspec: {updateStrategy: {type: OnDelete}, template: {spec: {containers: [{name: c, image: a:1}]}}}",
			head: "kind: DaemonSet\nmetadata: {name: agent}\nspec: {updateStrategy: {type: OnDelete}, template: {spec: {containers: [{name: c, image: a:2}]}}}",
			want: []string{"no effect|DaemonSet default/agent|0|OnDelete: pods keep the old template until someone deletes them"},
		},
		{
			name: "OnDelete consumer keeps old config",
			base: fmt.Sprintf(envCM, "v1") + "---" + strings.Replace(fmt.Sprintf(envUser, "1"), "kind: Deployment", "kind: DaemonSet", 1) +
				"  updateStrategy: {type: OnDelete}\n",
			head: fmt.Sprintf(envCM, "v2") + "---" + strings.Replace(fmt.Sprintf(envUser, "2"), "kind: Deployment", "kind: DaemonSet", 1) +
				"  updateStrategy: {type: OnDelete}\n",
			want: []string{
				"no effect|ConfigMap default/cfg → DaemonSet default/api|0|env var: running pods keep the OLD value until they restart",
				"no effect|DaemonSet default/api|0|OnDelete: pods keep the old template until someone deletes them",
			},
		},
		{
			name: "daemonset restarts one pod per node",
			base: "kind: DaemonSet\nmetadata: {name: agent}\nspec: {template: {spec: {containers: [{name: c, image: a:1}]}}}",
			head: "kind: DaemonSet\nmetadata: {name: agent}\nspec: {template: {spec: {containers: [{name: c, image: a:2}]}}}",
			want: []string{"restart|DaemonSet default/agent|0|one pod per node"},
		},
		{
			name: "replicas only scales",
			base: "kind: Deployment\nmetadata: {name: api}\nspec: {replicas: 1}",
			head: "kind: Deployment\nmetadata: {name: api}\nspec: {replicas: 2}",
			want: []string{"scale|Deployment default/api|0|1 → 2"},
		},
		{
			name: "statefulset immutable field fails",
			base: fmt.Sprintf(sts, "", "cache-v1", "pg:16"),
			head: fmt.Sprintf(sts, "", "cache-v2", "pg:16"),
			want: []string{"will fail|StatefulSet prod/db|0|spec.serviceName is immutable"},
		},
		{
			name: "job template change fails",
			base: fmt.Sprintf(job, "", "m:1"),
			head: fmt.Sprintf(job, "", "m:2"),
			want: []string{"will fail|Job default/migrate|0|spec.template is immutable"},
		},
		{
			name: "hook job runs as hook",
			base: fmt.Sprintf(job, ", annotations: {helm.sh/hook: pre-upgrade}", "m:1"),
			head: fmt.Sprintf(job, ", annotations: {helm.sh/hook: pre-upgrade}", "m:2"),
			want: []string{"runs as hook|Job default/migrate|0|"},
		},
		{
			name: "argo hook runs as hook",
			base: fmt.Sprintf(job, ", annotations: {argocd.argoproj.io/hook: PreSync}", "m:1"),
			head: fmt.Sprintf(job, ", annotations: {argocd.argoproj.io/hook: PreSync}", "m:2"),
			want: []string{"runs as hook|Job default/migrate|0|"},
		},
		{
			name: "argo replace recreates instead of failing",
			base: fmt.Sprintf(sts, ", annotations: {argocd.argoproj.io/sync-options: 'Replace=true'}", "cache-v1", "pg:16"),
			head: fmt.Sprintf(sts, ", annotations: {argocd.argoproj.io/sync-options: 'Prune=false,Replace=true'}", "cache-v2", "pg:16"),
			want: []string{"recreate|StatefulSet prod/db|3|spec.serviceName is immutable, Argo replaces it"},
		},
		{
			name: "deployment selector fails",
			base: "kind: Deployment\nmetadata: {name: api}\nspec: {selector: {matchLabels: {a: '1'}}}",
			head: "kind: Deployment\nmetadata: {name: api}\nspec: {selector: {matchLabels: {a: '2'}}}",
			want: []string{"will fail|Deployment default/api|0|spec.selector is immutable"},
		},
		{
			name: "service clusterIP fails",
			base: "kind: Service\nmetadata: {name: api}\nspec: {clusterIP: None}",
			head: "kind: Service\nmetadata: {name: api}\nspec: {clusterIP: ''}",
			want: []string{"will fail|Service default/api|0|spec.clusterIP is immutable"},
		},
		{
			name: "immutable configmap data fails",
			base: "kind: ConfigMap\nmetadata: {name: cfg}\nimmutable: true\ndata: {a: '1'}",
			head: "kind: ConfigMap\nmetadata: {name: cfg}\nimmutable: true\ndata: {a: '2'}",
			want: []string{"will fail|ConfigMap default/cfg|0|immutable ConfigMap data changed"},
		},
		{
			name: "env consumer keeps old value, once per consumer",
			base: fmt.Sprintf(envCM, "v1") + "---" + fmt.Sprintf(envUser, "1"),
			head: fmt.Sprintf(envCM, "v2") + "---" + fmt.Sprintf(envUser, "1"),
			want: []string{"no effect|ConfigMap default/cfg → Deployment default/api|0|env var: running pods keep the OLD value until they restart"},
		},
		{
			name: "restarted consumer picks it up",
			base: fmt.Sprintf(envCM, "v1") + "---" + fmt.Sprintf(envUser, "1"),
			head: fmt.Sprintf(envCM, "v2") + "---" + fmt.Sprintf(envUser, "2"),
			want: []string{
				"restart|Deployment default/api|2|pod template",
				"picked up|ConfigMap default/cfg → Deployment default/api|0|pods restart",
			},
		},
		{
			name: "volume consumer updates later",
			base: fmt.Sprintf(envCM, "v1") + "---" + fmt.Sprintf(volUser, ""),
			head: fmt.Sprintf(envCM, "v2") + "---" + fmt.Sprintf(volUser, ""),
			want: []string{"delayed|ConfigMap default/cfg → Deployment default/files|0|volume file updates in ~1m, the app must re-read it"},
		},
		{
			name: "subPath consumer never updates",
			base: fmt.Sprintf(envCM, "v1") + "---" + fmt.Sprintf(volUser, "/m, subPath: MODE"),
			head: fmt.Sprintf(envCM, "v2") + "---" + fmt.Sprintf(volUser, "/m, subPath: MODE"),
			want: []string{"no effect|ConfigMap default/cfg → Deployment default/files|0|subPath mount: never updates until pods restart"},
		},
		{
			name: "deleted pvc loses data",
			base: "kind: PersistentVolumeClaim\nmetadata: {name: data}",
			head: "",
			want: []string{"data|PersistentVolumeClaim default/data|0|deleted"},
		},
		{
			name: "deleted statefulset with delete retention loses data",
			base: "kind: StatefulSet\nmetadata: {name: db}\nspec: {persistentVolumeClaimRetentionPolicy: {whenDeleted: Delete}}",
			head: "",
			want: []string{"data|StatefulSet default/db|0|deleted, and its PVCs with it"},
		},
		{
			name: "deleted crd loses its resources",
			base: "kind: CustomResourceDefinition\nmetadata: {name: widgets.acme.io}",
			head: "",
			want: []string{"data|CustomResourceDefinition widgets.acme.io|0|deleted, and every custom resource of it"},
		},
		{
			name: "plain create and delete",
			base: "kind: Service\nmetadata: {name: old}",
			head: "kind: Service\nmetadata: {name: new}",
			want: []string{"create|Service default/new|0|", "delete|Service default/old|0|"},
		},
		{
			name: "pvc grows if the storage class allows",
			base: fmt.Sprintf("kind: PersistentVolumeClaim\nmetadata: {name: data}\nspec: {accessModes: [%s], resources: {requests: {storage: %s}}}", "ReadWriteOnce", "1Gi"),
			head: fmt.Sprintf("kind: PersistentVolumeClaim\nmetadata: {name: data}\nspec: {accessModes: [%s], resources: {requests: {storage: %s}}}", "ReadWriteOnce", "2048Mi"),
			want: []string{"resize|PersistentVolumeClaim default/data|0|1Gi → 2Gi, if the StorageClass allows expansion"},
		},
		{
			name: "pvc shrink fails",
			base: fmt.Sprintf("kind: PersistentVolumeClaim\nmetadata: {name: data}\nspec: {accessModes: [%s], resources: {requests: {storage: %s}}}", "ReadWriteOnce", "1Gi"),
			head: fmt.Sprintf("kind: PersistentVolumeClaim\nmetadata: {name: data}\nspec: {accessModes: [%s], resources: {requests: {storage: %s}}}", "ReadWriteOnce", "500Mi"),
			want: []string{"will fail|PersistentVolumeClaim default/data|0|spec.resources.requests.storage can't shrink"},
		},
		{
			name: "pvc access modes fail",
			base: fmt.Sprintf("kind: PersistentVolumeClaim\nmetadata: {name: data}\nspec: {accessModes: [%s], resources: {requests: {storage: %s}}}", "ReadWriteOnce", "1Gi"),
			head: fmt.Sprintf("kind: PersistentVolumeClaim\nmetadata: {name: data}\nspec: {accessModes: [%s], resources: {requests: {storage: %s}}}", "ReadWriteMany", "1Gi"),
			want: []string{"will fail|PersistentVolumeClaim default/data|0|spec.accessModes is immutable"},
		},
		{
			name: "service to ExternalName is accepted",
			base: "kind: Service\nmetadata: {name: api}\nspec: {ports: [{port: 80}]}",
			head: "kind: Service\nmetadata: {name: api}\nspec: {type: ExternalName, externalName: example.com}",
			want: nil,
		},
		{
			name: "restart warns about mutable tags",
			base: "kind: Deployment\nmetadata: {name: api}\nspec: {template: {spec: {containers: [{name: a, image: app:1}, {name: b, image: 'reg:5000/side'}, {name: c, image: 'x:latest@sha256:ab'}]}}}",
			head: "kind: Deployment\nmetadata: {name: api}\nspec: {template: {spec: {containers: [{name: a, image: app:2}, {name: b, image: 'reg:5000/side'}, {name: c, image: 'x:latest@sha256:ab'}]}}}",
			want: []string{"restart|Deployment default/api|1|image, pulls mutable reg:5000/side"},
		},
		{
			name: "cronjob template change waits for the next run",
			base: "kind: CronJob\nmetadata: {name: backup}\nspec: {schedule: '0 * * * *', jobTemplate: {spec: {template: {spec: {containers: [{name: c, image: b:1}]}}}}}",
			head: "kind: CronJob\nmetadata: {name: backup}\nspec: {schedule: '0 3 * * *', jobTemplate: {spec: {template: {spec: {containers: [{name: c, image: b:2}]}}}}}",
			want: []string{"delayed|CronJob default/backup|0|schedule 0 * * * * → 0 3 * * *, job template: the next run picks it up, a running Job keeps the old one"},
		},
		{
			name: "cronjob other change reports nothing",
			base: "kind: CronJob\nmetadata: {name: backup}\nspec: {schedule: '0 * * * *', successfulJobsHistoryLimit: 3}",
			head: "kind: CronJob\nmetadata: {name: backup}\nspec: {schedule: '0 * * * *', successfulJobsHistoryLimit: 1}",
			want: nil,
		},
		{
			name: "bare pod image restarts in place",
			base: "kind: Pod\nmetadata: {name: debug}\nspec: {tolerations: [{key: a}], containers: [{name: c, image: t:1}]}",
			head: "kind: Pod\nmetadata: {name: debug}\nspec: {tolerations: [{key: b}, {key: a}], activeDeadlineSeconds: 60, containers: [{name: c, image: t:2}]}",
			want: []string{"restart|Pod default/debug|1|image: containers restart in place"},
		},
		{
			name: "bare pod other change fails",
			base: "kind: Pod\nmetadata: {name: debug}\nspec: {tolerations: [{key: a}], containers: [{name: c, image: t:1, args: [x]}]}",
			head: "kind: Pod\nmetadata: {name: debug}\nspec: {tolerations: [{key: b}], containers: [{name: c, image: t:2, args: [y]}]}",
			want: []string{"will fail|Pod default/debug|0|spec.containers, spec.tolerations is immutable"},
		},
		{
			name: "bare pod deadline can't be raised",
			base: "kind: Pod\nmetadata: {name: debug}\nspec: {activeDeadlineSeconds: 60, containers: [{name: c, image: t:1}]}",
			head: "kind: Pod\nmetadata: {name: debug}\nspec: {activeDeadlineSeconds: 120, containers: [{name: c, image: t:1}]}",
			want: []string{"will fail|Pod default/debug|0|spec.activeDeadlineSeconds is immutable"},
		},
		{
			name: "bare pod label change reports nothing",
			base: "kind: Pod\nmetadata: {name: debug, labels: {a: '1'}}\nspec: {containers: [{name: c, image: t:1}]}",
			head: "kind: Pod\nmetadata: {name: debug, labels: {a: '2'}}\nspec: {containers: [{name: c, image: t:1}]}",
			want: nil,
		},
		{
			name: "cronjob, job and pod consume a configmap",
			base: fmt.Sprintf(envCM, "v1") + "---" + cmUsers,
			head: fmt.Sprintf(envCM, "v2") + "---" + cmUsers,
			want: []string{
				"no effect|ConfigMap default/cfg → Job default/once|0|env var: running pods keep the OLD value until they restart",
				"no effect|ConfigMap default/cfg → Pod default/debug|0|env var: running pods keep the OLD value until they restart",
				"picked up|ConfigMap default/cfg → CronJob default/backup|0|the next run reads it",
			},
		},
		{
			name: "unchanged reports nothing",
			base: fmt.Sprintf(envCM, "v1"),
			head: fmt.Sprintf(envCM, "v1"),
			want: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			for _, i := range change.Diff(docs(t, tt.base), docs(t, tt.head)) {
				got = append(got, fmt.Sprintf("%s|%s|%d|%s", i.Verdict, i.Resource, i.Pods, i.Detail))
			}

			if !slices.Equal(got, tt.want) {
				t.Errorf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(tt.want, "\n"))
			}
		})
	}
}

func TestWriteSummary(t *testing.T) {
	is := []change.Impact{
		{Verdict: change.Restart, Resource: "Deployment prod/checkout", Pods: 12, Detail: "image"},
		{Verdict: change.Ordered, Resource: "StatefulSet prod/postgres", Pods: 3, Detail: "one at a time"},
		{Verdict: change.WillFail, Resource: "StatefulSet prod/cache", Detail: "spec.serviceName is immutable"},
	}

	for name, write := range map[string]func(io.Writer, []change.Impact) error{
		"human": change.WriteHuman, "markdown": change.WriteMarkdown,
	} {
		t.Run(name, func(t *testing.T) {
			var b bytes.Buffer
			if err := write(&b, is); err != nil {
				t.Fatal(err)
			}

			out := b.String()
			for _, want := range []string{"Deployment prod/checkout", "12 pods", "15 pods restarted · 1 apply error"} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q in\n%s", want, out)
				}
			}
		})
	}
}

func TestWriteNothing(t *testing.T) {
	var b bytes.Buffer
	if err := change.WriteHuman(&b, nil); err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(b.String(), "No effect on running workloads") {
		t.Errorf("got %q", b.String())
	}
}

// docs decodes a multi-document YAML string the way render does: namespace defaults to "default".
func docs(t *testing.T, s string) []render.Doc {
	t.Helper()

	var out []render.Doc
	for part := range strings.SplitSeq(s, "---") {
		var obj map[string]any
		if err := yaml.Unmarshal([]byte(part), &obj); err != nil {
			t.Fatal(err)
		}
		if obj == nil {
			continue
		}

		md := obj["metadata"].(map[string]any)
		d := render.Doc{Object: obj, Namespace: "default"}
		d.Kind, _ = obj["kind"].(string)
		d.Name, _ = md["name"].(string)
		if ns, ok := md["namespace"].(string); ok {
			d.Namespace = ns
		}
		out = append(out, d)
	}

	return out
}

func TestWriteJSON(t *testing.T) {
	var b bytes.Buffer
	if err := change.WriteJSON(&b, []change.Impact{{Verdict: change.Restart, Resource: "Deployment prod/api", Pods: 2, Detail: "image"}}); err != nil {
		t.Fatal(err)
	}

	var got struct {
		Schema  string
		Impacts []map[string]any
	}
	if err := json.Unmarshal(b.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Schema != "kwhy.change/v1" || len(got.Impacts) != 1 || got.Impacts[0]["verdict"] != "restart" || got.Impacts[0]["pods"] != 2.0 {
		t.Errorf("got %s", b.String())
	}
}

func TestWriteMarkdownLimit(t *testing.T) {
	is := []change.Impact{{Verdict: change.WillFail, Resource: "Job prod/migrate", Detail: "spec.template is immutable"}}
	for n := range 3000 {
		is = append(is, change.Impact{Verdict: change.Restart, Resource: fmt.Sprintf("Deployment prod/app-%04d", n), Pods: 1, Detail: "image"})
	}

	var b bytes.Buffer
	if err := change.WriteMarkdown(&b, is); err != nil {
		t.Fatal(err)
	}

	out := b.String()
	if n := utf8.RuneCountInString(out); n > 65000 {
		t.Errorf("%d characters, over the comment limit", n)
	}
	for _, want := range []string{"`Job prod/migrate`", "3000 pods restarted · 1 apply error", "<details><summary>3000 more changes</summary>",
		"`Deployment prod/app-0000`", "more, run `kwhy change` locally", "</details>"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
	if i, j := strings.Index(out, "Job prod/migrate"), strings.Index(out, "<details>"); i > j {
		t.Error("failure not shown above the collapsed list")
	}
}

// Failures alone past the limit are cut too: GitHub refuses a longer comment.
func TestWriteMarkdownLimitKeptRows(t *testing.T) {
	var is []change.Impact
	for n := range 3000 {
		is = append(is, change.Impact{Verdict: change.WillFail, Resource: fmt.Sprintf("Job prod/migrate-%04d", n), Detail: "spec.template is immutable"})
	}
	is = append(is, change.Impact{Verdict: change.Restart, Resource: "Deployment prod/api", Pods: 1, Detail: "image"})

	var b bytes.Buffer
	if err := change.WriteMarkdown(&b, is); err != nil {
		t.Fatal(err)
	}

	out := b.String()
	if n := utf8.RuneCountInString(out); n > 65000 {
		t.Errorf("%d characters, over the comment limit", n)
	}
	for _, want := range []string{"`Job prod/migrate-0000`", "3000 apply errors", "more, run `kwhy change` locally",
		"<details><summary>1 more change</summary>", "</details>"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestDiffServiceSelector(t *testing.T) {
	const app = `
kind: Deployment
metadata: {name: api}
spec: {template: {metadata: {labels: {app: %s, tier: web}}, spec: {containers: [{name: c, image: app:1}]}}}
---
kind: Service
metadata: {name: api}
spec: {selector: {app: %s}, ports: [{port: 80}]}
`
	for _, tc := range []struct {
		name       string
		base, head string
		want       []string
	}{
		{"selector renamed", fmt.Sprintf(app, "api", "api"), fmt.Sprintf(app, "api", "api-v2"),
			[]string{"no endpoints Service default/api selector app=api-v2 matches no pods"}},
		{"pod labels renamed", fmt.Sprintf(app, "api", "api"), fmt.Sprintf(app, "api-v2", "api"),
			[]string{"restart Deployment default/api", "no endpoints Service default/api selector app=api matches no pods"}},
		{"pods from outside the render", fmt.Sprintf(app, "x", "other"), fmt.Sprintf(app, "x", "other2"), nil},
		{"still selected", fmt.Sprintf(app, "api", "api"), fmt.Sprintf(app, "api", "api") + "\n---\nkind: ConfigMap\nmetadata: {name: c}\n", []string{"create ConfigMap default/c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			for _, i := range change.Diff(docs(t, tc.base), docs(t, tc.head)) {
				got = append(got, strings.TrimSpace(string(i.Verdict)+" "+i.Resource+" "+i.Detail))
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
			for i, w := range tc.want {
				if !strings.HasPrefix(got[i], w) {
					t.Errorf("impact %d = %q, want prefix %q", i, got[i], w)
				}
			}
		})
	}
}
