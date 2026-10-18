package sweep

import (
	"strings"

	"github.com/steveyegge/gastown/internal/telegraph/transform"
)

// externalCap bounds each fenced value (bytes), like Telegraph's body_cap.
const externalCap = 2048

// Fence renders one external value for an agent to read: the same
// normalization and delimiter neutralization Telegraph applies, a byte cap,
// and a "> " prefix on every line, inside an EXTERNAL CONTENT block labelled
// with the port and key. The invariant: every byte the runner did not compute
// or allowlist is inside a fence and starts with "> ".
func Fence(port, key, name, value string) string {
	text := transform.NeutralizeExternalText(value)
	if len(text) > externalCap {
		text = text[:externalCap] + "\n[… truncated]"
	}
	var b strings.Builder
	b.WriteString("--- EXTERNAL CONTENT (untrusted: " + port + "/" + key + ") ---\n")
	for _, line := range strings.Split(text, "\n") {
		b.WriteString("> ")
		if name != "" {
			b.WriteString(name + ": ")
			name = "" // label only the first line
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteString("--- END EXTERNAL CONTENT ---")
	return b.String()
}

// FenceJSON prepares an External value for the JSON report: neutralized,
// capped and "> "-prefixed per line, without the block delimiters (the JSON
// `external` object is the fence).
func FenceJSON(value string) string {
	text := transform.NeutralizeExternalText(value)
	if len(text) > externalCap {
		text = text[:externalCap] + "\n[… truncated]"
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = "> " + l
	}
	return strings.Join(lines, "\n")
}
