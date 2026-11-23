package change

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"
)

const (
	// maxComment is GitHub's PR comment limit, in characters.
	maxComment = 65000
	more       = "\n…and %d more, run `kwhy change` locally for the full list.\n"
)

var icons = map[Verdict]string{
	Restart: "↻", Ordered: "⏱", Scale: "⇅", Resize: "⤢", Recreate: "⟲", Hook: "⚙", NoEffect: "!", Delayed: "~", PickedUp: "✓",
	WillFail: "✗", Orphaned: "⊘", Data: "⚠", Create: "+", Delete: "-",
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

// WriteMarkdown writes impacts as a PR comment. Past GitHub's comment limit it keeps failures, data
// warnings and recreates in view and folds the rest into a collapsed list, cut to fit.
func WriteMarkdown(w io.Writer, is []Impact) error {
	if len(is) == 0 {
		_, err := fmt.Fprintln(w, "**No effect on running workloads.**")
		return err
	}

	const head, tableHead = "**Merging this PR will:**\n\n", "| | Verdict | Resource | Pods | Detail |\n|---|---|---|---|---|\n"
	foot := ""
	if s := summary(is); s != "" {
		foot = "\n" + s + "\n"
	}

	var b strings.Builder
	b.WriteString(head + tableHead)
	for _, i := range is {
		b.WriteString(row(i))
	}
	b.WriteString(foot)

	if b.Len() > maxComment {
		var top, rest []Impact
		for _, i := range is {
			if i.Verdict == WillFail || i.Verdict == Data || i.Verdict == Recreate {
				top = append(top, i)
			} else {
				rest = append(rest, i)
			}
		}

		b.Reset()
		b.WriteString(head)
		details := fmt.Sprintf("\n<details><summary>%s</summary>\n\n%s", plural(len(rest), "more change"), tableHead)
		const end = "</details>\n"
		if len(top) > 0 {
			b.WriteString(tableHead)
			// Leave room for this list's own "…and N more", the summary, and the collapsed list's head and end.
			if n := len(top) - fit(&b, top, 2*len(fmt.Sprintf(more, len(is)))+len(foot)+len(details)+len(end)); n > 0 {
				fmt.Fprintf(&b, more, n)
			}
		}
		b.WriteString(foot)
		b.WriteString(details)
		if n := len(rest) - fit(&b, rest, len(fmt.Sprintf(more, len(is)))+len(end)); n > 0 {
			fmt.Fprintf(&b, more, n)
		}
		b.WriteString(end)
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// WriteJSON writes impacts as kwhy.change/v1 JSON.
func WriteJSON(w io.Writer, is []Impact) error {
	if is == nil {
		is = []Impact{}
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(struct {
		Schema  string   `json:"schema"`
		Impacts []Impact `json:"impacts"`
	}{"kwhy.change/v1", is})
}

// fit writes the rows of is to b while b stays reserve bytes under the comment limit, and says how many it wrote.
func fit(b *strings.Builder, is []Impact, reserve int) int {
	for n, i := range is {
		r := row(i)
		if b.Len()+len(r)+reserve > maxComment {
			return n
		}
		b.WriteString(r)
	}

	return len(is)
}

func row(i Impact) string {
	return fmt.Sprintf("| %s | %s | `%s` | %s | %s |\n", icons[i.Verdict], i.Verdict, i.Resource, pods(i.Pods), i.Detail)
}

// summary totals restarted pods, apply errors and data warnings, skipping zeros.
func summary(is []Impact) string {
	var restarted, failed, data, seconds int
	for _, i := range is {
		seconds = max(seconds, i.Seconds)
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
	if seconds > 0 {
		// Workloads roll in parallel, so the change takes as long as the slowest one.
		parts = append(parts, "est. "+minutes(time.Duration(seconds)*time.Second))
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
