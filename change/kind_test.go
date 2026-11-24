//go:build kind

// The kind suite checks every prediction against a real cluster: apply base, predict, apply head, look.
// Run: kind create cluster --name kwhy && go test -tags kind -timeout 20m ./change/
// KWHY_CONTEXT picks another kube context (default kind-kwhy).
package change_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/semirm-dev/kwhy/change"
	"github.com/semirm-dev/kwhy/render"
)

// propagate is how long a volume-mounted ConfigMap may take to update (kubelet sync period plus cache TTL).
const propagate = 150 * time.Second

func TestKind(t *testing.T) {
	kctx := os.Getenv("KWHY_CONTEXT")
	if kctx == "" {
		kctx = "kind-kwhy"
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{CurrentContext: kctx}).ClientConfig()
	if err != nil {
		t.Fatal(err)
	}
	c, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}

	dirs, err := filepath.Glob("../testdata/change/*")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}

	var (
		mu   sync.Mutex
		seen []change.Verdict
	)
	t.Run("fixtures", func(t *testing.T) {
		for _, dir := range dirs {
			name := filepath.Base(dir)
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				k := kind{t: t, c: c, ctx: kctx, ns: "kwhy-" + name}
				vs := k.run(dir)
				mu.Lock()
				seen = append(seen, vs...)
				mu.Unlock()
			})
		}
	})

	// Every verdict kubectl can show must be checked by some fixture. Hooks and Argo replace need Helm and Argo.
	for _, v := range []change.Verdict{change.Restart, change.Ordered, change.Scale, change.Resize, change.NoEffect, change.Orphaned,
		change.Delayed, change.PickedUp, change.WillFail, change.Data} {
		if !slices.Contains(seen, v) {
			t.Errorf("no fixture checks %q", v)
		}
	}
}

type kind struct {
	t   *testing.T
	c   kubernetes.Interface
	ctx string
	ns  string
}

// run applies base, predicts, applies head and checks each prediction; it returns the verdicts it checked.
func (k kind) run(dir string) []change.Verdict {
	t := k.t
	base, head := k.load(filepath.Join(dir, "base.yaml")), k.load(filepath.Join(dir, "head.yaml"))

	k.kubectl("", "create", "namespace", k.ns)
	t.Cleanup(func() {
		k.kubectl("", "delete", "namespace", k.ns, "--wait=false")
		for _, d := range base {
			if d.Kind == "StorageClass" {
				k.kubectl("", "delete", "storageclass", d.Name, "--ignore-not-found")
			}
		}
	})

	if _, err := k.apply(filepath.Join(dir, "base.yaml")); err != nil {
		t.Fatalf("apply base: %v", err)
	}
	k.settle(base)

	is := change.Diff(base, head)
	if err := change.Live(t.Context(), k.c, is); err != nil {
		t.Fatal(err)
	}
	if len(is) == 0 {
		t.Fatal("fixture predicts nothing")
	}
	for _, i := range is {
		t.Logf("predicted: %s %s %s", i.Verdict, i.Resource, i.Detail)
	}

	before := map[string][]string{}
	for _, d := range base {
		if isWorkload(d.Kind) {
			before[d.Name] = k.pods(d.Name)
		}
	}

	failed := map[string]bool{} // resource names whose apply was rejected
	for _, d := range head {
		if out, err := k.applyDoc(d); err != nil {
			failed[resourceOf(d)] = true
			t.Logf("apply %s rejected: %s", resourceOf(d), out)
		}
	}
	for _, d := range base {
		if !slices.ContainsFunc(head, func(h render.Doc) bool { return h.Kind == d.Kind && h.Name == d.Name }) {
			k.kubectl(k.ns, "delete", d.Kind, d.Name, "--wait=false")
		}
	}
	k.settle(head)

	var checked []change.Verdict
	for _, i := range is {
		if i.Verdict != change.WillFail && failed[i.Resource] {
			t.Errorf("%s: predicted %s, but the API rejected it", i.Resource, i.Verdict)
			continue
		}
		if k.check(i, before, failed) {
			checked = append(checked, i.Verdict)
		}
	}

	// The loop above checks only what was predicted: a workload whose pods went with no prediction is a miss.
	replaced := map[string]bool{}
	for _, i := range is {
		switch i.Verdict {
		case change.Restart, change.Ordered, change.Recreate, change.PickedUp, change.Delete, change.Data:
			w := i.Resource[strings.LastIndex(i.Resource, " ")+1:]
			replaced[w[strings.LastIndex(w, "/")+1:]] = true
		}
	}
	for app, pods := range before {
		after := k.pods(app)
		if n := len(slices.DeleteFunc(slices.Clone(pods), func(p string) bool { return slices.Contains(after, p) })); n > 0 && !replaced[app] {
			t.Errorf("%s: %d pods restarted, kwhy predicted none", app, n)
		}
	}

	return checked
}

// check compares one prediction with the cluster. It reports whether it checked anything.
func (k kind) check(i change.Impact, before map[string][]string, failed map[string]bool) bool {
	t := k.t
	workload := i.Resource
	if _, w, ok := strings.Cut(i.Resource, " → "); ok {
		workload = w
	}
	app := workload[strings.LastIndex(workload, "/")+1:]
	gone := func() int {
		after := k.pods(app)
		n := 0
		for _, p := range before[app] {
			if !slices.Contains(after, p) {
				n++
			}
		}
		return n
	}

	switch i.Verdict {
	case change.Restart, change.Ordered:
		if strings.HasPrefix(i.Resource, "Pod ") { // restarts in place: same pod, new container
			k.eventually(i.Resource+" restarted", time.Minute, func() bool {
				p, err := k.c.CoreV1().Pods(k.ns).Get(t.Context(), app, metav1.GetOptions{})
				return err == nil && len(p.Status.ContainerStatuses) > 0 && p.Status.ContainerStatuses[0].RestartCount > 0
			})
			break
		}
		if n := gone(); n != i.Pods {
			t.Errorf("%s: predicted %d pods restarted, %d were", i.Resource, i.Pods, n)
		}
	case change.Scale:
		if n := gone(); n != 0 {
			t.Errorf("%s: predicted a scale, %d pods restarted", i.Resource, n)
		}
	case change.WillFail:
		if !failed[i.Resource] {
			t.Errorf("%s: predicted will fail, the API accepted it", i.Resource)
		}
	case change.Resize:
		got, err := k.c.CoreV1().PersistentVolumeClaims(k.ns).Get(t.Context(), app, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if want := i.Detail[strings.LastIndex(i.Detail, " ")+1:]; got.Spec.Resources.Requests.Storage().String() != want {
			t.Errorf("%s: predicted resize to %s, spec has %s", i.Resource, want, got.Spec.Resources.Requests.Storage())
		}
	case change.Data:
		// The StatefulSet's claims go with it.
		k.eventually(i.Resource+" claims deleted", 2*time.Minute, func() bool {
			l, err := k.c.CoreV1().PersistentVolumeClaims(k.ns).List(t.Context(), metav1.ListOptions{})
			return err == nil && len(l.Items) == 0
		})
	case change.PickedUp:
		if n, all := gone(), len(before[app]); n != all {
			t.Errorf("%s: predicted pods restart, %d of %d did", i.Resource, n, all)
		}
		k.sees(app, "v2")
	case change.Delayed:
		k.eventually(i.Resource+" sees v2", propagate, func() bool { return k.value(app) == "v2" })
		if n := gone(); n != 0 {
			t.Errorf("%s: predicted no restart, %d pods restarted", i.Resource, n)
		}
	case change.NoEffect:
		if strings.Contains(i.Resource, " → ") {
			time.Sleep(propagate) // long enough for a volume to have updated, if it would
			k.sees(app, "v1")
		}
		if n := gone(); n != 0 {
			t.Errorf("%s: predicted no effect, %d pods restarted", i.Resource, n)
		}
	case change.Orphaned:
		svc := app
		k.eventually(i.Resource+" has no endpoints", time.Minute, func() bool {
			l, err := k.c.DiscoveryV1().EndpointSlices(k.ns).List(t.Context(), metav1.ListOptions{LabelSelector: "kubernetes.io/service-name=" + svc})
			if err != nil {
				return false
			}
			for _, es := range l.Items {
				if len(es.Endpoints) > 0 {
					return false
				}
			}
			return true
		})
	default:
		return false
	}

	return true
}

// sees checks that every pod of app reads want for MODE, from env or the mounted file.
func (k kind) sees(app, want string) {
	if got := k.value(app); got != want {
		k.t.Errorf("%s: pods see %q, want %q", app, got, want)
	}
}

// value is MODE as the pods of app see it, or a mismatch marker if they disagree.
func (k kind) value(app string) string {
	var vals []string
	for _, p := range k.podNames(app) {
		out, _ := k.kubectlOut(k.ns, "exec", p, "--", "sh", "-c", "cat /cfg/MODE 2>/dev/null || printenv MODE")
		vals = append(vals, strings.TrimSpace(out))
	}
	slices.Sort(vals)
	if vals = slices.Compact(vals); len(vals) != 1 {
		return fmt.Sprint(vals)
	}

	return vals[0]
}

// settle waits until every workload in docs has finished rolling out.
func (k kind) settle(docs []render.Doc) {
	for _, d := range docs {
		if !isWorkload(d.Kind) && d.Kind != "Pod" {
			continue
		}
		k.eventually(resourceOf(d)+" settled", 3*time.Minute, func() bool { return k.settled(d) })
	}
}

func (k kind) settled(d render.Doc) bool {
	ctx, apps := k.t.Context(), k.c.AppsV1()
	switch d.Kind {
	case "Pod":
		p, err := k.c.CoreV1().Pods(k.ns).Get(ctx, d.Name, metav1.GetOptions{})
		return err == nil && p.Status.Phase == "Running"
	case "Deployment":
		o, err := apps.Deployments(k.ns).Get(ctx, d.Name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		n, s := *o.Spec.Replicas, o.Status
		// A rejected selector change leaves the old Deployment in place, which is settled too.
		return s.ObservedGeneration >= o.Generation && s.UpdatedReplicas == n && s.ReadyReplicas == n && s.Replicas == n
	case "StatefulSet":
		o, err := apps.StatefulSets(k.ns).Get(ctx, d.Name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		n, s := *o.Spec.Replicas, o.Status
		want := n
		if ru := o.Spec.UpdateStrategy.RollingUpdate; ru != nil && ru.Partition != nil {
			want = max(n-*ru.Partition, 0)
		}
		updated := o.Spec.UpdateStrategy.Type == "OnDelete" || s.UpdatedReplicas >= want
		return s.ObservedGeneration >= o.Generation && s.ReadyReplicas == n && s.Replicas == n && updated
	case "DaemonSet":
		o, err := apps.DaemonSets(k.ns).Get(ctx, d.Name, metav1.GetOptions{})
		if err != nil {
			return false
		}
		s := o.Status
		updated := o.Spec.UpdateStrategy.Type == "OnDelete" || s.UpdatedNumberScheduled == s.DesiredNumberScheduled
		return s.ObservedGeneration >= o.Generation && s.DesiredNumberScheduled > 0 && s.NumberReady == s.DesiredNumberScheduled &&
			s.CurrentNumberScheduled == s.DesiredNumberScheduled && updated
	}

	return true
}

// pods lists the UIDs of the live (not terminating) pods of app.
func (k kind) pods(app string) []string {
	l, err := k.c.CoreV1().Pods(k.ns).List(k.t.Context(), metav1.ListOptions{LabelSelector: "app=" + app})
	if err != nil {
		k.t.Fatal(err)
	}
	var out []string
	for _, p := range l.Items {
		if p.DeletionTimestamp == nil {
			out = append(out, string(p.UID))
		}
	}

	return out
}

func (k kind) podNames(app string) []string {
	l, err := k.c.CoreV1().Pods(k.ns).List(k.t.Context(), metav1.ListOptions{LabelSelector: "app=" + app})
	if err != nil {
		k.t.Fatal(err)
	}
	var out []string
	for _, p := range l.Items {
		if p.DeletionTimestamp == nil {
			out = append(out, p.Name)
		}
	}

	return out
}

func (k kind) eventually(what string, timeout time.Duration, ok func() bool) {
	ctx, cancel := context.WithTimeout(k.t.Context(), timeout)
	defer cancel()
	for !ok() {
		select {
		case <-ctx.Done():
			k.t.Errorf("timed out after %s waiting for %s", timeout, what)
			return
		case <-time.After(2 * time.Second):
		}
	}
}

// load reads a fixture the way render returns it, in this fixture's namespace.
func (k kind) load(path string) []render.Doc {
	b, err := os.ReadFile(path)
	if err != nil {
		k.t.Fatal(err)
	}
	ds := docs(k.t, string(b))
	for n := range ds {
		ds[n].Namespace = k.ns
	}

	return ds
}

func (k kind) apply(path string) (string, error) {
	return k.kubectlOut(k.ns, "apply", "-f", path)
}

func (k kind) applyDoc(d render.Doc) (string, error) {
	f := filepath.Join(k.t.TempDir(), "doc.yaml")
	b, err := yaml.Marshal(d.Object)
	if err != nil {
		k.t.Fatal(err)
	}
	if err := os.WriteFile(f, b, 0o644); err != nil {
		k.t.Fatal(err)
	}

	return k.apply(f)
}

func (k kind) kubectl(ns string, args ...string) {
	if out, err := k.kubectlOut(ns, args...); err != nil {
		k.t.Fatalf("kubectl %v: %v\n%s", args, err, out)
	}
}

func (k kind) kubectlOut(ns string, args ...string) (string, error) {
	pre := []string{"--context", k.ctx}
	if ns != "" {
		pre = append(pre, "-n", ns)
	}
	out, err := exec.Command("kubectl", append(pre, args...)...).CombinedOutput()

	return string(out), err
}

func isWorkload(kind string) bool {
	return kind == "Deployment" || kind == "StatefulSet" || kind == "DaemonSet"
}

func resourceOf(d render.Doc) string {
	if d.Kind == "StorageClass" {
		return d.Kind + " " + d.Name
	}

	return d.Kind + " " + d.Namespace + "/" + d.Name
}
