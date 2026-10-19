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

// serverMayNotBeRunning is the diagnosis half of bd's "may not be running. Try:"
// lead-in. It is kept because callers match on it to tell a down server from a
// transient failure (polecat isDoltConfigError): bd's dial errors other than
// "connection refused" (i/o timeout, no route to host) carry no other marker.
const serverMayNotBeRunning = "The Dolt server may not be running."

// RedirectDoltStartHint rewrites every line of bd output that advises running
// `bd dolt start` into DoltStartGuidance (once per message) and replaces the
// lead-in that introduced the advice with its diagnosis, once. gt pins
// BEADS_DOLT_AUTO_START=0, so the lead-in it sees from bd is "Start the server
// manually:", not "may not be running. Try:". Output without the advice is
// returned as is.
func RedirectDoltStartHint(s string) string {
	if !strings.Contains(s, "bd dolt start") {
		return s
	}
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	redirected, diagnosed := false, false
	for _, line := range lines {
		switch trimmed := strings.TrimSpace(line); {
		case trimmed == "Start the server manually:" || trimmed == serverMayNotBeRunning+" Try:":
			if !diagnosed {
				out = append(out, serverMayNotBeRunning)
				diagnosed = true
			}
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
