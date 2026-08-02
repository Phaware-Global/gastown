//go:build !windows

package main

import (
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestCanonicalSignalName_UnknownSignal pins the fallback for a signal the
// launcher does not name explicitly: it still crosses in an upper-cased form
// the worker can reject cleanly rather than misinterpret.
//
// Unix-only because SIGUSR1 does not exist on windows, where vet fails to
// compile the reference at all.
func TestCanonicalSignalName_UnknownSignal(t *testing.T) {
	assert.Equal(t, "USER DEFINED SIGNAL 1", canonicalSignalName(syscall.SIGUSR1))
}
