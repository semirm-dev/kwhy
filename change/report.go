package change

import (
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

var icons = map[Verdict]string{
	Restart: "↻", Ordered: "⏱", Scale: "⇅", Recreate: "⟲", Hook: "⚙", NoEffect: "!", Delayed: "~", PickedUp: "✓",
	WillFail: "✗", Data: "⚠", Create: "+", Delete: "-",
}

// WriteHuman writes impacts for a terminal.
func WriteHuman(w io.Writer, is []Impact) error {
	if len(is) == 0 {
		_, err := fmt.Fprintln(w, "No effect on running workloads.")
		return err
	}

	var b strings.Builder
	b.WriteString("Merging this change will:\n")

	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	for _, i := range is {
		fmt.Fprintf(tw, "%s %s\t%s\t%s\t%s\n", icons[i.Verdict], i.Verdict, i.Resource, pods(i.Pods), i.Detail)
	}
	tw.Flush()

	if s := summary(is); s != "" {
		b.WriteString(s)
		b.WriteByte('\n')
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// WriteMarkdown writes impacts as a PR comment.
func WriteMarkdown(w io.Writer, is []Impact) error {
	if len(is) == 0 {
		_, err := fmt.Fprintln(w, "**No effect on running workloads.**")
		return err
	}

	var b strings.Builder
	b.WriteString("**Merging this PR will:**\n\n| | Verdict | Resource | Pods | Detail |\n|---|---|---|---|---|\n")
	for _, i := range is {
		fmt.Fprintf(&b, "| %s | %s | `%s` | %s | %s |\n", icons[i.Verdict], i.Verdict, i.Resource, pods(i.Pods), i.Detail)
	}

	if s := summary(is); s != "" {
		fmt.Fprintf(&b, "\n%s\n", s)
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// summary totals restarted pods, apply errors and data warnings, skipping zeros.
func summary(is []Impact) string {
	var restarted, failed, data int
	for _, i := range is {
		switch i.Verdict {
		case Restart, Ordered, Recreate:
			restarted += i.Pods
		case WillFail:
			failed++
		case Data:
			data++
		}
	}

	var parts []string
	if restarted > 0 {
		parts = append(parts, pods(restarted)+" restarted")
	}
	if failed > 0 {
		parts = append(parts, plural(failed, "apply error"))
	}
	if data > 0 {
		parts = append(parts, plural(data, "data warning"))
	}

	return strings.Join(parts, " · ")
}

func pods(n int) string {
	if n == 0 {
		return ""
	}

	return plural(n, "pod")
}

func plural(n int, s string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", s)
	}

	return fmt.Sprintf("%d %ss", n, s)
}
