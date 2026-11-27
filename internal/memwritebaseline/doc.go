// Package memwritebaseline is a frozen copy of the shell memory-write detector.
//
// The memory-write guard blocks a command if this detector flags it or if the
// newer passes in internal/cmd do. Changes to those passes therefore cannot
// allow a command this detector blocks. Keep it frozen: put new detection in
// internal/cmd.
package memwritebaseline

// Write returns the first Claude Code memory path that command, started in cwd,
// writes.
func Write(command, cwd string) (string, bool) {
	return bashMemoryWrite(command, cwd)
}
