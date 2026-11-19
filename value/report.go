package value

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
)

// WriteHuman writes answers for a terminal. Several answers each get a header naming the field.
func WriteHuman(w io.Writer, as []Answer) error {
	var b strings.Builder
	for i, a := range as {
		if len(as) > 1 {
			if i > 0 {
				b.WriteByte('\n')
			}
			fmt.Fprintf(&b, "%s %s\n", a.Resource, a.Field.Path)
		}

		val := fmt.Sprint(a.Value)
		pad := strings.Repeat(" ", len(val)+2)
		var rows []string

		switch {
		case a.Kind == Direct:
			l := a.Leaves[0]
			fmt.Fprintf(&b, "%s  %s  %s\n", val, source(l.Chain[0]), PathString(l.Path))
			for j, s := range l.Chain[1:] {
				// shortcut: the last entry is labelled default even when it is a lower user file; Source doesn't know which files are the chart's.
				label := "├─ overrides"
				if j == len(l.Chain)-2 {
					label = "└─ default"
				}
				rows = append(rows, fmt.Sprintf("%s\t%s\t= %v", label, source(s), s.Value))
			}

		case len(a.Leaves) > 0:
			var from []string
			for _, l := range a.Leaves {
				from = append(from, fmt.Sprintf("%s (%s)", PathString(l.Path), source(l.Chain[0])))
			}
			fmt.Fprintf(&b, "%s  %s  ← %s\n", val, a.Kind, strings.Join(from, ", "))

		default:
			fmt.Fprintf(&b, "%s  %s\n", val, a.Kind)
		}

		rows = append(rows, "template\t"+a.Template)
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		for _, r := range rows {
			fmt.Fprintf(tw, "%s%s\n", pad, r)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// WriteMarkdown writes answers as a table, for pull request comments.
func WriteMarkdown(w io.Writer, as []Answer) error {
	var b strings.Builder
	b.WriteString("| Field | Value | Kind | Source | Template |\n|---|---|---|---|---|\n")
	for _, a := range as {
		var from []string
		for _, l := range a.Leaves {
			var s strings.Builder
			fmt.Fprintf(&s, "`%s` %s", PathString(l.Path), source(l.Chain[0]))
			for _, o := range l.Chain[1:] {
				s.WriteString(", overrides ")
				s.WriteString(source(o))
			}
			from = append(from, s.String())
		}

		fmt.Fprintf(&b, "| `%s %s` | `%v` | %s | %s | %s |\n", a.Resource, a.Field.Path, a.Value, a.Kind,
			strings.Join(from, "; "), a.Template)
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// WriteJSON writes answers as the kwhy.value/v1 JSON document.
func WriteJSON(w io.Writer, as []Answer) error {
	if as == nil {
		as = []Answer{}
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(struct {
		Schema  string   `json:"schema"`
		Answers []Answer `json:"answers"`
	}{"kwhy.value/v1", as})
}

func source(s Source) string {
	if strings.HasPrefix(s.File, "--set ") {
		return s.File
	}

	return fmt.Sprintf("%s:%d", s.File, s.Line)
}
