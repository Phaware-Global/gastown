//go:build !windows

package daemon

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// setEscalationSession runs cmd as the leader of a new session and makes a
// context timeout take down everything gt started: SIGTERM to its whole
// process tree at once, then SIGKILL to whatever survives after grace.
//
// A process-group kill is not enough. gt starts each bd in its own process
// group, so kill(-gtpid) misses it, and a hung bd would be orphaned on every
// retry. The tree is found by walking parent pids from gt while gt is still
// alive (pgrep -s and ps's session column are not portable to macOS, and
// once gt dies its children reparent and can no longer be traced to it).
func setEscalationSession(cmd *exec.Cmd, grace time.Duration) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		root := cmd.Process.Pid
		tree := processTree(root)
		_ = syscall.Kill(-root, syscall.SIGTERM) // same-group stragglers
		for _, p := range tree {
			_ = syscall.Kill(p.pid, syscall.SIGTERM)
		}
		time.AfterFunc(grace, func() { killSurvivors(root, tree) })
		return nil
	}
}

// procInfo identifies a process. started (ps lstart) distinguishes a process
// from a later one that reuses its pid.
type procInfo struct {
	pid, ppid int
	started   string
}

// listProcesses returns every process keyed by pid.
func listProcesses() map[int]procInfo {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ps", "-axo", "pid=,ppid=,lstart=").Output()
	if err != nil {
		return nil
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
	return procs
}

// processTree returns root and all of its descendants.
func processTree(root int) []procInfo {
	procs := listProcesses()
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

// killSurvivors SIGKILLs the members of tree that are still the same
// processes (same pid and start time), plus gt's process group if gt is.
func killSurvivors(root int, tree []procInfo) {
	procs := listProcesses()
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
