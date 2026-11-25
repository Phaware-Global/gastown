//go:build !windows

package daemon

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// setEscalationSession runs cmd as the leader of a new session and makes a
// context timeout take down everything gt started: SIGTERM to its whole
// process tree at once, then SIGKILL to whatever survives after grace. logf
// reports a failed process listing, which can leave descendants running.
//
// The caller must call recordStart after cmd.Start succeeds and before
// cmd.Wait. Cancel can run after Wait has reaped gt, and by then gt's pid may
// name another process; the start time recorded here tells the two apart.
//
// A process-group kill is not enough. gt starts each bd in its own process
// group, so kill(-gtpid) misses it, and a hung bd would be orphaned on every
// retry. The tree is found by walking parent pids from gt while gt is still
// alive. A second look after SIGTERM catches anything started after that
// snapshot: once gt dies its children reparent, so they are matched by session
// ID as well as by ancestry.
func setEscalationSession(cmd *exec.Cmd, grace time.Duration, logf func(string, ...interface{})) (recordStart func()) {
	var started string // gt's start time; empty if it could not be read
	recorded := make(chan struct{})
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		<-recorded
		root := procInfo{pid: cmd.Process.Pid, started: started}
		procs := listProcessesLogged(logf)
		var tree []procInfo
		if !pidReused(procs, root) {
			tree = processTree(procs, root.pid)
			_ = syscall.Kill(-root.pid, syscall.SIGTERM) // same-group stragglers
		}
		for _, p := range tree {
			_ = syscall.Kill(p.pid, syscall.SIGTERM)
		}
		late := lateProcesses(listProcessesLogged(logf), root, tree)
		for _, p := range late {
			_ = syscall.Kill(p.pid, syscall.SIGTERM)
		}
		all := append(tree, late...)
		time.AfterFunc(grace, func() { killSurvivors(root.pid, all, logf) })
		return nil
	}
	return func() {
		defer close(recorded)
		procs, err := listProcesses()
		if err != nil {
			logf("disk_guard: cannot read gt's start time, so reuse of its pid cannot be detected: %v", err)
		}
		started = procs[cmd.Process.Pid].started
	}
}

// procInfo identifies a process. started (ps lstart) distinguishes a process
// from a later one that reuses its pid.
type procInfo struct {
	pid, ppid int
	started   string
}

// listProcesses returns every process keyed by pid.
func listProcesses() (map[int]procInfo, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,lstart=").Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	procs := map[int]procInfo{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		procs[pid] = procInfo{pid: pid, ppid: ppid, started: strings.Join(f[2:], " ")}
	}
	return procs, nil
}

// listProcessesLogged is listProcesses for the kill path, which carries on
// without a listing but must say that it did.
func listProcessesLogged(logf func(string, ...interface{})) map[int]procInfo {
	procs, err := listProcesses()
	if err != nil {
		logf("disk_guard: cannot list processes, descendants of gt may be left running: %v", err)
	}
	return procs
}

// processTree returns root and all of its descendants found in procs.
func processTree(procs map[int]procInfo, root int) []procInfo {
	children := map[int][]int{}
	for _, p := range procs {
		children[p.ppid] = append(children[p.ppid], p.pid)
	}
	var tree []procInfo
	seen := map[int]bool{}
	queue := []int{root}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		if p, ok := procs[pid]; ok {
			tree = append(tree, p)
		}
		queue = append(queue, children[pid]...)
	}
	return tree
}

// pidReused reports whether p's pid names a different process in procs. A pid
// missing from procs, or a p with no recorded start time, is not known to be
// reused.
func pidReused(procs map[int]procInfo, p procInfo) bool {
	cur, ok := procs[p.pid]
	return ok && p.started != "" && cur.started != p.started
}

// lateProcesses returns the members of procs that are not in known but belong
// to root's session or descend from a process in known.
func lateProcesses(procs map[int]procInfo, root procInfo, known []procInfo) []procInfo {
	seen := map[int]bool{}
	for _, p := range known {
		seen[p.pid] = true
	}
	var late []procInfo
	add := func(p procInfo) {
		if !seen[p.pid] {
			seen[p.pid] = true
			late = append(late, p)
		}
	}
	for _, p := range known {
		if cur, ok := procs[p.pid]; !ok || cur.started != p.started {
			continue // exited, or the pid now names another process
		}
		for _, d := range processTree(procs, p.pid) {
			add(d)
		}
	}
	if pidReused(procs, root) {
		return late // the session with this ID is no longer gt's
	}
	for pid, p := range procs {
		if sid, err := unix.Getsid(pid); err == nil && sid == root.pid {
			add(p)
		}
	}
	return late
}

// killSurvivors SIGKILLs the members of tree that are still the same
// processes (same pid and start time), plus gt's process group if gt is.
func killSurvivors(root int, tree []procInfo, logf func(string, ...interface{})) {
	procs := listProcessesLogged(logf)
	for _, p := range tree {
		cur, ok := procs[p.pid]
		if !ok || cur.started != p.started {
			continue
		}
		if p.pid == root {
			_ = syscall.Kill(-root, syscall.SIGKILL)
		}
		_ = syscall.Kill(p.pid, syscall.SIGKILL)
	}
}
