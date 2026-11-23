package pod

import (
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	v1 "k8s.io/api/core/v1"
)

// WriteHuman writes, for a terminal, the node × check matrix of an unscheduled pod, or the problems of a pod on
// a node.
func WriteHuman(w io.Writer, p *v1.Pod, r Result) error {
	var b strings.Builder
	b.WriteString(heading(p, r))
	b.WriteByte('\n')

	if rows := problemRows(p, r); len(rows) > 0 {
		b.WriteByte('\n')
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "CONTAINER\tPROBLEM\tDETAIL")
		for _, row := range rows {
			fmt.Fprintln(tw, strings.TrimSuffix(strings.Join(row[:], "\t"), "\t"))
		}
		tw.Flush()

		writeEvidence(&b, r.Problems)
	}

	cols := failedPlugins(r)
	if len(cols) > 0 {
		b.WriteByte('\n')
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintf(tw, "NODE\t%s\n", strings.Join(cols, "\t"))
		for _, g := range groups(r, cols) {
			fmt.Fprintf(tw, "%s\t%s\n", g.label, strings.Join(g.cells, "\t"))
		}
		tw.Flush()

		b.WriteString("\nReasons:\n")
		for _, c := range cols {
			fmt.Fprintf(&b, "  %s: %s\n", c, strings.Join(reasons(r, c), "; "))
		}
	}

	if len(r.Preempt) > 0 {
		b.WriteString("\n")
		b.WriteString(preempt(r))
		b.WriteByte('\n')
	}

	if len(r.Sources) > 0 {
		b.WriteString("\nSource:\n")
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		for _, s := range r.Sources {
			fmt.Fprintf(tw, "  %s\t%s = %v\t← %s\n", s.label(), s.Field, s.Value, s.From)
		}
		tw.Flush()
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// WriteMarkdown writes the matrix or the problems as Markdown tables.
func WriteMarkdown(w io.Writer, p *v1.Pod, r Result) error {
	var b strings.Builder
	b.WriteString("**")
	b.WriteString(heading(p, r))
	b.WriteString("**\n")

	if rows := problemRows(p, r); len(rows) > 0 {
		b.WriteString("\n| Container | Problem | Detail |\n|---|---|---|\n")
		for _, row := range rows {
			fmt.Fprintf(&b, "| `%s` | %s | %s |\n", row[0], row[1], row[2])
		}
		writeEvidence(&b, r.Problems)
	}

	cols := failedPlugins(r)
	if len(cols) > 0 {
		fmt.Fprintf(&b, "\n| Node | %s |\n|---|%s\n", strings.Join(cols, " | "), strings.Repeat("---|", len(cols)))
		for _, g := range groups(r, cols) {
			fmt.Fprintf(&b, "| `%s` | %s |\n", g.label, strings.Join(g.cells, " | "))
		}

		b.WriteString("\n")
		for _, c := range cols {
			fmt.Fprintf(&b, "- **%s**: %s\n", c, strings.Join(reasons(r, c), "; "))
		}
	}

	if len(r.Preempt) > 0 {
		b.WriteString("\n")
		b.WriteString(preempt(r))
		b.WriteByte('\n')
	}

	if len(r.Sources) > 0 {
		b.WriteString("\n**Source:**\n\n")
		for _, s := range r.Sources {
			fmt.Fprintf(&b, "- %s: `%s` = `%v` ← %s\n", s.label(), s.Field, s.Value, s.From)
		}
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// WriteJSON writes the result as kwhy.pod/v1 JSON, every node listed on its own. A pod already on a node has
// "scheduled" set to that node and no plugins or nodes.
func WriteJSON(w io.Writer, p *v1.Pod, r Result) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(struct {
		Schema    string    `json:"schema"`
		Namespace string    `json:"namespace"`
		Pod       string    `json:"pod"`
		Template  bool      `json:"template"`
		Scheduled string    `json:"scheduled,omitempty"`
		Plugins   []string  `json:"plugins,omitempty"`
		Nodes     []Node    `json:"nodes,omitempty"`
		Preempt   []string  `json:"preempt,omitempty"`
		Node      string    `json:"preemptNode,omitempty"`
		Victims   []string  `json:"victims,omitempty"`
		PDBs      int64     `json:"pdbViolations,omitempty"`
		Sampled   bool      `json:"preemptSampled,omitempty"`
		Problems  []Problem `json:"problems,omitempty"`
		Sources   []Source  `json:"sources,omitempty"`
	}{"kwhy.pod/v1", p.Namespace, p.Name, IsTemplate(p), p.Spec.NodeName, r.Plugins, r.Nodes, r.Preempt,
		r.PreemptNode, r.Victims, r.PDBViolations, r.PreemptSampled, r.Problems, r.Sources})
}

func preempt(r Result) string {
	s := fmt.Sprintf("Preempting lower-priority pods would make room on %s (%s).", plural(len(r.Preempt), "node"), strings.Join(r.Preempt, ", "))
	if r.PreemptNode == "" {
		s += " Several tie by the scheduler's rules, so it picks one of those at random."
	} else {
		s += fmt.Sprintf(" The scheduler would pick %s and evict %s.", r.PreemptNode, strings.Join(r.Victims, ", "))
	}
	if r.PDBViolations > 0 {
		s += fmt.Sprintf(" That breaks %s; it goes ahead because every node here would break one.", plural(int(r.PDBViolations), "PodDisruptionBudget"))
	}
	if r.PreemptSampled {
		s += " On a cluster this size the scheduler checks only a random sample of these nodes, so it may pick another."
	}

	return s
}

// heading is the first line: a pod on a node, a pod being scheduled, or a workload only its controller speaks for.
func heading(p *v1.Pod, r Result) string {
	switch {
	case p.Spec.NodeName != "":
		return onNode(p, r)
	case len(r.Nodes) == 0:
		return fmt.Sprintf("%s/%s · no pod to check · %s from its controller", p.Namespace, p.Name, plural(len(r.Problems), "problem"))
	}

	return title(p, r)
}

// onNode is the title for a pod on a node: "ml/api-1 on n1 · Running · not ready · 2 containers, 1 failing".
func onNode(p *v1.Pod, r Result) string {
	failing := map[string]bool{}
	for _, pr := range r.Problems {
		if !pr.Earlier {
			failing[pr.Container] = true
		}
	}
	if len(failing) == 0 {
		return fmt.Sprintf("%s/%s is on %s, %s and ready.", p.Namespace, p.Name, p.Spec.NodeName, p.Status.Phase)
	}

	state := "ready"
	if !ready(p) {
		state = "not ready"
	}
	delete(failing, "")

	return fmt.Sprintf("%s/%s on %s · %s · %s · %s, %d failing", p.Namespace, p.Name, p.Spec.NodeName, p.Status.Phase, state,
		plural(len(p.Spec.Containers), "container"), len(failing))
}

// problemRows are one row per problem, then a ✓ row for each app container without one.
func problemRows(p *v1.Pod, r Result) [][3]string {
	if len(r.Problems) == 0 {
		return nil
	}

	var rows [][3]string
	seen := map[string]bool{}
	for _, pr := range r.Problems {
		name, kind := pr.Container, pr.Kind
		switch {
		case name == "" && (pr.Kind == Create || pr.Kind == Rollout || pr.Kind == JobFail):
			name = "(controller)"
		case name == "" && pr.Kind == Scaler:
			name = "(autoscaler)"
		case name == "":
			name = "(pod)"
		case pr.Init:
			name += " (init)"
		}
		if pr.Earlier {
			kind += " (earlier)"
		}
		rows = append(rows, [3]string{name, kind, pr.Detail})
		seen[pr.Container] = true
	}
	for _, c := range p.Spec.Containers {
		if !seen[c.Name] {
			rows = append(rows, [3]string{c.Name, "✓", ""})
		}
	}

	return rows
}

// writeEvidence lists the events and logs behind the problems.
func writeEvidence(b *strings.Builder, ps []Problem) {
	var events []string
	for _, pr := range ps {
		for _, e := range pr.Events {
			events = append(events, fmt.Sprintf("  %s: %s", or(pr.Container, "pod"), e))
		}
	}
	if len(events) > 0 {
		b.WriteString("\nEvents:\n")
		b.WriteString(strings.Join(events, "\n"))
		b.WriteByte('\n')
	}

	for _, pr := range ps {
		if len(pr.Logs) > 0 {
			fmt.Fprintf(b, "\nLogs (%s, last run):\n", pr.Container)
			for _, l := range pr.Logs {
				b.WriteString("  ")
				b.WriteString(l)
				b.WriteByte('\n')
			}
		}
	}
}

func or(s, def string) string {
	if s == "" {
		return def
	}

	return s
}

func plural(n int, s string) string {
	if n == 1 {
		return "1 " + s
	}

	return fmt.Sprintf("%d %ss", n, s)
}

type group struct {
	label string
	cells []string
}

func title(p *v1.Pod, r Result) string {
	fit := 0
	for _, n := range r.Nodes {
		if len(n.Failed) == 0 {
			fit++
		}
	}

	state := fmt.Sprintf("%s/%s Pending %s", p.Namespace, p.Name, age(time.Since(p.CreationTimestamp.Time)))
	if IsTemplate(p) {
		state = fmt.Sprintf("%s/%s has no pod waiting; a new one from its template", p.Namespace, p.Name)
	}

	return fmt.Sprintf("%s · %d of %d nodes fit · every check, every node", state, fit, len(r.Nodes))
}

// failedPlugins are the plugins that fail on some node, in the scheduler's order.
func failedPlugins(r Result) []string {
	var cols []string
	for _, pl := range r.Plugins {
		for _, n := range r.Nodes {
			if slices.ContainsFunc(n.Failed, func(c Check) bool { return c.Plugin == pl }) {
				cols = append(cols, pl)
				break
			}
		}
	}

	return cols
}

// groups merges nodes with identical cells, in node name order.
func groups(r Result, cols []string) []group {
	var (
		order []string
		names = map[string][]string{}
		cells = map[string][]string{}
	)
	for _, n := range r.Nodes {
		row := make([]string, len(cols))
		for i, c := range cols {
			row[i] = "✓"
			if j := slices.IndexFunc(n.Failed, func(f Check) bool { return f.Plugin == c }); j >= 0 {
				row[i] = strings.TrimSpace("✗ " + n.Failed[j].Detail)
			}
		}

		key := strings.Join(row, "\x00")
		if _, ok := cells[key]; !ok {
			order = append(order, key)
			cells[key] = row
		}
		names[key] = append(names[key], n.Name)
	}

	gs := make([]group, 0, len(order))
	for _, k := range order {
		gs = append(gs, group{label(names[k]), cells[k]})
	}

	return gs
}

// label is the node name, or for several nodes their common prefix and count: "cpu-* (9)", else "9 nodes".
func label(names []string) string {
	if len(names) == 1 {
		return names[0]
	}

	prefix := names[0]
	for _, n := range names[1:] {
		for !strings.HasPrefix(n, prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	if prefix == "" {
		return fmt.Sprintf("%d nodes", len(names))
	}

	return fmt.Sprintf("%s* (%d)", prefix, len(names))
}

// reasons are the plugin's distinct reasons over all nodes, in node order.
func reasons(r Result, plugin string) []string {
	var rs []string
	for _, n := range r.Nodes {
		for _, c := range n.Failed {
			if c.Plugin != plugin {
				continue
			}
			for _, s := range c.Reasons {
				if !slices.Contains(rs, s) {
					rs = append(rs, s)
				}
			}
		}
	}

	return rs
}

func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// WriteHealthy writes the answer for a workload whose pods are all ready, in output (human, markdown or json).
func WriteHealthy(w io.Writer, output, namespace, workload string, h Healthy) error {
	line := fmt.Sprintf("%s/%s: %s.", namespace, workload, h)

	switch output {
	case "json":
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(struct {
			Schema    string `json:"schema"`
			Namespace string `json:"namespace"`
			Workload  string `json:"workload"`
			Ready     int    `json:"ready"`
		}{"kwhy.pod/v1", namespace, workload, h.Pods})
	case "markdown":
		line = "**" + line + "**"
	}

	_, err := fmt.Fprintln(w, line)
	return err
}
