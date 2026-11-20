package change

import (
	"context"
	"fmt"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"
)

// podStart is the assumed time from scheduling to a started container, before readiness.
const podStart = 10 * time.Second

// Live refines impacts with what the cluster runs now: live and HPA replica counts, DaemonSet node
// counts, StorageClass expansion and rollout estimates. It only reads.
func Live(ctx context.Context, c kubernetes.Interface, is []Impact) error {
	hpas := map[string][]autoscalingv2.HorizontalPodAutoscaler{}
	for n := range is {
		i := &is[n]
		ns := i.doc.Namespace
		switch {
		case i.Verdict == Resize:
			if err := resize(ctx, c, i); err != nil {
				return err
			}
		case workloads[i.doc.Kind] && (i.Verdict == Restart || i.Verdict == Ordered || i.Verdict == Recreate):
			if _, ok := hpas[ns]; !ok {
				l, err := c.AutoscalingV2().HorizontalPodAutoscalers(ns).List(ctx, metav1.ListOptions{})
				if err != nil {
					return fmt.Errorf("list HPAs in %s: %w", ns, err)
				}
				hpas[ns] = l.Items
			}
			if err := rollout(ctx, c, i, hpas[ns]); err != nil {
				return err
			}
		}
	}
	sortImpacts(is)

	return nil
}

// rollout sets the pods a workload restarts from the cluster and estimates how long that takes.
// Objects not in the cluster yet keep the manifest's count.
func rollout(ctx context.Context, c kubernetes.Interface, i *Impact, hpas []autoscalingv2.HorizontalPodAutoscaler) error {
	d, opts := i.doc, metav1.GetOptions{}
	var (
		live int32
		err  error
	)
	switch d.Kind {
	case "Deployment":
		o, e := c.AppsV1().Deployments(d.Namespace).Get(ctx, d.Name, opts)
		if err = e; e == nil {
			live = deref(o.Spec.Replicas)
		}
	case "StatefulSet":
		o, e := c.AppsV1().StatefulSets(d.Namespace).Get(ctx, d.Name, opts)
		if err = e; e == nil {
			live = deref(o.Spec.Replicas)
		}
	case "DaemonSet":
		o, e := c.AppsV1().DaemonSets(d.Namespace).Get(ctx, d.Name, opts)
		if err = e; e == nil {
			live = o.Status.DesiredNumberScheduled
		}
	}
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

	ru := get(o, "spec", "strategy", "rollingUpdate")
	surge, unavailable := "25%", "25%"
	switch {
	case kind == "Deployment" && str(o, "spec", "strategy", "type") == "Recreate":
		return perBatch
	case kind == "StatefulSet":
		ru, surge, unavailable = get(o, "spec", "updateStrategy", "rollingUpdate"), "0", "1"
	case kind == "DaemonSet":
		ru, surge, unavailable = get(o, "spec", "updateStrategy", "rollingUpdate"), "0", "1"
	}
	if v := get(ru, "maxSurge"); v != nil {
		surge = fmt.Sprint(v)
	}
	if v := get(ru, "maxUnavailable"); v != nil {
		unavailable = fmt.Sprint(v)
	}

	s, _ := intstr.GetScaledValueFromIntOrPercent(new(intstr.Parse(surge)), pods, true)
	u, _ := intstr.GetScaledValueFromIntOrPercent(new(intstr.Parse(unavailable)), pods, false)
	batch := max(s+u, 1)

	return time.Duration((pods+batch-1)/batch) * perBatch
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
