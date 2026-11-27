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

const memoryCaseDir = "/home/u/.claude/projects/-work-rig/memory"

var memoryBashCases = []struct {
	name    string
	command string
	cwd     string
	block   bool
}{
	{name: "overwrite redirect", command: "printf x > " + memoryCaseDir + "/a.md", block: true},
	{name: "append without spaces", command: "echo x>>" + memoryCaseDir + "/a.md", block: true},
	{name: "quoted target", command: `echo x >> "` + memoryCaseDir + `/a.md"`, block: true},
	{name: "home variable", command: `echo x >> "$HOME/.claude/projects/p/memory/a.md"`, block: true},
	{name: "tilde", command: "echo x >> ~/.claude/projects/p/memory/a.md", block: true},
	{name: "clobber redirect", command: "echo x >| " + memoryCaseDir + "/a.md", block: true},
	{name: "zsh forced append", command: "echo x >>! " + memoryCaseDir + "/a.md", block: true},
	{name: "both streams", command: "make &> " + memoryCaseDir + "/a.md", block: true},
	{name: "stderr redirect", command: "make 2>" + memoryCaseDir + "/a.md", block: true},
	{name: "heredoc into file", command: "cat >> " + memoryCaseDir + "/a.md <<'EOF'\n- note\nEOF", block: true},
	{name: "redirect after heredoc marker", command: "cat <<EOF >> " + memoryCaseDir + "/a.md\n- note\nEOF", block: true},
	{name: "command after heredoc", command: "cat <<EOF\nbody\nEOF\necho x >> " + memoryCaseDir + "/a.md", block: true},
	{name: "brace group", command: "{ echo a; echo b; } >> " + memoryCaseDir + "/a.md", block: true},
	{name: "tee", command: "echo x | tee " + memoryCaseDir + "/a.md", block: true},
	{name: "tee append", command: "echo x | tee -a /tmp/log " + memoryCaseDir + "/a.md >/dev/null", block: true},
	{name: "sed in place", command: "sed -i 's/a/b/' " + memoryCaseDir + "/a.md", block: true},
	{name: "BSD sed in place", command: "sed -i '' -e '1d' " + memoryCaseDir + "/a.md", block: true},
	{name: "sed in place with suffix", command: "sed -E -i.bak 's/a/b/' " + memoryCaseDir + "/a.md", block: true},
	{name: "sed long expression flag", command: "sed --in-place --expression=1d " + memoryCaseDir + "/a.md", block: true},
	{name: "perl in place", command: "perl -pi -e 's/a/b/' " + memoryCaseDir + "/a.md", block: true},
	{name: "cp into file", command: "cp /tmp/n.md " + memoryCaseDir + "/n.md", block: true},
	{name: "cp into directory", command: "cp /tmp/n.md " + memoryCaseDir, block: true},
	{name: "cp target-directory flag", command: "cp -t " + memoryCaseDir + " /tmp/n.md", block: true},
	{name: "mv into directory", command: "mv /tmp/n.md " + memoryCaseDir + "/", block: true},
	{name: "install", command: "install -m 600 /tmp/n.md " + memoryCaseDir + "/n.md", block: true},
	{name: "ln", command: "ln -sf /tmp/n.md " + memoryCaseDir + "/n.md", block: true},
	{name: "rsync", command: "rsync -a /tmp/notes/ " + memoryCaseDir + "/", block: true},
	{name: "touch", command: "touch " + memoryCaseDir + "/a.md", block: true},
	{name: "truncate", command: "truncate -s 0 " + memoryCaseDir + "/a.md", block: true},
	{name: "dd", command: "dd if=/tmp/n of=" + memoryCaseDir + "/a.md", block: true},
	{name: "after cd", command: "cd " + memoryCaseDir + " && echo x >> MEMORY.md", block: true},
	{name: "session already in memory dir", command: "echo x >> MEMORY.md", cwd: memoryCaseDir, block: true},
	{name: "variable", command: "M=" + memoryCaseDir + "; echo x >> $M/MEMORY.md", block: true},
	{name: "braced variable", command: "export M=" + memoryCaseDir + "\necho x >> \"${M}/MEMORY.md\"", block: true},
	{name: "dot segments", command: "echo x >> " + memoryCaseDir + "/../memory/a.md", block: true},
	{name: "wrapper command", command: "env LC_ALL=C tee " + memoryCaseDir + "/a.md", block: true},
	{name: "xargs", command: "ls /tmp/n | xargs -I{} cp {} " + memoryCaseDir + "/", block: true},
	{name: "shell -c script", command: `bash -c 'echo x >> ` + memoryCaseDir + `/a.md'`, block: true},
	{name: "subshell", command: "(echo x >> " + memoryCaseDir + "/a.md)", block: true},
	{name: "command substitution", command: "v=$(echo x | tee " + memoryCaseDir + "/a.md)", block: true},

	{name: "quoted substitution", command: `v="$(echo x | tee ` + memoryCaseDir + `/a.md)"`, block: true},
	{name: "quoted substitution redirect", command: `echo "$(echo x >> ` + memoryCaseDir + `/a.md)"`, block: true},
	{name: "nested quoted substitution", command: `echo "a $(echo "$(touch ` + memoryCaseDir + `/a.md)")"`, block: true},
	{name: "quoted backticks", command: "echo \"`echo x >> " + memoryCaseDir + "/a.md`\"", block: true},
	{name: "backticks", command: "v=`echo x >> " + memoryCaseDir + "/a.md`", block: true},
	{name: "escaped nested backticks", command: "v=`echo \\`touch " + memoryCaseDir + "/a.md\\``", block: true},
	{name: "substitution in unquoted heredoc", command: "cat <<EOF\n$(touch " + memoryCaseDir + "/a.md)\nEOF", block: true},
	{name: "substitution in arithmetic", command: `echo "$(( $(touch ` + memoryCaseDir + `/a.md) + 1 ))"`, block: true},
	{name: "substitution after cd", command: `cd ` + memoryCaseDir + ` && echo "$(touch a.md)"`, block: true},
	{name: "arithmetic shift expansion", command: "echo $((1<<2))\necho x >> " + memoryCaseDir + "/a.md", block: true},
	{name: "arithmetic shift command", command: "(( n = 1 << shift ))\necho x >> " + memoryCaseDir + "/a.md\nshift", block: true},
	{name: "let shift", command: "let n=1<<2\necho x >> " + memoryCaseDir + "/a.md", block: true},
	{name: "cp directory named memory", command: "cp -r /tmp/memory /home/u/.claude/projects/p/", block: true},
	{name: "mv directory named memory", command: "mv /tmp/x /tmp/memory /home/u/.claude/projects/p", block: true},
	{name: "cp target-directory flag directory named memory", command: "cp -rt /home/u/.claude/projects/p /tmp/memory", block: true},
	{name: "cp variable naming a memory directory", command: "S=/tmp/memory; cp -r $S /home/u/.claude/projects/p/", block: true},
	{name: "rsync directory named memory", command: "rsync -a /tmp/memory /home/u/.claude/projects/p/", block: true},
	{name: "heredoc fed to bash", command: "bash <<'EOF'\necho x >> " + memoryCaseDir + "/a.md\nEOF", block: true},
	{name: "heredoc fed to sh -s", command: "sh -s -- arg <<EOF\necho x >> " + memoryCaseDir + "/a.md\nEOF", block: true},
	{name: "heredoc fed to bash with option", command: "bash -o pipefail <<EOF\necho x >> " + memoryCaseDir + "/a.md\nEOF", block: true},
	{name: "here-string fed to bash", command: `bash <<< 'echo x >> ` + memoryCaseDir + `/a.md'`, block: true},
	{name: "escaped backticks in heredoc fed to bash", command: "bash <<EOF\nv=\\`touch " + memoryCaseDir + "/a.md\\`\nEOF", block: true},
	{name: "heredoc fed to bash /dev/stdin", command: "bash /dev/stdin <<EOF\necho x >> " + memoryCaseDir + "/a.md\nEOF", block: true},
	{name: "heredoc fed to bash /dev/fd/0", command: "bash /dev/fd/0 <<EOF\necho x >> " + memoryCaseDir + "/a.md\nEOF", block: true},
	{name: "paren line inside a heredoc zsh reads on", command: "x=$(cat <<EOF\nEOF)'\nEOF\n)\necho x > " + memoryCaseDir + "/a.md\n# '", block: true},
	{name: "command after the paren line before a later delimiter", command: "v=\"$(cat <<EOF\nEOF)\"\necho x > " + memoryCaseDir + "/a.md\ncat <<EOF\nEOF", block: true},
	{name: "parameter expansion holding a paren", command: `v="$(echo ${x%)}; touch ` + memoryCaseDir + `/a.md)"`, block: true},
	{name: "substitution in a parameter expansion", command: `v="$(echo ${x:-$(touch ` + memoryCaseDir + `/a.md)})"`, block: true},
	{name: "cp source that looks like -t after --", command: "cp -- -at /tmp/x " + memoryCaseDir + "/", block: true},
	{name: "cp directory named memory without slash", command: "cp -r /tmp/memory /home/u/.claude/projects/p", block: true},
	{name: "long s in memory path", command: "echo x > /home/u/.claude/project\u017f/p/memory/a", block: true},
	{name: "upper-case memory directory", command: "echo x >> /home/u/.claude/projects/p/MEMORY/a.md", block: true},
	{name: "bare brace in a parameter expansion", command: "x=$(echo ${x:-{}; echo hi > " + memoryCaseDir + "/a.md; echo })", block: true},
	{name: "brace after the process id", command: "x=$(echo $${; touch " + memoryCaseDir + "/a.md; echo })", block: true},
	{name: "escaped dollar before a brace", command: "x=$(echo ${x:-\\${}; touch " + memoryCaseDir + "/a.md; echo })", block: true},
	{name: "bare brace zsh nests on", command: "x=$(echo ${x:-{ # }}\ntouch " + memoryCaseDir + "/a.md)", block: true},
	{name: "quote after the paren line pairing across the delimiter", command: "v=\"$(cat <<EOF\nEOF)\"'\nEOF\n'; echo x > " + memoryCaseDir + "/a.md\n#')\"", block: true},
	{name: "paren line closing an unquoted substitution", command: "x=$(cat <<EOF\nEOF)\"\n\"; echo x > " + memoryCaseDir + "/a.md\nEOF\n)", block: true},
	{name: "comment brace in a substitution in a parameter expansion", command: "v=\"$(echo ${x:-$(echo # }\n)}; touch " + memoryCaseDir + "/a.md)\"", block: true},
	{name: "heredoc brace in a substitution in a parameter expansion", command: "v=\"$(echo ${x:-$(cat <<E\n}\nE\n)}; touch " + memoryCaseDir + "/a.md)\"", block: true},
	{name: "comment brace in backticks in a parameter expansion", command: "v=\"$(echo ${x:-`echo # }`}; touch " + memoryCaseDir + "/a.md)\"", block: true},
	{name: "heredoc fed to bash /dev/./stdin", command: "bash /dev/./stdin <<EOF\necho x >> " + memoryCaseDir + "/a.md\nEOF", block: true},
	{name: "heredoc fed to zsh /dev//fd/0", command: "zsh /dev//fd/0 <<EOF\necho x >> " + memoryCaseDir + "/a.md\nEOF", block: true},
	{name: "heredoc fed to bash /proc/self/fd/0", command: "bash /proc/self/fd/0 <<EOF\necho x >> " + memoryCaseDir + "/a.md\nEOF", block: true},
	{name: "mv into an existing directory", command: "mv memory p", cwd: "/home/u/.claude/projects", block: true},
	{name: "mv into an existing directory with a slash", command: "mv memory p/", cwd: "/home/u/.claude/projects", block: true},
	{name: "cp into an existing directory", command: "cp -r memory p", cwd: "/home/u/.claude/projects", block: true},
	{name: "rsync into an existing directory", command: "rsync -a memory p", cwd: "/home/u/.claude/projects", block: true},
	{name: "ln into an existing directory", command: "ln -s /tmp/memory p", cwd: "/home/u/.claude/projects", block: true},
	{name: "install into an existing directory", command: "install /tmp/memory p", cwd: "/home/u/.claude/projects", block: true},

	{name: "cd inside backticks stays blocked", command: "v=`cd " + memoryCaseDir + " && pwd`; echo x > out.txt", block: true},
	{name: "cd inside substitution does not persist", command: `v="$(cd ` + memoryCaseDir + ` && pwd)"; echo x > out.txt`},
	{name: "single-quoted substitution", command: `echo '$(touch ` + memoryCaseDir + `/a.md)'`},
	{name: "escaped substitution", command: `echo "\$(touch ` + memoryCaseDir + `/a.md)"`},
	{name: "substitution in quoted heredoc", command: "cat <<'EOF'\n$(touch " + memoryCaseDir + "/a.md)\nEOF"},
	{name: "heredoc inside quoted substitution", command: "git commit -m \"$(cat <<'EOF'\nit's: echo x > " + memoryCaseDir + "/a.md )\nEOF\n)\""},
	{name: "heredoc closed on the paren line", command: "v=\"$(cat <<'EOF'\necho x > " + memoryCaseDir + "/a.md\nEOF)\""},
	{name: "substitution naming a memory file as input", command: "echo x > /tmp/$(basename " + memoryCaseDir + "/a.md)"},
	{name: "arithmetic comparison stays blocked", command: "(( a > b ))", cwd: memoryCaseDir, block: true},
	{name: "heredoc to a script file", command: "bash run.sh <<EOF\necho x >> " + memoryCaseDir + "/a.md\nEOF"},
	{name: "cp other file into project directory", command: "cp /tmp/n.md /home/u/.claude/projects/p/"},
	{name: "cp directory named memory elsewhere", command: "cp -r " + memoryCaseDir + " /tmp/"},
	{name: "rsync contents of a directory named memory", command: "rsync -a /tmp/memory/ /home/u/.claude/projects/p/"},
	{name: "perl module flag stays blocked", command: "perl -Mstrict -ne 'print' " + memoryCaseDir + "/MEMORY.md", block: true},
	{name: "sed script file flag is not in place", command: "sed -nfprint.sed " + memoryCaseDir + "/MEMORY.md"},
	{name: "touch reference file stays blocked", command: "touch -r " + memoryCaseDir + "/MEMORY.md /tmp/stamp", block: true},
	{name: "truncate reference file stays blocked", command: "truncate -r " + memoryCaseDir + "/MEMORY.md /tmp/stamp", block: true},

	{name: "rename memory directory to a sibling", command: "mv /home/u/.claude/projects/p/memory /home/u/.claude/projects/p/memory.bak"},
	{name: "rename memory directory after cd", command: "cd /home/u/.claude/projects/p && mv memory memory.bak"},
	{name: "read", command: "cat " + memoryCaseDir + "/MEMORY.md"},
	{name: "read redirected elsewhere", command: "grep x " + memoryCaseDir + "/MEMORY.md > /tmp/out"},
	{name: "input redirect", command: "wc -l < " + memoryCaseDir + "/MEMORY.md"},
	{name: "copy out", command: "cp " + memoryCaseDir + "/MEMORY.md /tmp/backup.md"},
	{name: "sed without in place", command: "sed -n '1,5p' " + memoryCaseDir + "/MEMORY.md"},
	{name: "sed script mentions the path", command: "sed -i 's|" + memoryCaseDir + "/|x|' /tmp/doc.md"},
	{name: "quoted mention", command: `gt nudge mayor/ "do not run echo x >> ` + memoryCaseDir + `/a.md"`},
	{name: "heredoc mention", command: "cat > /tmp/doc.md <<'EOF'\necho x >> " + memoryCaseDir + "/a.md\nEOF"},
	{name: "comment", command: "ls # echo x >> " + memoryCaseDir + "/a.md"},
	{name: "remove is allowed", command: "rm " + memoryCaseDir + "/a.md"},
	{name: "fd duplication", command: "ls " + memoryCaseDir + " 2>&1"},
	{name: "other memory directory", command: "echo x >> /work/repo/memory/notes.md"},
	{name: "other claude directory", command: "echo x >> /home/u/.claude/projects/p/settings.json"},
	{name: "cd away again", command: "cd " + memoryCaseDir + " && ls; cd /tmp && echo x >> MEMORY.md"},
	{name: "subshell cd does not persist", command: "(cd " + memoryCaseDir + " && ls); echo x >> MEMORY.md", cwd: "/work/repo"},
}

func TestMemoryWriteGuardBashCommands(t *testing.T) {
	for _, tc := range memoryBashCases {
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
