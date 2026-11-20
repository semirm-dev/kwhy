package pod

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// Problem kinds. The strings are part of the kwhy.pod/v1 JSON contract.
const (
	Pull    = "pull"    // the image can't be pulled
	Crash   = "crash"   // the container exits with an error
	OOM     = "oom"     // the container was killed at its memory limit
	Config  = "config"  // a Secret, ConfigMap or key the container needs is missing
	Mount   = "mount"   // a volume can't be mounted or attached
	Probe   = "probe"   // a readiness, liveness or startup probe fails
	Evicted = "evicted" // the kubelet evicted the pod
	Sandbox = "sandbox" // the node can't create the pod's sandbox (network, runtime)
	NodeBad = "node"    // the pod's node isn't Ready or is under pressure
	Other   = "other"   // not running or not ready for a reason not listed above
)

// maxEvents caps the event messages kept per problem.
const maxEvents = 3

// Problem is one reason a pod on a node isn't running or ready, as kubelet reported it.
type Problem struct {
	// Container is empty for pod-level problems (evicted, mount, sandbox).
	Container string `json:"container,omitempty"`
	Init      bool   `json:"init,omitempty"`
	Kind      string `json:"kind"`
	// Reason is kubelet's, e.g. CrashLoopBackOff, OOMKilled, Unhealthy.
	Reason string `json:"reason"`
	Detail string `json:"detail"`
	// Earlier marks a past failure of a container that is running and ready now.
	Earlier bool `json:"earlier,omitempty"`
	// Events are the matching Warning event messages, newest first.
	Events []string `json:"events,omitempty"`
	// Logs are the last lines of the previous container instance, with --logs only.
	Logs []string `json:"logs,omitempty"`
}

// Diagnose says why p, which is on a node, isn't running or ready, from its status and recent events. Events
// expire (an hour by default), so every problem the status shows is found without them.
func Diagnose(ctx context.Context, c kubernetes.Interface, p *v1.Pod) ([]Problem, error) {
	list, err := c.CoreV1().Events(p.Namespace).List(ctx, metav1.ListOptions{FieldSelector: "involvedObject.uid=" + string(p.UID)})
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}

	evs := list.Items
	slices.SortFunc(evs, func(a, b v1.Event) int { return lastSeen(b).Compare(lastSeen(a)) })

	if p.Status.Phase == v1.PodFailed && p.Status.Reason == "Evicted" {
		return []Problem{{Kind: Evicted, Reason: p.Status.Reason, Detail: p.Status.Message}}, nil
	}

	var ps []Problem
	for _, cs := range p.Status.InitContainerStatuses {
		if pr, ok := container(p, cs, true, evs); ok {
			ps = append(ps, pr)
		}
	}
	for _, cs := range p.Status.ContainerStatuses {
		if pr, ok := container(p, cs, false, evs); ok {
			ps = append(ps, pr)
		}
	}

	// Mount and sandbox failures only show in events, while the containers wait in ContainerCreating.
	for _, pl := range []struct {
		kind    string
		reasons []string
	}{{Mount, []string{"FailedMount", "FailedAttachVolume"}}, {Sandbox, []string{"FailedCreatePodSandBox"}}} {
		if msgs := messages(evs, "", pl.reasons...); len(msgs) > 0 && !ready(p) {
			ps = append(ps, Problem{Kind: pl.kind, Reason: pl.reasons[0], Detail: msgs[0], Events: msgs})
		}
	}

	if pr, ok := nodeProblem(ctx, c, p.Spec.NodeName); ok {
		ps = append(ps, pr)
	}

	if len(ps) == 0 && !ready(p) {
		ps = append(ps, Problem{Kind: Other, Reason: string(p.Status.Phase), Detail: "not ready; kubelet reports no reason"})
	}

	return ps, nil
}

// nodeProblem names the node's condition when it isn't Ready or is under pressure. Best effort: many users can't
// read nodes, so any error means no problem.
func nodeProblem(ctx context.Context, c kubernetes.Interface, name string) (Problem, bool) {
	n, err := c.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return Problem{}, false
	}

	var bad []string
	reason := ""
	for _, cond := range n.Status.Conditions {
		notReady := cond.Type == v1.NodeReady && cond.Status != v1.ConditionTrue
		pressure := cond.Type != v1.NodeReady && cond.Status == v1.ConditionTrue && strings.HasSuffix(string(cond.Type), "Pressure")
		if notReady || pressure {
			bad = append(bad, fmt.Sprintf("%s=%s (%s)", cond.Type, cond.Status, cond.Message))
			reason = or(reason, cond.Reason)
		}
	}
	if len(bad) == 0 {
		return Problem{}, false
	}

	return Problem{Kind: NodeBad, Reason: reason, Detail: "node " + name + ": " + strings.Join(bad, "; ")}, true
}

// container returns the problem of one container, if it has one.
func container(p *v1.Pod, cs v1.ContainerStatus, init bool, evs []v1.Event) (Problem, bool) {
	pr := Problem{Container: cs.Name, Init: init}
	restarts := fmt.Sprintf("%d restarts", cs.RestartCount)
	last := cs.LastTerminationState.Terminated

	switch w, t := cs.State.Waiting, cs.State.Terminated; {
	case w != nil:
		pr.Reason = w.Reason
		switch w.Reason {
		case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "ErrImageNeverPull":
			pr.Kind, pr.Detail = Pull, cs.Image
			pr.Events = messages(evs, cs.Name, "Failed")
		case "CrashLoopBackOff":
			pr.Kind, pr.Detail = Crash, restarts
			if last != nil {
				pr.Kind, pr.Detail = exited(p, cs.Name, init, last, restarts)
			}
			pr.Events = messages(evs, cs.Name, "BackOff")
		case "CreateContainerConfigError", "CreateContainerError":
			pr.Kind, pr.Detail = Config, w.Message
		case "ContainerCreating", "PodInitializing":
			return pr, false // a mount or sandbox problem, if any, shows at pod level
		default:
			pr.Kind, pr.Detail = Other, w.Message
		}
	case t != nil:
		// A finished init container or a finished container of a Job is fine when it exited 0.
		if t.ExitCode == 0 {
			return pr, false
		}
		pr.Reason = t.Reason
		pr.Kind, pr.Detail = exited(p, cs.Name, init, t, restarts)
	case !init && !cs.Ready:
		pr.Kind, pr.Reason, pr.Detail = Other, "NotReady", "running, not ready"
		if msgs := messages(evs, cs.Name, "Unhealthy"); len(msgs) > 0 {
			pr.Kind, pr.Reason, pr.Detail, pr.Events = Probe, "Unhealthy", msgs[0], msgs
		}
	case last != nil && last.Reason == "OOMKilled":
		pr.Earlier, pr.Reason = true, last.Reason
		pr.Kind, pr.Detail = exited(p, cs.Name, init, last, restarts)
	default:
		return pr, false
	}

	return pr, true
}

// exited describes a terminated container: an OOM kill with the limit it hit, else the exit code.
func exited(p *v1.Pod, name string, init bool, t *v1.ContainerStateTerminated, restarts string) (kind, detail string) {
	ago := ""
	if !t.FinishedAt.IsZero() {
		ago = ", last " + age(time.Since(t.FinishedAt.Time)) + " ago"
	}

	if t.Reason == "OOMKilled" {
		limit := "no memory limit"
		if c := spec(p, name, init); c != nil {
			if l, ok := c.Resources.Limits[v1.ResourceMemory]; ok {
				limit = "limit " + bytes(l.Value())
			}
		}
		return OOM, fmt.Sprintf("killed at %s, %s%s", limit, restarts, ago)
	}

	return Crash, fmt.Sprintf("exit %d, %s%s", t.ExitCode, restarts, ago)
}

func spec(p *v1.Pod, name string, init bool) *v1.Container {
	cs := p.Spec.Containers
	if init {
		cs = p.Spec.InitContainers
	}

	for i := range cs {
		if cs[i].Name == name {
			return &cs[i]
		}
	}

	return nil
}

// messages are the distinct Warning messages with one of reasons, newest first, for container (empty: any).
func messages(evs []v1.Event, container string, reasons ...string) []string {
	var out []string
	for _, e := range evs {
		if e.Type != v1.EventTypeWarning || !slices.Contains(reasons, e.Reason) || slices.Contains(out, e.Message) {
			continue
		}
		if container != "" && !strings.HasSuffix(e.InvolvedObject.FieldPath, "{"+container+"}") {
			continue
		}
		if out = append(out, e.Message); len(out) == maxEvents {
			break
		}
	}

	return out
}

func lastSeen(e v1.Event) time.Time {
	if !e.LastTimestamp.IsZero() {
		return e.LastTimestamp.Time
	}
	if e.EventTime.Time.IsZero() {
		return e.CreationTimestamp.Time
	}

	return e.EventTime.Time
}

// AddLogs fills Logs of each current crash or OOM problem with the last n lines of the container's last run.
// A container that crashed is waiting or terminated, so its latest instance is the run that died; the previous
// instance is tried when that has no logs. Logs are best effort: when kubelet has none, the line says why.
func AddLogs(ctx context.Context, c kubernetes.Interface, p *v1.Pod, ps []Problem, n int64) {
	for i := range ps {
		pr := &ps[i]
		if pr.Earlier || pr.Kind != Crash && pr.Kind != OOM {
			continue
		}

		var msg string
		for _, previous := range []bool{false, true} {
			raw, err := c.CoreV1().Pods(p.Namespace).GetLogs(p.Name,
				&v1.PodLogOptions{Container: pr.Container, Previous: previous, TailLines: &n}).DoRaw(ctx)
			if err != nil {
				msg = err.Error()
				continue
			}
			// Kubelet answers 200 with this text when the instance's log file is gone.
			if msg = strings.TrimSpace(string(raw)); msg == "" || strings.HasPrefix(msg, "unable to retrieve container logs") {
				continue
			}
			pr.Logs = strings.Split(msg, "\n")
			break
		}
		if pr.Logs == nil {
			pr.Logs = []string{"(no logs: " + or(msg, "empty") + ")"}
		}
	}
}
