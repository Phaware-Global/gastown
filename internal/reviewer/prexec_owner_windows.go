package reviewer

import "os"

// There are no tmux servers to stop on Windows.
func ownedByCurrentUser(os.FileInfo) bool { return false }
