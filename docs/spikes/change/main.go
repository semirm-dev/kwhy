// Spike: classify what applying head.yaml over base.yaml does to running workloads.
// usage: spike-impact base.yaml head.yaml
package main

import (
	"bytes"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type obj = map[string]any

func load(f string) map[string]obj {
	b, err := os.ReadFile(f)
	if err != nil {
		panic(err)
	}
	out := map[string]obj{}
	dec := yaml.NewDecoder(bytes.NewReader(b))
	for {
		var o obj
		if dec.Decode(&o) != nil {
			break
		}
		if o == nil {
			continue
		}
		out[key(o)] = o
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

func key(o obj) string {
	ns := str(o, "metadata", "namespace")
	if ns == "" {
		ns = "default"
	}
	return fmt.Sprintf("%s/%s/%s", str(o, "kind"), ns, str(o, "metadata", "name"))
}

func replicas(o obj) int {
	if r, ok := get(o, "spec", "replicas").(int); ok {
		return r
	}
	return 1
}

// changedTop lists top-level spec keys that differ.
func changedTop(a, b obj) []string {
	as, _ := get(a, "spec").(obj)
	bs, _ := get(b, "spec").(obj)
	seen := map[string]bool{}
	var out []string
	for _, m := range []obj{as, bs} {
		for k := range m {
			if !seen[k] && !reflect.DeepEqual(as[k], bs[k]) {
				out = append(out, k)
			}
			seen[k] = true
		}
	}
	sort.Strings(out)
	return out
}

// Mutable spec fields per kind; anything else changing is rejected by the API server.
var mutable = map[string]map[string]bool{
	"StatefulSet": {"replicas": true, "template": true, "updateStrategy": true, "persistentVolumeClaimRetentionPolicy": true, "minReadySeconds": true, "ordinals": true, "revisionHistoryLimit": true},
	"Job":         {"parallelism": true, "completions": true, "activeDeadlineSeconds": true, "suspend": true, "backoffLimit": true, "ttlSecondsAfterFinished": true, "backoffLimitPerIndex": true, "maxFailedIndexes": true, "podFailurePolicy": true, "successPolicy": true, "managedBy": false},
}

type ref struct {
	kind, name string
	env        bool
	subPath    bool
}

// refs finds ConfigMaps/Secrets a pod template consumes, and how.
func refs(tpl any) []ref {
	var out []ref
	spec := get(tpl, "spec")
	vols := map[string]ref{}
	for _, v := range asList(get(spec, "volumes")) {
		n := str(v, "name")
		if c := str(v, "configMap", "name"); c != "" {
			vols[n] = ref{kind: "ConfigMap", name: c}
		}
		if s := str(v, "secret", "secretName"); s != "" {
			vols[n] = ref{kind: "Secret", name: s}
		}
		for _, src := range asList(get(v, "projected", "sources")) {
			if c := str(src, "configMap", "name"); c != "" {
				vols[n] = ref{kind: "ConfigMap", name: c}
			}
			if s := str(src, "secret", "name"); s != "" {
				vols[n] = ref{kind: "Secret", name: s}
			}
		}
	}
	for _, field := range []string{"containers", "initContainers"} {
		for _, c := range asList(get(spec, field)) {
			for _, e := range asList(get(c, "envFrom")) {
				if n := str(e, "configMapRef", "name"); n != "" {
					out = append(out, ref{"ConfigMap", n, true, false})
				}
				if n := str(e, "secretRef", "name"); n != "" {
					out = append(out, ref{"Secret", n, true, false})
				}
			}
			for _, e := range asList(get(c, "env")) {
				if n := str(e, "valueFrom", "configMapKeyRef", "name"); n != "" {
					out = append(out, ref{"ConfigMap", n, true, false})
				}
				if n := str(e, "valueFrom", "secretKeyRef", "name"); n != "" {
					out = append(out, ref{"Secret", n, true, false})
				}
			}
			for _, m := range asList(get(c, "volumeMounts")) {
				if r, ok := vols[str(m, "name")]; ok {
					r.subPath = str(m, "subPath") != ""
					out = append(out, r)
				}
			}
		}
	}
	return out
}

func asList(v any) []any { l, _ := v.([]any); return l }

var workloads = map[string]bool{"Deployment": true, "StatefulSet": true, "DaemonSet": true}

func main() {
	base, head := load(os.Args[1]), load(os.Args[2])
	var lines []string
	add := func(f string, a ...any) { lines = append(lines, fmt.Sprintf(f, a...)) }

	restarted := map[string]bool{}
	for k, h := range head {
		b, ok := base[k]
		kind := str(h, "kind")
		if !ok {
			add("+ create     %s", k)
			continue
		}
		ch := changedTop(b, h)
		if len(ch) == 0 && reflect.DeepEqual(b["data"], h["data"]) && reflect.DeepEqual(b["stringData"], h["stringData"]) {
			continue
		}
		if m, strict := mutable[kind]; strict {
			for _, f := range ch {
				if !m[f] {
					add("✗ will fail  %s  spec.%s is immutable", k, f)
				}
			}
		}
		if workloads[kind] {
			for _, f := range ch {
				if f == "selector" {
					add("✗ will fail  %s  spec.selector is immutable", k)
				}
			}
		}
		if workloads[kind] && !reflect.DeepEqual(get(b, "spec", "template"), get(h, "spec", "template")) {
			restarted[k] = true
			switch kind {
			case "StatefulSet":
				add("⏱ ordered    %s  %d pods, one at a time", k, replicas(h))
			case "DaemonSet":
				add("↻ restart    %s  one pod per node", k)
			default:
				add("↻ restart    %s  %d pods", k, replicas(h))
			}
		} else if workloads[kind] && fmt.Sprint(ch) == "[replicas]" {
			add("⇅ scale      %s  %d → %d", k, replicas(b), replicas(h))
		}
		if kind == "Service" && strings.Contains(fmt.Sprint(ch), "clusterIP") {
			add("✗ will fail  %s  spec.clusterIP is immutable", k)
		}
	}
	// ConfigMap / Secret data changes: who consumes them, and do they pick it up?
	for k, h := range head {
		kind := str(h, "kind")
		b, ok := base[k]
		if !ok || (kind != "ConfigMap" && kind != "Secret") || (reflect.DeepEqual(b["data"], h["data"]) && reflect.DeepEqual(b["stringData"], h["stringData"])) {
			continue
		}
		if get(b, "immutable") == true {
			add("✗ will fail  %s  immutable %s data changed", k, kind)
			continue
		}
		name, ns := str(h, "metadata", "name"), strings.Split(k, "/")[1]
		consumers := 0
		for wk, w := range head {
			if !workloads[str(w, "kind")] || strings.Split(wk, "/")[1] != ns {
				continue
			}
			for _, r := range refs(get(w, "spec", "template")) {
				if r.kind != kind || r.name != name {
					continue
				}
				consumers++
				switch {
				case restarted[wk]:
					add("✓ picked up  %s → %s (pods restart)", k, wk)
				case r.env:
					add("! no effect  %s → %s  env var: running pods keep the OLD value", k, wk)
				case r.subPath:
					add("! no effect  %s → %s  subPath mount never updates", k, wk)
				default:
					add("~ delayed    %s → %s  volume file updates in ~1m, app must re-read it", k, wk)
				}
			}
		}
		if consumers == 0 {
			add("· data only  %s  no workload in this diff consumes it", k)
		}
	}
	for k := range base {
		if _, ok := head[k]; !ok {
			add("- delete     %s", k)
		}
	}
	sort.Strings(lines)
	fmt.Println(strings.Join(lines, "\n"))
}
