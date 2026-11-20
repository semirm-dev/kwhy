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

// WriteHuman writes the node × check matrix for a terminal.
func WriteHuman(w io.Writer, p *v1.Pod, r Result) error {
	var b strings.Builder
	b.WriteString(title(p, r) + "\n")

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

	if len(r.Sources) > 0 {
		b.WriteString("\nSource:\n")
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		for _, s := range r.Sources {
			fmt.Fprintf(tw, "  %s\t%s = %v\t← %s\n", s.Plugin, s.Field, s.Value, s.From)
		}
		tw.Flush()
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// WriteMarkdown writes the matrix as a Markdown table.
func WriteMarkdown(w io.Writer, p *v1.Pod, r Result) error {
	var b strings.Builder
	b.WriteString("**" + title(p, r) + "**\n")

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

	if len(r.Sources) > 0 {
		b.WriteString("\n**Source:**\n\n")
		for _, s := range r.Sources {
			fmt.Fprintf(&b, "- %s: `%s` = `%v` ← %s\n", s.Plugin, s.Field, s.Value, s.From)
		}
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// WriteJSON writes the result as kwhy.pod/v1 JSON, every node listed on its own.
func WriteJSON(w io.Writer, p *v1.Pod, r Result) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(struct {
		Schema    string   `json:"schema"`
		Namespace string   `json:"namespace"`
		Pod       string   `json:"pod"`
		Template  bool     `json:"template"`
		Plugins   []string `json:"plugins"`
		Nodes     []Node   `json:"nodes"`
		Sources   []Source `json:"sources,omitempty"`
	}{"kwhy.pod/v1", p.Namespace, p.Name, p.UID == templateUID, r.Plugins, r.Nodes, r.Sources})
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
	if p.UID == templateUID {
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
