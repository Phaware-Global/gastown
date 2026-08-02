//go:build windows

package main

import "os"

// notifyWindowChange is a no-op on windows: there is no SIGWINCH, and console
// resizes are reported through a console event API rather than a signal. The
// attach still works — the remote pty simply keeps the geometry sent at attach
// time instead of tracking later resizes.
func notifyWindowChange(chan<- os.Signal) {}
