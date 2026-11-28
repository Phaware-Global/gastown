package cmd

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/memwritebaseline"
)

type memoryCommand struct {
	name    string
	command string
	cwd     string // "/work/repo" when empty
}

const (
	corpusDir  = "/home/u/.claude/projects/-work-rig/memory"
	corpusFile = corpusDir + "/a.md"
	projects   = "/home/u/.claude/projects"
)

func heredocTo(shell string) string {
	return shell + " <<EOF\necho x >> " + corpusFile + "\nEOF"
}

// memoryWriteCorpus holds commands that write, or may write, a memory file in a
// real shell. Every one must be blocked.
var memoryWriteCorpus = []memoryCommand{
	// Heredocs that bash and zsh close on different lines.
	{name: "paren line inside a heredoc zsh reads on", command: "x=$(cat <<EOF\nEOF)'\nEOF\n)\necho x > " + corpusFile + "\n# '"},
	{name: "quote after the paren line pairing across the delimiter", command: "v=\"$(cat <<EOF\nEOF)\"'\nEOF\n'; echo x > " + corpusFile + "\n#')\""},
	{name: "paren line closing an unquoted substitution", command: "x=$(cat <<EOF\nEOF)\"\n\"; echo x > " + corpusFile + "\nEOF\n)"},
	{name: "command after the paren line before a later delimiter", command: "v=\"$(cat <<EOF\nEOF)\"\necho x > " + corpusFile + "\ncat <<EOF\nEOF"},

	// Parameter expansions.
	{name: "paren inside a parameter expansion", command: `v="$(echo ${x%)}; touch ` + corpusFile + `)"`},
	{name: "bare brace in a parameter expansion", command: "x=$(echo ${x:-{}; echo hi > " + corpusFile + "; echo })"},
	{name: "comment brace in a nested substitution", command: "v=\"$(echo ${x:-$(echo # }\n)}; touch " + corpusFile + ")\""},
	{name: "heredoc brace in a nested substitution", command: "v=\"$(echo ${x:-$(cat <<E\n}\nE\n)}; touch " + corpusFile + ")\""},
	{name: "quoted backtick before a quoted substitution", command: "echo $(echo ${x:-'`'\"$(echo hi > " + corpusFile + ")\"})"},

	// A shell inside a shell, each reading its text its own way.
	{name: "bash -c inside a heredoc only zsh sees past", cwd: "/tmp", command: "v=$(cat <<EOF\nEOF)'\nEOF\n)\nbash -c \"x=\\$(echo \\${x:-{}; touch " + corpusFile + "; echo })\"\n#'\n)"},
	{name: "zsh -c holding a bash reading", command: "zsh -c 'bash -c \"x=\\$(echo \\${x:-{}; touch " + corpusFile + "; echo })\"'"},
	{name: "eval of a parameter expansion", command: "eval 'x=$(echo ${x:-{}; touch " + corpusFile + "; echo })'"},

	// Heredocs and here-strings fed to a shell.
	{name: "escaped backticks in a heredoc fed to bash", command: "bash <<EOF\nv=\\`touch " + corpusFile + "\\`\nEOF"},
	{name: "bash /dev/stdin", command: heredocTo("bash /dev/stdin")},
	{name: "bash /dev/./stdin", command: heredocTo("bash /dev/./stdin")},
	{name: "zsh /dev//fd/0", command: heredocTo("zsh /dev//fd/0")},
	{name: "bash /proc/self/fd/0", command: heredocTo("bash /proc/self/fd/0")},
	{name: "cd /dev then bash stdin", command: heredocTo("cd /dev && bash stdin")},
	{name: "relative path to stdin", cwd: "/tmp", command: heredocTo("bash ../dev/stdin")},
	{name: "variable directory holding stdin", command: heredocTo("D=/dev; bash $D/stdin")},
	{name: "variable naming stdin", command: heredocTo("F=/dev/stdin; bash $F")},
	{name: "cd /dev then sh fd/0", command: heredocTo("cd /dev; sh fd/0")},
	{name: "unknown variable naming the script", command: heredocTo("bash $F")},
	{name: "source /dev/stdin", command: heredocTo("source /dev/stdin")},
	{name: "dot /dev/stdin", command: heredocTo(". /dev/stdin")},
	{name: "heredoc piped to bash", command: "cat <<EOF | bash\necho x >> " + corpusFile + "\nEOF"},

	// Scripts the command generates and then runs.
	{name: "source a generated file", command: "echo 'touch " + corpusFile + "' > /tmp/s\nsource /tmp/s"},
	{name: "run a generated script", command: "cat > /tmp/s <<'EOF'\ntouch " + corpusFile + "\nEOF\nbash /tmp/s"},
	{name: "generated script fed to bash", command: "echo 'touch " + corpusFile + "' > /tmp/s; bash < /tmp/s"},
	{name: "pipe into a subshell holding a shell", command: "cat <<EOF |\necho x >> " + corpusFile + "\nEOF\n(bash)"},
	{name: "shells nested past the depth followed", command: strings.Repeat("eval ", 8) + "'x=${y:-{}; echo' " + corpusDir},
	{name: "source a process substitution", command: "source <(echo 'touch " + corpusFile + "')"},

	// Text no shell reading can be sure of.
	{name: "unclosed quote", command: "echo 'x; touch " + corpusFile},
	{name: "unclosed substitution", command: "echo $(touch " + corpusFile},

	// Memory paths and copies into a memory directory.
	{name: "long s in the memory path", command: "echo x > /home/u/.claude/project\u017f/p/memory/a"},
	{name: "cp source that looks like -t after --", command: "cp -- -at /tmp/x " + corpusDir + "/"},
	{name: "mv into an existing directory", command: "mv memory p", cwd: projects},
	{name: "mv into an existing directory with a slash", command: "mv memory p/", cwd: projects},
	{name: "cp into an existing directory", command: "cp -r memory p", cwd: projects},
	{name: "rsync into an existing directory", command: "rsync -a memory p", cwd: projects},
	{name: "ln into an existing directory", command: "ln -s /tmp/memory p", cwd: projects},
	{name: "install into an existing directory", command: "install /tmp/memory p", cwd: projects},
}

// benignMemoryCommands only read memory, so they must stay allowed.
var benignMemoryCommands = []memoryCommand{
	{name: "cat a memory file", command: "cat " + corpusFile},
	{name: "grep a memory file", command: "grep -n note " + corpusFile},
	{name: "git status in the memory directory", command: "git status", cwd: corpusDir},
	{name: "git status after cd", command: "cd " + corpusDir + " && git status"},
	{name: "read inside bash -c", command: "bash -c 'cat " + corpusFile + "'"},
	{name: "read inside a heredoc fed to bash", command: "bash <<'EOF'\ncat " + corpusFile + "\nEOF"},
	{name: "read inside a quoted substitution", command: `echo "$(head -1 ` + corpusFile + `)"`},
	{name: "unclosed quote without a memory path", command: "echo 'unfinished > out.txt"},
	{name: "source an existing file", command: "source ~/.profile && cat " + corpusFile},
}

func (c memoryCommand) dir() string {
	if c.cwd == "" {
		return "/work/repo"
	}
	return c.cwd
}

func TestMemoryWriteGuardCorpus(t *testing.T) {
	for _, c := range memoryWriteCorpus {
		t.Run(c.name, func(t *testing.T) {
			if _, ok := bashMemoryWrite(c.command, c.dir()); !ok {
				t.Errorf("command %q was allowed", c.command)
			}
		})
	}
	for _, c := range benignMemoryCommands {
		t.Run(c.name, func(t *testing.T) {
			if p, ok := bashMemoryWrite(c.command, c.dir()); ok {
				t.Errorf("command %q was blocked for %q", c.command, p)
			}
		})
	}
}

// The verdict must never allow what the baseline detector blocks.
func TestMemoryWriteGuardNeverAllowsWhatBaselineBlocks(t *testing.T) {
	var all []memoryCommand
	all = append(all, memoryWriteCorpus...)
	all = append(all, benignMemoryCommands...)
	for _, c := range memoryBashCases {
		all = append(all, memoryCommand{name: c.name, command: c.command, cwd: c.cwd})
	}
	blocked := 0
	for _, c := range all {
		if _, ok := memwritebaseline.Write(c.command, c.dir()); !ok {
			continue
		}
		blocked++
		if _, ok := bashMemoryWrite(c.command, c.dir()); !ok {
			t.Errorf("%s: baseline blocks %q, the guard allows it", c.name, c.command)
		}
	}
	if blocked == 0 {
		t.Fatal("the baseline blocked nothing, so this test compared nothing")
	}
}
