//go:build windows

package daemon

import (
	"context"
	"errors"
)

// Without a way to see which processes hold a directory, Windows sweeps nothing.
var errGoTmpUnsupported = errors.New("go temp sweep is not supported on Windows")

func checkPrivateDir(string) error { return errGoTmpUnsupported }

func listCommand(context.Context, string, ...string) (string, error) {
	return "", errGoTmpUnsupported
}

func goTmpHolders(context.Context) (string, error) { return "", errGoTmpUnsupported }
