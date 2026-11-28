// Package doltport holds the override that outranks every configured Dolt
// server port. Each resolver of that port has to consult it.
package doltport

import (
	"os"
	"strconv"
)

// ForceEnv names a port that wins over config.yaml, daemon.json, a beads
// directory's metadata and the other port variables. It is how a process tree
// is kept off the town's Dolt server even when it can still find the town.
const ForceEnv = "GT_DOLT_FORCE_PORT"

// Forced returns the port ForceEnv names, if it names one.
func Forced() (int, bool) {
	port, err := strconv.Atoi(os.Getenv(ForceEnv))
	if err != nil || port <= 0 {
		return 0, false
	}
	return port, true
}
