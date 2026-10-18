package transform

// NeutralizeExternalText is the exported form of neutralizeDelimiters for
// other producers of fenced external text (the Mayor's Assistant sweep
// report). It normalizes line separators and control characters and prefixes
// every line that reads as a trust delimiter, trusted heading or
// Telegraph-Boundary header with "[neutralized delimiter]".
func NeutralizeExternalText(text string) string {
	return neutralizeDelimiters(text)
}
