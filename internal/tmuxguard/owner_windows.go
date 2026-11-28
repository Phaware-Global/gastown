package tmuxguard

import "io/fs"

// Windows reports no Unix owner or mode, and its temp directory is per user.
func checkOwnerOnly(string, fs.FileInfo) error { return nil }
