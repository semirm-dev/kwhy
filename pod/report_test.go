package pod_test

import (
	"bytes"
	"encoding/json"
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
