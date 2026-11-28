//go:build !windows

package reviewer

import (
	"bytes"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunPRCommandStopsTmuxServersTheCommandLeft(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	var out bytes.Buffer
	code, err := RunPRCommand([]string{"sh", "-c",
		`tmux -L left new-session -d -s s "sleep 60" && tmux -L left display-message -p '#{pid}'`}, nil, &out, &out)
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v output=%s", code, err, out.String())
	}
	pid, err := strconv.Atoi(strings.TrimSpace(out.String()))
	if err != nil {
		t.Fatalf("server pid: %v", err)
	}
	for deadline := time.Now().Add(5 * time.Second); syscall.Kill(pid, 0) == nil; time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("tmux server %d is still running", pid)
		}
	}
}

// PR code writes into the private directory, so cleanup acts only on sockets
// that are really there: never through a link to someone else's server.
func TestRemoveTmuxDirIgnoresPlantedEntries(t *testing.T) {
	short := func() string {
		dir, err := os.MkdirTemp("/tmp", "gt-t-") // socket paths are length-limited
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(dir) })
		return dir
	}
	listen := func(path string) {
		l, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = l.Close() })
	}
	must := func(err error) {
		if err != nil {
			t.Fatal(err)
		}
	}
	dir, outside, bin := short(), short(), short()
	town := filepath.Join(outside, "town")
	listen(town)
	own := filepath.Join(dir, "tmux-1", "own")
	must(os.Mkdir(filepath.Dir(own), 0o700))
	listen(own)
	must(os.Symlink(town, filepath.Join(dir, "tmux-1", "link")))
	must(os.WriteFile(filepath.Join(dir, "tmux-1", "file"), nil, 0o600))
	must(os.Symlink(outside, filepath.Join(dir, "tmux-2")))

	// A stand-in tmux records the sockets it is asked to act on.
	log := filepath.Join(bin, "log")
	must(os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\necho \"$2\" >> "+log+"\n"), 0o700)) //nolint:gosec // a test script
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))

	removeTmuxDir(dir)

	got, err := os.ReadFile(log)
	must(err)
	if string(got) != own+"\n" {
		t.Errorf("tmux was run on %q, want only %q", got, own)
	}
	if _, err := os.Lstat(town); err != nil {
		t.Errorf("the socket outside the directory is gone: %v", err)
	}
}
