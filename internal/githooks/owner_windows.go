//go:build windows

package githooks

import "io/fs"

// Windows access control is not expressed in the file mode.
func checkOwnership(fs.FileInfo) error { return nil }
