package sweep

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Markdown renders a report for the assistant to read. External values are
// fenced; Fields are printed as-is (they passed their shape allowlist).
func Markdown(r *Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Sweep report\n\nRun-ID: %s\nGenerated: %s\n", r.RunID, r.GeneratedAt.UTC().Format(time.RFC3339))
	if r.Pending {
		fmt.Fprintf(&b, "After mailing the Mayor, acknowledge with: gt assistant sweep --ack %s\n", r.RunID)
	} else {
		b.WriteString("(not recorded: --json/--dry-run runs never write state)\n")
	}
	for _, p := range r.Ports {
		fmt.Fprintf(&b, "\n## Port: %s\n", p.Name)
		if p.Error != "" {
			b.WriteString("NOT CHECKED: the port failed; its error is external text (below), not a result\n")
			b.WriteString(Fence(p.Name, "error", "", p.Error))
			b.WriteByte('\n')
			continue
		}
		if p.Partial {
			b.WriteString("PARTIAL: some findings may be missing; standing items are unchanged, not cleared\n")
		}
		if len(p.New) == 0 && len(p.Standing) == 0 {
			b.WriteString("all clear\n")
		}
		for _, f := range p.New {
			fmt.Fprintf(&b, "\n### [%s] %s — %s\n", f.Severity, f.Kind, f.Key)
			if f.URL != "" {
				fmt.Fprintf(&b, "URL: %s\n", f.URL)
			}
			if !f.Since.IsZero() {
				fmt.Fprintf(&b, "Since: %s (%s)\n", f.Since.UTC().Format(time.RFC3339), humanAge(r.GeneratedAt.Sub(f.Since)))
			}
			for _, k := range sortedKeys(f.Fields) {
				fmt.Fprintf(&b, "%s: %s\n", k, f.Fields[k])
			}
			if f.Suggest != "" {
				fmt.Fprintf(&b, "Suggest: %s\n", f.Suggest)
			}
			for _, k := range sortedKeys(f.External) {
				b.WriteString(Fence(f.Port, f.Key, k, f.External[k]))
				b.WriteByte('\n')
			}
		}
		for _, s := range p.Standing {
			fmt.Fprintf(&b, "\nstanding: %s ×%d", s.Kind, s.Count)
			if !s.Oldest.IsZero() {
				fmt.Fprintf(&b, " (oldest %s)", humanAge(r.GeneratedAt.Sub(s.Oldest)))
			}
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// JSON renders a report for machines. External values are neutralized and
// "> "-prefixed per line; the `external` object itself is the fence.
func JSON(r *Report) ([]byte, error) {
	out := *r
	out.Ports = make([]PortResult, len(r.Ports))
	for i, p := range r.Ports {
		q := p
		if p.Error != "" {
			q.Error = FenceJSON(p.Error)
		}
		q.New = make([]Finding, len(p.New))
		for j, f := range p.New {
			g := f
			g.Fingerprint = FenceJSON(f.Fingerprint) // port-computed from provider data
			if len(f.External) > 0 {
				g.External = make(map[string]string, len(f.External))
				for k, v := range f.External {
					g.External[k] = FenceJSON(v)
				}
			}
			q.New[j] = g
		}
		out.Ports[i] = q
	}
	return json.MarshalIndent(out, "", "  ")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func humanAge(d time.Duration) string {
	switch {
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}
