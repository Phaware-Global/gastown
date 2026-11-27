// gt-worker-attach is the standalone form of the pane launcher for the socket
// execution provider; see internal/workerattach. The pane itself runs
// `gt worker-attach`.
package main

import (
	"os"

	"github.com/steveyegge/gastown/internal/workerattach"
)

func main() {
	os.Exit(workerattach.Main(os.Args[1:]))
}
