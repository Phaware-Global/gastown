package beads

import "strings"

// DoltStartGuidance replaces bd's "run bd dolt start" advice in anything gt
// relays to an agent.
//
// bd's connection errors tell the reader to start a server with `bd dolt start`.
// That command is an explicit start: it ignores dolt.auto-start: false and
// BEADS_DOLT_AUTO_START=0, and when run from an agent's own directory (the
// deacon's cwd is <town>/deacon) it binds the shared port from that directory's
// empty .beads/dolt store. Every agent then reads empty data until the canonical
// server reclaims the port. Agents follow the advice because gt hands it to them
// verbatim in `gt patrol new` and every wrapped bd error.
const DoltStartGuidance = "Dolt is managed by Gas Town, not by bd. Do NOT start a server with bd: " +
	"it ignores dolt.auto-start and binds the shared port to an empty store. " +
	"Run 'gt dolt status'; if the server is down, escalate: gt escalate -s HIGH \"Dolt: server unreachable\"."

// RedirectDoltStartHint rewrites every line of bd output that advises running
// `bd dolt start` into DoltStartGuidance (once per message) and drops the lead-in
// lines that introduced the advice. Output without the advice is returned as is.
func RedirectDoltStartHint(s string) string {
	if !strings.Contains(s, "bd dolt start") {
		return s
	}
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	redirected := false
	for _, line := range lines {
		switch trimmed := strings.TrimSpace(line); {
		case trimmed == "Start the server manually:",
			trimmed == "The Dolt server may not be running. Try:":
			continue
		case strings.Contains(line, "bd dolt start"):
			if !redirected {
				out = append(out, DoltStartGuidance)
				redirected = true
			}
		default:
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}
