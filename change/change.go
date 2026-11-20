// Package change explains what merging a change does to running workloads (v0.3, docs/specs/02-change.md).
// It compares the rendered objects of two git refs; it never applies or proposes anything.
package change

import (
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/semirm-dev/kwhy/render"
)

// Verdicts, in the order a report lists them.
const (
	Restart  Verdict = "restart"
	Ordered  Verdict = "ordered"
	Scale    Verdict = "scale"
	Resize   Verdict = "resize"
	Recreate Verdict = "recreate"
	Hook     Verdict = "runs as hook"
	NoEffect Verdict = "no effect"
	Delayed  Verdict = "delayed"
	PickedUp Verdict = "picked up"
	WillFail Verdict = "will fail"
	Orphaned Verdict = "no endpoints"
	Data     Verdict = "data"
	Create   Verdict = "create"
	Delete   Verdict = "delete"
)

var (
	order = []Verdict{Restart, Ordered, Scale, Resize, Recreate, Hook, NoEffect, Delayed, PickedUp, WillFail, Orphaned, Data, Create, Delete}

	workloads = map[string]bool{"Deployment": true, "StatefulSet": true, "DaemonSet": true}

	// mutable lists the spec fields the API server lets change; any other spec field is rejected.
	mutable = map[string]map[string]bool{
		"StatefulSet": {"replicas": true, "template": true, "updateStrategy": true, "persistentVolumeClaimRetentionPolicy": true,
			"minReadySeconds": true, "ordinals": true, "revisionHistoryLimit": true},
		"Job": {"parallelism": true, "completions": true, "activeDeadlineSeconds": true, "suspend": true, "backoffLimit": true,
			"ttlSecondsAfterFinished": true, "backoffLimitPerIndex": true, "maxFailedIndexes": true, "podFailurePolicy": true,
			"successPolicy": true},
		"PersistentVolumeClaim": {"resources": true, "volumeAttributesClassName": true},
	}

	clusterScoped = map[string]bool{"Namespace": true, "CustomResourceDefinition": true, "ClusterRole": true,
		"ClusterRoleBinding": true, "StorageClass": true, "PersistentVolume": true, "IngressClass": true, "PriorityClass": true,
		"MutatingWebhookConfiguration": true, "ValidatingWebhookConfiguration": true}
)

// Verdict says what applying the change does to one object.
type Verdict string

// Impact is one line of the report.
type Impact struct {
	Verdict  Verdict `json:"verdict"`
	Resource string  `json:"resource"`       // "Deployment prod/api", or "ConfigMap prod/cfg → Deployment prod/api" for a consumer
	Pods     int     `json:"pods,omitempty"` // pods restarted, for restart, ordered and recreate
	Detail   string  `json:"detail,omitempty"`
	Seconds  int     `json:"estimateSeconds,omitempty"` // estimated rollout time, with --context

	doc render.Doc // the head object, for Live
}

type obj = map[string]any

// Diff classifies what applying head over base does. Both sides are rendered, undefaulted manifests.
func Diff(base, head []render.Doc) []Impact {
	b, h := index(base), index(head)
	var out []Impact
	restarted := map[string]bool{}

	for k, hd := range h {
		bd, ok := b[k]
		if !ok {
			out = append(out, Impact{Verdict: Create, Resource: name(hd)})
			continue
		}
		if reflect.DeepEqual(bd.Object, hd.Object) {
			continue
		}

		if i, ok := classify(bd, hd); ok {
			out = append(out, i)
			if i.Verdict == Restart || i.Verdict == Ordered || i.Verdict == Recreate {
				restarted[k] = true
			}
		}
	}

	for k, hd := range h {
		if bd, ok := b[k]; ok && (hd.Kind == "ConfigMap" || hd.Kind == "Secret") && !sameData(bd.Object, hd.Object) && !immutable(bd.Object) {
			out = append(out, consumers(hd, h, restarted)...)
		}
	}

	for k, bd := range b {
		if _, ok := h[k]; !ok {
			out = append(out, deleted(bd))
		}
	}

	// A Service that selected pods rendered here before, and selects none after: its traffic stops. Pods from
	// outside the render are unknown, so a Service that selected none before is never flagged.
	for k, hd := range h {
		bd, ok := b[k]
		sel, _ := get(hd.Object, "spec", "selector").(obj)
		if !ok || hd.Kind != "Service" || len(sel) == 0 || !selects(bd, base) || selects(hd, head) {
			continue
		}
		out = append(out, Impact{Verdict: Orphaned, Resource: name(hd), doc: hd,
			Detail: "selector " + labels(sel) + " matches no pods after this change: traffic to it stops"})
	}

	sortImpacts(out)

	return out
}

func sortImpacts(is []Impact) {
	slices.SortFunc(is, func(x, y Impact) int {
		if c := slices.Index(order, x.Verdict) - slices.Index(order, y.Verdict); c != 0 {
			return c
		}
		return strings.Compare(x.Resource, y.Resource)
	})
}

// classify returns the impact of one changed object, if it has one worth reporting.
func classify(b, h render.Doc) (Impact, bool) {
	i := Impact{Resource: name(h), doc: h}

	if hook(h.Object) {
		i.Verdict = Hook
		return i, true
	}

	if why := rejected(b, h); why != "" {
		i.Verdict, i.Detail = WillFail, why
		if replaced(h.Object) {
			i.Verdict, i.Detail = Recreate, i.Detail+", Argo replaces it"
			if workloads[h.Kind] {
				i.Pods = replicas(h.Object)
			}
		}
		return i, true
	}

	if h.Kind == "PersistentVolumeClaim" {
		from, to := storage(b.Object), storage(h.Object)
		if from.Cmp(to) == 0 {
			return i, false
		}
		i.Verdict, i.Detail = Resize, from.String()+" → "+to.String()+", if the StorageClass allows expansion"
		return i, true
	}

	if !workloads[h.Kind] {
		return i, false
	}

	bt, ht := get(b.Object, "spec", "template"), get(h.Object, "spec", "template")
	partition, _ := get(h.Object, "spec", "updateStrategy", "rollingUpdate", "partition").(int)
	switch {
	case !reflect.DeepEqual(bt, ht) && str(h.Object, "spec", "updateStrategy", "type") == "OnDelete":
		i.Verdict, i.Detail = NoEffect, "OnDelete: pods keep the old template until someone deletes them"
	case !reflect.DeepEqual(bt, ht) && h.Kind == "DaemonSet":
		i.Verdict, i.Detail = Restart, "one pod per node"
	case !reflect.DeepEqual(bt, ht) && h.Kind == "StatefulSet" && partition >= replicas(h.Object):
		i.Verdict, i.Detail = NoEffect, fmt.Sprintf("partition %d: no pod is updated", partition)
	case !reflect.DeepEqual(bt, ht) && h.Kind == "StatefulSet" && partition > 0:
		i.Verdict, i.Pods, i.Detail = Ordered, replicas(h.Object)-partition, fmt.Sprintf("one at a time, ordinals %d and up", partition)
	case !reflect.DeepEqual(bt, ht) && h.Kind == "StatefulSet":
		i.Verdict, i.Pods, i.Detail = Ordered, replicas(h.Object), "one at a time"
	case !reflect.DeepEqual(bt, ht):
		i.Verdict, i.Pods, i.Detail = Restart, replicas(h.Object), "pod template"
		if !slices.Equal(images(bt), images(ht)) {
			i.Detail = "image"
		}
		if m := mutableTags(ht); len(m) > 0 {
			i.Detail += ", pulls mutable " + strings.Join(m, ", ")
		}
	case slices.Equal(changedSpec(b.Object, h.Object), []string{"replicas"}):
		i.Verdict, i.Detail = Scale, fmt.Sprintf("%d → %d", replicas(b.Object), replicas(h.Object))
	default:
		return i, false
	}

	return i, true
}

// rejected says why the API server will reject the update, or "" if it accepts it.
func rejected(b, h render.Doc) string {
	if immutable(b.Object) && !sameData(b.Object, h.Object) {
		return "immutable " + h.Kind + " data changed"
	}

	var fs []string
	for _, f := range changedSpec(b.Object, h.Object) {
		m, strict := mutable[h.Kind]
		if (strict && !m[f]) || (workloads[h.Kind] && f == "selector") || (h.Kind == "Service" && f == "clusterIP") {
			fs = append(fs, "spec."+f)
		}
	}
	if h.Kind == "PersistentVolumeClaim" {
		if !reflect.DeepEqual(get(b.Object, "spec", "resources", "limits"), get(h.Object, "spec", "resources", "limits")) {
			fs = append(fs, "spec.resources.limits")
		}
		if s := storage(h.Object); s.Cmp(storage(b.Object)) < 0 {
			return "spec.resources.requests.storage can't shrink"
		}
	}
	if len(fs) == 0 {
		return ""
	}

	return strings.Join(fs, ", ") + " is immutable"
}

// consumers reports how each workload using a changed ConfigMap or Secret sees the new data.
func consumers(cm render.Doc, h map[string]render.Doc, restarted map[string]bool) []Impact {
	var out []Impact
	for k, w := range h {
		if !workloads[w.Kind] || w.Namespace != cm.Namespace {
			continue
		}

		env, subPath, volume := uses(get(w.Object, "spec", "template", "spec"), cm.Kind, cm.Name)
		i := Impact{Resource: name(cm) + " → " + name(w)}
		switch {
		case !env && !subPath && !volume:
			continue
		case restarted[k]:
			i.Verdict, i.Detail = PickedUp, "pods restart"
		case env:
			i.Verdict, i.Detail = NoEffect, "env var: running pods keep the OLD value until they restart"
		case subPath:
			i.Verdict, i.Detail = NoEffect, "subPath mount: never updates until pods restart"
		default:
			i.Verdict, i.Detail = Delayed, "volume file updates in ~1m, the app must re-read it"
		}
		out = append(out, i)
	}

	return out
}

// uses reports how a pod spec consumes the named ConfigMap or Secret.
func uses(spec any, kind, name string) (env, subPath, volume bool) {
	vols := map[string]bool{}
	for _, v := range list(get(spec, "volumes")) {
		srcs := append([]any{v}, list(get(v, "projected", "sources"))...)
		for _, s := range srcs {
			if (kind == "ConfigMap" && str(s, "configMap", "name") == name) ||
				(kind == "Secret" && (str(s, "secret", "secretName") == name || str(s, "secret", "name") == name)) {
				vols[str(v, "name")] = true
			}
		}
	}

	ref, keyRef := "configMapRef", "configMapKeyRef"
	if kind == "Secret" {
		ref, keyRef = "secretRef", "secretKeyRef"
	}

	for _, field := range []string{"containers", "initContainers"} {
		for _, c := range list(get(spec, field)) {
			for _, e := range list(get(c, "envFrom")) {
				env = env || str(e, ref, "name") == name
			}
			for _, e := range list(get(c, "env")) {
				env = env || str(e, "valueFrom", keyRef, "name") == name
			}
			for _, m := range list(get(c, "volumeMounts")) {
				if vols[str(m, "name")] {
					if str(m, "subPath") != "" {
						subPath = true
					} else {
						volume = true
					}
				}
			}
		}
	}

	return env, subPath, volume
}

// deleted reports a removed object, as data loss where deleting it deletes stored data.
func deleted(d render.Doc) Impact {
	i := Impact{Verdict: Data, Resource: name(d), Detail: "deleted"}
	switch {
	case d.Kind == "PersistentVolumeClaim" || d.Kind == "Namespace":
	case d.Kind == "CustomResourceDefinition":
		i.Detail = "deleted, and every custom resource of it"
	case d.Kind == "StatefulSet" && str(d.Object, "spec", "persistentVolumeClaimRetentionPolicy", "whenDeleted") == "Delete":
		i.Detail = "deleted, and its PVCs with it"
	default:
		i.Verdict, i.Detail = Delete, ""
	}

	return i
}

func index(docs []render.Doc) map[string]render.Doc {
	out := make(map[string]render.Doc, len(docs))
	for _, d := range docs {
		out[d.Kind+"/"+d.Namespace+"/"+d.Name] = d
	}

	return out
}

func name(d render.Doc) string {
	if clusterScoped[d.Kind] {
		return d.Kind + " " + d.Name
	}

	return d.Kind + " " + d.Namespace + "/" + d.Name
}

// selects reports whether the Service svc selects the pods of a workload, Job or Pod in docs.
func selects(svc render.Doc, docs []render.Doc) bool {
	sel, _ := get(svc.Object, "spec", "selector").(obj)
	if len(sel) == 0 {
		return false
	}

	for _, d := range docs {
		if d.Namespace != svc.Namespace {
			continue
		}

		var lbls obj
		switch {
		case workloads[d.Kind] || d.Kind == "Job" || d.Kind == "ReplicaSet":
			lbls, _ = get(d.Object, "spec", "template", "metadata", "labels").(obj)
		case d.Kind == "Pod":
			lbls, _ = get(d.Object, "metadata", "labels").(obj)
		}
		if len(lbls) > 0 && matches(sel, lbls) {
			return true
		}
	}

	return false
}

func matches(sel, lbls obj) bool {
	for k, v := range sel {
		if fmt.Sprint(lbls[k]) != fmt.Sprint(v) {
			return false
		}
	}

	return true
}

// labels formats a selector as k=v,k=v in key order.
func labels(sel obj) string {
	keys := slices.Sorted(maps.Keys(sel))
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s=%v", k, sel[k])
	}

	return strings.Join(parts, ",")
}

// hook reports whether Helm or Argo CD runs the object as a hook, which recreates it instead of patching.
func hook(o obj) bool {
	return str(o, "metadata", "annotations", "helm.sh/hook") != "" ||
		str(o, "metadata", "annotations", "argocd.argoproj.io/hook") != ""
}

// replaced reports whether Argo CD deletes and recreates the object instead of patching it.
func replaced(o obj) bool {
	opts := str(o, "metadata", "annotations", "argocd.argoproj.io/sync-options")
	return strings.Contains(opts, "Replace=true") || strings.Contains(opts, "Force=true")
}

func immutable(o obj) bool { return o["immutable"] == true }

func sameData(a, b obj) bool {
	return reflect.DeepEqual(a["data"], b["data"]) && reflect.DeepEqual(a["stringData"], b["stringData"]) &&
		reflect.DeepEqual(a["binaryData"], b["binaryData"])
}

// changedSpec lists the top-level spec fields that differ, sorted.
func changedSpec(a, b obj) []string {
	as, _ := a["spec"].(obj)
	bs, _ := b["spec"].(obj)
	var out []string
	for _, m := range []obj{as, bs} {
		for k := range m {
			if !slices.Contains(out, k) && !reflect.DeepEqual(as[k], bs[k]) {
				out = append(out, k)
			}
		}
	}
	slices.Sort(out)

	return out
}

func replicas(o obj) int {
	if r, ok := get(o, "spec", "replicas").(int); ok {
		return r
	}

	return 1
}

// storage is a claim's requested size; zero if unset or unparsable.
func storage(o obj) resource.Quantity {
	q, _ := resource.ParseQuantity(fmt.Sprint(get(o, "spec", "resources", "requests", "storage")))
	return q
}

// mutableTags lists the template's images that a rollout pulls anew: tagged latest or untagged, without a digest.
func mutableTags(tpl any) []string {
	var out []string
	for _, field := range []string{"initContainers", "containers"} {
		for _, c := range list(get(tpl, "spec", field)) {
			img := str(c, "image")
			if strings.Contains(img, "@") || str(c, "imagePullPolicy") == "Never" {
				continue
			}
			// The tag follows the last colon after the last slash; a registry port has a slash after it.
			last := img[strings.LastIndex(img, "/")+1:]
			if _, tag, ok := strings.Cut(last, ":"); !ok || tag == "latest" {
				out = append(out, img)
			}
		}
	}

	return out
}

func images(tpl any) []string {
	var out []string
	for _, field := range []string{"initContainers", "containers"} {
		for _, c := range list(get(tpl, "spec", field)) {
			out = append(out, str(c, "image"))
		}
	}

	return out
}

func get(o any, path ...string) any {
	for _, p := range path {
		m, ok := o.(obj)
		if !ok {
			return nil
		}
		o = m[p]
	}

	return o
}

func str(o any, path ...string) string { s, _ := get(o, path...).(string); return s }

func list(v any) []any { l, _ := v.([]any); return l }
