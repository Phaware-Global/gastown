package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// runMemoryWriteGuard feeds one hook payload to the guard as an agent session
// and reports whether it blocked.
func runMemoryWriteGuard(t *testing.T, payload map[string]any) bool {
	t.Helper()
	t.Setenv("GT_ROLE", "somerig/polecats/worker")

	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	in := filepath.Join(t.TempDir(), "payload.json")
	if err := os.WriteFile(in, data, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(in)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	stdin := os.Stdin
	os.Stdin = f
	defer func() { os.Stdin = stdin }()

	err = runTapGuardMemoryWrite(tapGuardMemoryWriteCmd, nil)
	if err == nil {
		return false
	}
	var exit *SilentExitError
	if !errors.As(err, &exit) || exit.Code != 2 {
		t.Fatalf("guard returned %v; want nil or a silent exit 2", err)
	}
	return true
}

func TestMemoryWriteGuardBlocksWriteAndShellRedirect(t *testing.T) {
	const index = "/home/u/.claude/projects/-work-rig/memory/MEMORY.md"
	if !runMemoryWriteGuard(t, map[string]any{
		"tool_name":  "Write",
		"tool_input": map[string]any{"file_path": index, "content": "x"},
	}) {
		t.Error("Write to a memory file was allowed")
	}
	if !runMemoryWriteGuard(t, map[string]any{
		"tool_name":  "Bash",
		"tool_input": map[string]any{"command": "echo '- a note' >> " + index},
	}) {
		t.Error("shell append to a memory file was allowed")
	}
}

func TestMemoryWriteGuardBashCommands(t *testing.T) {
	const dir = "/home/u/.claude/projects/-work-rig/memory"
	tests := []struct {
		name    string
		command string
		cwd     string
		block   bool
	}{
		{name: "overwrite redirect", command: "printf x > " + dir + "/a.md", block: true},
		{name: "append without spaces", command: "echo x>>" + dir + "/a.md", block: true},
		{name: "quoted target", command: `echo x >> "` + dir + `/a.md"`, block: true},
		{name: "home variable", command: `echo x >> "$HOME/.claude/projects/p/memory/a.md"`, block: true},
		{name: "tilde", command: "echo x >> ~/.claude/projects/p/memory/a.md", block: true},
		{name: "clobber redirect", command: "echo x >| " + dir + "/a.md", block: true},
		{name: "zsh forced append", command: "echo x >>! " + dir + "/a.md", block: true},
		{name: "both streams", command: "make &> " + dir + "/a.md", block: true},
		{name: "stderr redirect", command: "make 2>" + dir + "/a.md", block: true},
		{name: "heredoc into file", command: "cat >> " + dir + "/a.md <<'EOF'\n- note\nEOF", block: true},
		{name: "redirect after heredoc marker", command: "cat <<EOF >> " + dir + "/a.md\n- note\nEOF", block: true},
		{name: "command after heredoc", command: "cat <<EOF\nbody\nEOF\necho x >> " + dir + "/a.md", block: true},
		{name: "brace group", command: "{ echo a; echo b; } >> " + dir + "/a.md", block: true},
		{name: "tee", command: "echo x | tee " + dir + "/a.md", block: true},
		{name: "tee append", command: "echo x | tee -a /tmp/log " + dir + "/a.md >/dev/null", block: true},
		{name: "sed in place", command: "sed -i 's/a/b/' " + dir + "/a.md", block: true},
		{name: "BSD sed in place", command: "sed -i '' -e '1d' " + dir + "/a.md", block: true},
		{name: "sed in place with suffix", command: "sed -E -i.bak 's/a/b/' " + dir + "/a.md", block: true},
		{name: "sed long expression flag", command: "sed --in-place --expression=1d " + dir + "/a.md", block: true},
		{name: "perl in place",command: "perl -pi -e 's/a/b/' " + dir + "/a.md", block: true},
		{name: "cp into file", command: "cp /tmp/n.md " + dir + "/n.md", block: true},
		{name: "cp into directory", command: "cp /tmp/n.md " + dir, block: true},
		{name: "cp target-directory flag", command: "cp -t " + dir + " /tmp/n.md", block: true},
		{name: "mv into directory", command: "mv /tmp/n.md " + dir + "/", block: true},
		{name: "install", command: "install -m 600 /tmp/n.md " + dir + "/n.md", block: true},
		{name: "ln", command: "ln -sf /tmp/n.md " + dir + "/n.md", block: true},
		{name: "rsync", command: "rsync -a /tmp/notes/ " + dir + "/", block: true},
		{name: "touch", command: "touch " + dir + "/a.md", block: true},
		{name: "truncate", command: "truncate -s 0 " + dir + "/a.md", block: true},
		{name: "dd", command: "dd if=/tmp/n of=" + dir + "/a.md", block: true},
		{name: "after cd", command: "cd " + dir + " && echo x >> MEMORY.md", block: true},
		{name: "session already in memory dir", command: "echo x >> MEMORY.md", cwd: dir, block: true},
		{name: "variable", command: "M=" + dir + "; echo x >> $M/MEMORY.md", block: true},
		{name: "braced variable", command: "export M=" + dir + "\necho x >> \"${M}/MEMORY.md\"", block: true},
		{name: "dot segments", command: "echo x >> " + dir + "/../memory/a.md", block: true},
		{name: "wrapper command", command: "env LC_ALL=C tee " + dir + "/a.md", block: true},
		{name: "xargs", command: "ls /tmp/n | xargs -I{} cp {} " + dir + "/", block: true},
		{name: "shell -c script", command: `bash -c 'echo x >> ` + dir + `/a.md'`, block: true},
		{name: "subshell", command: "(echo x >> " + dir + "/a.md)", block: true},
		{name: "command substitution", command: "v=$(echo x | tee " + dir + "/a.md)", block: true},

		{name: "read", command: "cat " + dir + "/MEMORY.md"},
		{name: "read redirected elsewhere", command: "grep x " + dir + "/MEMORY.md > /tmp/out"},
		{name: "input redirect", command: "wc -l < " + dir + "/MEMORY.md"},
		{name: "copy out", command: "cp " + dir + "/MEMORY.md /tmp/backup.md"},
		{name: "sed without in place", command: "sed -n '1,5p' " + dir + "/MEMORY.md"},
		{name: "sed script mentions the path", command: "sed -i 's|" + dir + "/|x|' /tmp/doc.md"},
		{name: "quoted mention", command: `gt nudge mayor/ "do not run echo x >> ` + dir + `/a.md"`},
		{name: "heredoc mention", command: "cat > /tmp/doc.md <<'EOF'\necho x >> " + dir + "/a.md\nEOF"},
		{name: "comment", command: "ls # echo x >> " + dir + "/a.md"},
		{name: "remove is allowed", command: "rm " + dir + "/a.md"},
		{name: "fd duplication", command: "ls " + dir + " 2>&1"},
		{name: "other memory directory", command: "echo x >> /work/repo/memory/notes.md"},
		{name: "other claude directory", command: "echo x >> /home/u/.claude/projects/p/settings.json"},
		{name: "cd away again", command: "cd " + dir + " && ls; cd /tmp && echo x >> MEMORY.md"},
		{name: "subshell cd does not persist", command: "(cd " + dir + " && ls); echo x >> MEMORY.md", cwd: "/work/repo"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cwd := tc.cwd
			if cwd == "" {
				cwd = "/work/repo"
			}
			got := runMemoryWriteGuard(t, map[string]any{
				"tool_name":  "Bash",
				"cwd":        cwd,
				"tool_input": map[string]any{"command": tc.command},
			})
			if got != tc.block {
				t.Errorf("command %q: blocked = %v, want %v", tc.command, got, tc.block)
			}
		})
	}
}
