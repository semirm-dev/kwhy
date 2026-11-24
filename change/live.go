package change

import (
	"context"
	"fmt"
	"slices"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	"github.com/semirm-dev/kwhy/render"
)

// podStart is the assumed time from scheduling to a started container, before readiness.
const podStart = 10 * time.Second

// Live refines impacts with what the cluster runs now: live and HPA replica counts, DaemonSet node
// counts, StorageClass expansion and rollout estimates. It only reads.
func Live(ctx context.Context, c kubernetes.Interface, is []Impact) error {
	hpas := map[string][]autoscalingv2.HorizontalPodAutoscaler{}
	hpasIn := func(ns string) ([]autoscalingv2.HorizontalPodAutoscaler, error) {
		if _, ok := hpas[ns]; !ok {
			l, err := c.AutoscalingV2().HorizontalPodAutoscalers(ns).List(ctx, metav1.ListOptions{})
			if err != nil {
				return nil, fmt.Errorf("list HPAs in %s: %w", ns, err)
			}
			hpas[ns] = l.Items
		}
		return hpas[ns], nil
	}
	quotas := map[string][]corev1.ResourceQuota{}
	for n := range is {
		i := &is[n]
		ns := i.doc.Namespace
		switch {
		case i.Verdict == Resize:
			if err := resize(ctx, c, i); err != nil {
				return err
			}
		case i.Verdict == NoEffect && i.doc.Kind == "StatefulSet" && str(i.doc.Object, "spec", "updateStrategy", "type") != "OnDelete":
			// The partition is at or above the manifest's replicas. An HPA, or the live count when the manifest
			// leaves replicas unset, can run pods above it; with replicas set and no HPA, apply resets the count.
			hs, err := hpasIn(ns)
			if err != nil {
				return err
			}
			scaled := slices.ContainsFunc(hs, func(h autoscalingv2.HorizontalPodAutoscaler) bool {
				return h.Spec.ScaleTargetRef.Kind == i.doc.Kind && h.Spec.ScaleTargetRef.Name == i.doc.Name
			})
			if !scaled && get(i.doc.Object, "spec", "replicas") != nil {
				continue
			}
			partition, _ := get(i.doc.Object, "spec", "updateStrategy", "rollingUpdate", "partition").(int)
			o := *i
			o.Verdict, o.Detail = Ordered, fmt.Sprintf("one at a time, ordinals %d and up", partition)
			if err := rollout(ctx, c, &o, hs); err != nil {
				return err
			}
			if o.Pods > 0 {
				*i = o
			}
		case workloads[i.doc.Kind] && (i.Verdict == Restart || i.Verdict == Ordered || i.Verdict == Recreate):
			if _, err := hpasIn(ns); err != nil {
				return err
			}
			if _, ok := quotas[ns]; !ok {
				l, err := c.CoreV1().ResourceQuotas(ns).List(ctx, metav1.ListOptions{})
				if err != nil {
					return fmt.Errorf("list ResourceQuotas in %s: %w", ns, err)
				}
				quotas[ns] = l.Items
			}
			if err := rollout(ctx, c, i, hpas[ns]); err != nil {
				return err
			}
			surgeQuota(i, quotas[ns])
		}
	}
	sortImpacts(is)

	return nil
}

// Drift finds workloads whose running images differ from the head render while the change doesn't touch them:
// someone changed them outside git (kubectl set image), and applying head puts the rendered image back, which
// restarts the pods though the diff shows no image change. It only reads; is are the impacts Diff found.
func Drift(ctx context.Context, c kubernetes.Interface, head []render.Doc, is []Impact) ([]Impact, error) {
	changed := map[string]bool{}
	for _, i := range is {
		changed[i.Resource] = true
	}

	var out []Impact
	for _, d := range head {
		if !workloads[d.Kind] || changed[name(d)] {
			continue
		}

		live, pods, err := running(ctx, c, d)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("get %s: %w", name(d), err)
		}

		rendered := map[string]string{}
		for _, ctr := range list(get(d.Object, "spec", "template", "spec", "containers")) {
			rendered[str(ctr, "name")] = str(ctr, "image")
		}
		for _, ctr := range live {
			if want, ok := rendered[ctr.Name]; ok && want != ctr.Image {
				out = append(out, Impact{Verdict: Restart, Resource: name(d), Pods: int(pods), doc: d,
					Detail: fmt.Sprintf("live runs %s (changed outside git); applying puts back %s", ctr.Image, want)})
				break
			}
		}
	}

	return out, nil
}

// rollout sets the pods a workload restarts from the cluster and estimates how long that takes.
// Objects not in the cluster yet keep the manifest's count.
func rollout(ctx context.Context, c kubernetes.Interface, i *Impact, hpas []autoscalingv2.HorizontalPodAutoscaler) error {
	d := i.doc
	_, live, err := running(ctx, c, d)
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("get %s: %w", i.Resource, err)
	}

	pods := i.Pods
	if err == nil {
		pods = int(live)
	}
	for _, h := range hpas {
		if h.Spec.ScaleTargetRef.Kind == d.Kind && h.Spec.ScaleTargetRef.Name == d.Name {
			pods = int(h.Status.CurrentReplicas)
			i.Detail += fmt.Sprintf(", HPA %d–%d", deref(h.Spec.MinReplicas), h.Spec.MaxReplicas)
		}
	}

	// A partition spares the ordinals below it.
	if partition, _ := get(d.Object, "spec", "updateStrategy", "rollingUpdate", "partition").(int); partition > 0 {
		pods = max(pods-partition, 0)
	}
	if d.Kind == "DaemonSet" {
		i.Detail = plural(pods, "node")
	}

	i.Pods = pods
	if t := estimate(d.Kind, d.Object, pods); t > 0 {
		i.Seconds = int(t.Seconds())
		i.Detail += ", ~" + minutes(t)
	}

	return nil
}

// running is the live workload's containers and pods: its replicas, or for a DaemonSet the nodes it should run on.
func running(ctx context.Context, c kubernetes.Interface, d render.Doc) ([]corev1.Container, int32, error) {
	opts := metav1.GetOptions{}
	switch d.Kind {
	case "Deployment":
		o, err := c.AppsV1().Deployments(d.Namespace).Get(ctx, d.Name, opts)
		if err != nil {
			return nil, 0, err
		}
		return o.Spec.Template.Spec.Containers, deref(o.Spec.Replicas), nil
	case "StatefulSet":
		o, err := c.AppsV1().StatefulSets(d.Namespace).Get(ctx, d.Name, opts)
		if err != nil {
			return nil, 0, err
		}
		return o.Spec.Template.Spec.Containers, deref(o.Spec.Replicas), nil
	case "DaemonSet":
		o, err := c.AppsV1().DaemonSets(d.Namespace).Get(ctx, d.Name, opts)
		if err != nil {
			return nil, 0, err
		}
		return o.Spec.Template.Spec.Containers, o.Status.DesiredNumberScheduled, nil
	}

	return nil, 0, nil
}

// surgeQuota notes when a Deployment's surge pods don't fit a ResourceQuota of the namespace: the new ReplicaSet
// can't create them, so the rollout stalls or runs slower than estimated.
func surgeQuota(i *Impact, quotas []corev1.ResourceQuota) {
	d := i.doc
	if d.Kind != "Deployment" || len(quotas) == 0 {
		return
	}

	surge, _ := batches(d.Kind, d.Object, i.Pods)
	if surge == 0 {
		return
	}

	// What one pod of the template counts against a quota.
	per := map[corev1.ResourceName]resource.Quantity{corev1.ResourcePods: resource.MustParse("1")}
	for _, ctr := range list(get(d.Object, "spec", "template", "spec", "containers")) {
		for _, r := range []string{"cpu", "memory"} {
			q, err := resource.ParseQuantity(fmt.Sprint(get(ctr, "resources", "requests", r)))
			if err != nil {
				continue
			}
			for _, name := range []corev1.ResourceName{corev1.ResourceName(r), corev1.ResourceName("requests." + r)} {
				sum := per[name]
				sum.Add(q)
				per[name] = sum
			}
		}
	}

	for _, q := range quotas {
		for name, hard := range q.Status.Hard {
			one, ok := per[name]
			if !ok {
				continue
			}
			need := one.DeepCopy()
			need.Mul(int64(surge))
			left := hard.DeepCopy()
			left.Sub(q.Status.Used[name])
			if left.Cmp(need) < 0 {
				used := q.Status.Used[name]
				i.Detail += fmt.Sprintf(", surge of %s exceeds ResourceQuota %s (%s: %s of %s used, needs +%s)",
					plural(surge, "pod"), q.Name, name, used.String(), hard.String(), need.String())
				return
			}
		}
	}
}

// estimate is the rollout time: batches of pods, each taking pod start, readiness and minReadySeconds.
func estimate(kind string, o obj, pods int) time.Duration {
	if pods == 0 {
		return 0
	}

	tpl := get(o, "spec", "template", "spec")
	ready := time.Duration(0)
	for _, ctr := range list(get(tpl, "containers")) {
		p := get(ctr, "readinessProbe")
		if p == nil {
			continue
		}
		t := time.Duration(num(p, "initialDelaySeconds", 0)+num(p, "periodSeconds", 10)*num(p, "successThreshold", 1)) * time.Second
		ready = max(ready, t)
	}
	perBatch := podStart + ready + time.Duration(num(get(o, "spec"), "minReadySeconds", 0))*time.Second

	if kind == "Deployment" && str(o, "spec", "strategy", "type") == "Recreate" {
		return perBatch
	}
	_, batch := batches(kind, o, pods)

	return time.Duration((pods+batch-1)/batch) * perBatch
}

// batches is the surge (extra pods at once) and the batch size (surge + unavailable, at least 1) of a rolling
// update of pods.
func batches(kind string, o obj, pods int) (surge, batch int) {
	ru := get(o, "spec", "strategy", "rollingUpdate")
	s, unavailable := "25%", "25%"
	switch {
	case kind == "Deployment" && str(o, "spec", "strategy", "type") == "Recreate":
		return 0, pods
	case kind == "StatefulSet" || kind == "DaemonSet":
		ru, s, unavailable = get(o, "spec", "updateStrategy", "rollingUpdate"), "0", "1"
	}
	if v := get(ru, "maxSurge"); v != nil {
		s = fmt.Sprint(v)
	}
	if v := get(ru, "maxUnavailable"); v != nil {
		unavailable = fmt.Sprint(v)
	}

	surge, _ = intstr.GetScaledValueFromIntOrPercent(new(intstr.Parse(s)), pods, true)
	u, _ := intstr.GetScaledValueFromIntOrPercent(new(intstr.Parse(unavailable)), pods, false)

	return surge, max(surge+u, 1)
}

// resize turns a claim's growth into a failure when the claim isn't bound or its StorageClass can't expand.
func resize(ctx context.Context, c kubernetes.Interface, i *Impact) error {
	d := i.doc
	pvc, err := c.CoreV1().PersistentVolumeClaims(d.Namespace).Get(ctx, d.Name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("get %s: %w", i.Resource, err)
	}

	if pvc.Status.Phase != "Bound" {
		i.Verdict, i.Detail = WillFail, "claim isn't bound, its spec is immutable"
		return nil
	}

	name := deref(pvc.Spec.StorageClassName)
	if name == "" {
		return nil
	}

	sc, err := c.StorageV1().StorageClasses().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get StorageClass %s: %w", name, err)
	}

	if !deref(sc.AllowVolumeExpansion) {
		i.Verdict, i.Detail = WillFail, "StorageClass "+name+" doesn't allow expansion"
		return nil
	}
	to := storage(d.Object)
	i.Detail = pvc.Spec.Resources.Requests.Storage().String() + " → " + to.String()

	return nil
}

func minutes(t time.Duration) string {
	if t < time.Minute {
		return fmt.Sprintf("%ds", int(t.Seconds()))
	}

	return fmt.Sprintf("%dm", int((t+time.Minute-1)/time.Minute))
}

func num(o any, key string, def int) int {
	if v, ok := get(o, key).(int); ok {
		return v
	}

	return def
}

func deref[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}

	return *p
}
