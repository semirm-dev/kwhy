package pod_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/semirm-dev/kwhy/pod"
)

func report(t *testing.T, write func(*bytes.Buffer, *v1.Pod, pod.Result) error) string {
	t.Helper()

	p := newPod("train", "", "2", "16Gi")
	p.Spec.NodeSelector = map[string]string{"gpu": "true"}
	cl, err := pod.Snapshot(t.Context(), fake.NewClientset(
		node("gpu-1", "8", "64Gi", map[string]string{"gpu": "true"}),
		node("cpu-1", "4", "16Gi", nil),
		node("cpu-2", "4", "16Gi", nil),
		node("cpu-3", "4", "16Gi", nil),
		newPod("busy", "gpu-1", "7", "8Gi"),
	))
	if err != nil {
		t.Fatal(err)
	}

	r, err := pod.Evaluate(t.Context(), cl, p)
	if err != nil {
		t.Fatal(err)
	}

	var b bytes.Buffer
	if err := write(&b, p, r); err != nil {
		t.Fatal(err)
	}

	return b.String()
}

func TestWriteHuman(t *testing.T) {
	got := report(t, func(b *bytes.Buffer, p *v1.Pod, r pod.Result) error { return pod.WriteHuman(b, p, r) })

	head, body, _ := strings.Cut(got, "\n")
	if !strings.HasPrefix(head, "ml/train Pending ") || !strings.HasSuffix(head, " · 0 of 4 nodes fit · every check, every node") {
		t.Errorf("title = %q", head)
	}

	want := `
NODE       NodeAffinity  NodeResourcesFit
cpu-* (3)  ✗             ✓
gpu-1      ✓             ✗ cpu 7/8 +2

Reasons:
  NodeAffinity: node(s) didn't match Pod's node affinity/selector
  NodeResourcesFit: Insufficient cpu
`
	if body != want {
		t.Errorf("got\n%s\nwant\n%s", body, want)
	}
}

func TestWriteMarkdown(t *testing.T) {
	got := report(t, func(b *bytes.Buffer, p *v1.Pod, r pod.Result) error { return pod.WriteMarkdown(b, p, r) })

	for _, want := range []string{"| Node | NodeAffinity | NodeResourcesFit |\n|---|---|---|\n", "| `gpu-1` | ✓ | ✗ cpu 7/8 +2 |", "- **NodeResourcesFit**: Insufficient cpu"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
}

func TestWriteJSON(t *testing.T) {
	got := report(t, func(b *bytes.Buffer, p *v1.Pod, r pod.Result) error { return pod.WriteJSON(b, p, r) })

	var out struct {
		Schema string     `json:"schema"`
		Nodes  []pod.Node `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(got), &out); err != nil {
		t.Fatal(err)
	}
	if out.Schema != "kwhy.pod/v1" || len(out.Nodes) != 4 {
		t.Errorf("schema %q, %d nodes; want kwhy.pod/v1 and 4 ungrouped", out.Schema, len(out.Nodes))
	}
}

func TestWriteSources(t *testing.T) {
	p := newPod("train", "", "2", "16Gi")
	r := pod.Result{Plugins: []string{"NodeResourcesFit"}, Nodes: []pod.Node{{Name: "n1", Failed: []pod.Check{{Plugin: "NodeResourcesFit", Reasons: []string{"Insufficient cpu"}}}}},
		Sources: []pod.Source{{Plugin: "NodeResourcesFit", Field: ".spec.containers[0].resources.requests.cpu", Value: "2", From: "resources.requests.cpu (values-prod.yaml:31)"}}}

	for name, write := range map[string]func(*bytes.Buffer) error{
		"human":    func(b *bytes.Buffer) error { return pod.WriteHuman(b, p, r) },
		"markdown": func(b *bytes.Buffer) error { return pod.WriteMarkdown(b, p, r) },
		"json":     func(b *bytes.Buffer) error { return pod.WriteJSON(b, p, r) },
	} {
		var b bytes.Buffer
		if err := write(&b); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(b.String(), "values-prod.yaml:31") {
			t.Errorf("%s: no source in\n%s", name, b.String())
		}
	}

	var b bytes.Buffer
	pod.WriteHuman(&b, p, r)
	if want := "Source:\n  NodeResourcesFit  .spec.containers[0].resources.requests.cpu = 2  ← resources.requests.cpu (values-prod.yaml:31)\n"; !strings.Contains(b.String(), want) {
		t.Errorf("human:\n%s\nwant %q", b.String(), want)
	}
}

func TestWriteScheduled(t *testing.T) {
	p := newPod("api-1", "n1", "1", "1Gi")
	p.Status.Phase = v1.PodRunning

	var human, md, js bytes.Buffer
	if err := errors.Join(pod.WriteHuman(&human, p, pod.Result{}), pod.WriteMarkdown(&md, p, pod.Result{}),
		pod.WriteJSON(&js, p, pod.Result{})); err != nil {
		t.Fatal(err)
	}

	if got := human.String(); got != "ml/api-1 is on n1, Running and ready.\n" {
		t.Errorf("human = %q", got)
	}
	if got := md.String(); got != "**ml/api-1 is on n1, Running and ready.**\n" {
		t.Errorf("markdown = %q", got)
	}

	var out map[string]any
	if err := json.Unmarshal(js.Bytes(), &out); err != nil {
		t.Fatalf("json: %v\n%s", err, js.String())
	}
	if out["schema"] != "kwhy.pod/v1" || out["scheduled"] != "n1" || out["nodes"] != nil {
		t.Errorf("json = %v", out)
	}
}

func TestWriteHealthy(t *testing.T) {
	var human, js bytes.Buffer
	if err := errors.Join(pod.WriteHealthy(&human, "human", "ml", "deploy/web", pod.Healthy{Pods: 3}),
		pod.WriteHealthy(&js, "json", "ml", "deploy/web", pod.Healthy{Pods: 3})); err != nil {
		t.Fatal(err)
	}

	if got := human.String(); got != "ml/deploy/web: all 3 pods ready.\n" {
		t.Errorf("human = %q", got)
	}
	if !strings.Contains(js.String(), `"ready": 3`) {
		t.Errorf("json = %s", js.String())
	}
}

func TestWriteProblems(t *testing.T) {
	p := running(ok("sidecar"))
	p.Status.Conditions = []v1.PodCondition{{Type: v1.PodReady, Status: v1.ConditionFalse}}
	r := pod.Result{Problems: []pod.Problem{{Container: "app", Kind: pod.OOM, Reason: "CrashLoopBackOff", Detail: "killed at limit 512Mi, 6 restarts",
		Events: []string{"Back-off restarting failed container app in pod api-1"}, Logs: []string{"allocating 1GiB"}}}}

	var b bytes.Buffer
	if err := pod.WriteHuman(&b, p, r); err != nil {
		t.Fatal(err)
	}

	want := `ml/api-1 on n1 · Running · not ready · 2 containers, 1 failing

CONTAINER  PROBLEM  DETAIL
app        oom      killed at limit 512Mi, 6 restarts
sidecar    ✓

Events:
  app: Back-off restarting failed container app in pod api-1

Logs (app, last run):
  allocating 1GiB
`
	if b.String() != want {
		t.Errorf("got\n%s\nwant\n%s", b.String(), want)
	}

	b.Reset()
	if err := pod.WriteMarkdown(&b, p, r); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "| `app` | oom | killed at limit 512Mi, 6 restarts |") {
		t.Errorf("markdown:\n%s", b.String())
	}
}

func TestWritePreempt(t *testing.T) {
	p := newPod("api", "", "2", "1Gi")
	r := pod.Result{Plugins: []string{"NodeResourcesFit"}, Preempt: []string{"n1", "n2"}, PreemptNode: "n1",
		Victims: []string{"ml/batch"}, PDBViolations: 1,
		Nodes: []pod.Node{{Name: "n1", Failed: []pod.Check{{Plugin: "NodeResourcesFit", Reasons: []string{"Insufficient cpu"}}}}}}

	var b bytes.Buffer
	if err := pod.WriteHuman(&b, p, r); err != nil {
		t.Fatal(err)
	}
	want := "\nPreempting lower-priority pods would make room on 2 nodes (n1, n2). The scheduler would pick n1 and evict " +
		"ml/batch. That breaks 1 PodDisruptionBudget; it goes ahead because every node here would break one.\n"
	if !strings.Contains(b.String(), want) {
		t.Errorf("no preemption line in\n%s", b.String())
	}
}
