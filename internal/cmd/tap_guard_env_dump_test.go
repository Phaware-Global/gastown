package cmd

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// runEnvDumpGuard feeds one raw hook payload to the guard and reports whether
// it blocked, plus what it printed to stderr.
func runEnvDumpGuard(t *testing.T, payload []byte) (bool, string) {
	t.Helper()
	dir := t.TempDir()
	in, err := os.CreateTemp(dir, "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if _, err := in.Write(payload); err != nil {
		t.Fatal(err)
	}
	if _, err := in.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	errFile, err := os.CreateTemp(dir, "stderr")
	if err != nil {
		t.Fatal(err)
	}
	defer errFile.Close()

	oldIn, oldErr := os.Stdin, os.Stderr
	os.Stdin, os.Stderr = in, errFile
	runErr := runTapGuardEnvDump(tapGuardEnvDumpCmd, nil)
	os.Stdin, os.Stderr = oldIn, oldErr

	out, err := os.ReadFile(errFile.Name())
	if err != nil {
		t.Fatal(err)
	}
	if runErr == nil {
		return false, string(out)
	}
	if code, ok := IsSilentExit(runErr); !ok || code != 2 {
		t.Fatalf("guard returned %v, want exit 2", runErr)
	}
	return true, string(out)
}

func envDumpPayload(t *testing.T, command string) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{
		"tool_name":  "Bash",
		"tool_input": map[string]any{"command": command},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestEnvDumpGuardBlocks(t *testing.T) {
	commands := []string{
		// Whole-environment listings.
		"env",
		"env -0",
		"env -u HOME",
		"/usr/bin/env",
		`\env`,
		`"env"`,
		"FOO=1 env",
		"env > /tmp/dump.txt",
		"env 2>&1",
		"printenv",
		"printenv -0",
		"set",
		"set | grep GT_",
		"export",
		"export -p",
		"declare",
		"declare -x",
		"declare -p",
		"typeset -x",
		"readonly -p",
		"ps eww",
		"ps auxe",
		"ps -E -p 1",
		// Pipelines that start with a listing, whatever the redaction.
		`env | sed 's/\(TOKEN=\).*/\1***/'`,
		"env | grep -v SECRET",
		"env | sort",
		"printenv | grep GT_",
		"env | cut -d= -f2",
		"env | cut -d= -f1-2",
		"env | cut -d= -f1,2",
		"env -0 | cut -d= -f1",
		"env >&2 | cut -d= -f1",
		"env | tee /tmp/e | cut -d= -f1",
		"env | grep -o 'ghp_.*' | cut -d= -f1",
		"set | cut -d= -f1",
		// Cutting a listing to names does not hide multi-line values.
		"env | cut -d= -f1",
		"env | cut -s -d= -f1",
		"env | cut -d '=' -f 1 | sort",
		"env | grep ^GT_ | cut -d= -f1",
		"env | sort | cut -s -d= -f1",
		"env 2>/dev/null | cut -d= -f1",
		"printenv | cut -d= -f1",
		`env | awk -F= '{print $1}'`,
		// Descriptors above 2.
		"env 3>&1",
		"env 3>&1 1>/dev/null",
		"env 3>/tmp/dump",
		"env {fd}>&1",
		"printenv {fd}>/tmp/dump",
		"exec 3>&1; env >&3",
		`echo "$GH_TOKEN" 3>&1`,
		// A secret piped into a consumer: the guard cannot tell it from a
		// command that displays its input.
		`echo "$DB_PASSWORD" | docker login -u me --password-stdin`,
		`printf %s "$GH_TOKEN" | gh auth login --with-token`,
		// A secret variable by name.
		"printenv GH_TOKEN",
		"printenv HOME WEBHOOK_SECRET",
		"declare -p GH_TOKEN",
		"typeset GH_TOKEN",
		"export -p NPM_TOKEN",
		"echo $GH_TOKEN",
		`echo "$GH_TOKEN"`,
		`echo "token: ${GITHUB_PAT}"`,
		"echo ${API_KEY:0:4}",
		"echo ${DB_PASSWORD:-unset}",
		`echo "${HOME:+$GH_TOKEN}"`,
		"echo $PGPASSWORD",
		"echo $WEBHOOK_SECRET_PROD",
		`printf '%s\n' "$WEBHOOK_SECRET"`,
		`print -r -- $GH_TOKEN`,
		`echo "$GH_TOKEN" | cut -c1-4`,
		`echo $GH_TOKEN | sed 's/./*/g'`,
		`t=$GH_TOKEN; echo "$t"`,
		`export t="${GH_TOKEN}"; printf %s "$t"`,
		`cat <<< "$GH_TOKEN"`,
		"cat <<EOF\nvalue=$GH_TOKEN\nEOF",
		// Process environment files.
		"cat /proc/self/environ",
		"cat /proc/*/environ",
		"cat /proc/$$/environ",
		"cat /proc/$(pgrep -n gt)/environ",
		`tr '\0' '\n' < /proc/1/environ`,
		"strings /proc/123/environ | grep TOKEN",
		// Reached through other constructs.
		"cd /tmp && env",
		"true; printenv",
		"false || env",
		"ls\nenv",
		"(env)",
		"{ env; }",
		"if true; then env; fi",
		`x=$(env); echo done`,
		`echo "$(env)"`,
		"echo `printenv`",
		`diff <(env) /tmp/old`,
		"cat <(env)",
		"diff <(env) x",
		"cat < <(printenv)",
		"tee >(env) </dev/null",
		`sh -c 'env'`,
		`bash -lc "echo \$GH_TOKEN"`,
		`zsh -c 'printenv | head'`,
		`eval "env"`,
		"command env",
		"timeout 5 env",
		"nohup printenv",
		"sudo env",
		"env -i sh -c 'echo $GH_TOKEN'",
		"env FOO=1 printenv",
		`env -S 'printenv GH_TOKEN'`,
		"echo GH_TOKEN | xargs printenv",
		"bash <<'EOF'\nenv\nEOF",
		"a=(1 2); env",
		"git log -1 \"$(cat <<'EOF'\nit's fine\nEOF\n)\"; env",
		"echo \"$( # don't\nprintenv GH_TOKEN)\"",
		"set -x; set",
		"echo ${HOME:-$(env)}",
		"cat <<EOF\n$(printenv)\nEOF",
		"env &> /tmp/e",
		"env |& grep TOKEN",
		// Indirect expansion: the guard cannot know which variable it prints.
		"n=GH_TOKEN; echo ${!n}",
		"for v in GH_TOKEN; do echo ${!v}; done",
		`compgen -v | while read v; do echo "$v=${!v}"; done`,
		"echo ${!1}",
		"echo ${!#}",
		`echo "${!v:-unset}"`,
		`echo "${!v@Q}"`,
		"echo ${!v:-x*}",
		"echo ${!arr[0]}",
		"echo ${(P)name}",
		`print -r -- "${(@P)name}"`,
		`echo "${${(P)name}}"`,
		`echo "${HOME:-${!v}}"`,
		`printf '%s\n' "${!v}"`,
		`cat <<< "${!v}"`,
		"cat <<EOF\nvalue=${!v}\nEOF",
		`x=${!v}; echo "$x"`,
		`a=("${!v}"); echo "${a[0]}"`,
		`printenv "${!v}"`,
		`declare -p "${!v}"`,
		// Shell flags around -c.
		"bash -ce 'env'",
		"sh -c -e 'env'",
		"bash -e -c 'env'",
		"bash -xc 'printenv'",
		"bash -c -- 'env'",
		"bash -eo pipefail -c 'env'",
		"bash -c -o pipefail 'env'",
		"bash -O extglob -c 'env'",
		"bash --norc -c 'env'",
		"bash --rcfile /dev/null -c 'env'",
		"bash +e -c 'env'",
		"zsh -oerrexit -c 'env'",
		"zsh --emulate sh -c 'env'",
		"sudo bash -e -c 'env'",
		// zsh pattern listings, and listings with no names.
		"typeset -m",
		"typeset -m '*'",
		"typeset -m 'GH_*'",
		"typeset -xm 'GH*'",
		"export -m '*TOKEN*'",
		"zsh -c \"typeset -m '*'\"",
		"typeset -p",
		"typeset -p 1",
		"typeset -x",
		// A name built at run time.
		`printenv "$n"`,
		"for v in GH_TOKEN NPM_TOKEN; do printenv $v; done",
		"printenv $(echo GH_TOKEN)",
		"printenv `echo GH_TOKEN`",
		"printenv GH_TOKE?",
		"printenv {GH,NPM}_TOKEN",
		`printenv HOME "$@"`,
		`declare -p "$n"`,
		"for v in GH_TOKEN; do typeset -p $v; done",
		"declare -p $(echo GH_TOKEN)",
		`export -p "$n"`,
		`declare "$n"`,
		`local "$1"`,
		"export $(cat .env)",
		`declare -p "$k=$v"`,
		`env "$n"`,
		// Indirection and namerefs.
		"echo ${(e)x}",
		`echo "${x@P}"`,
		`echo "${arr[@]@P}"`,
		"echo $=GH_TOKEN",
		"echo $~GH_TOKEN",
		"declare -n r=GH_TOKEN; echo $r",
		`f() { local -n r=$1; echo "$r"; }; f GH_TOKEN`,
		"typeset -n r=GH_TOKEN; print $r",
		`declare -n r; r=GH_TOKEN; echo "$r"`,
		// Script text built at run time.
		`eval "echo \$$v"`,
		`eval "$cmd"`,
		`eval "$@"`,
		"eval $(ssh-agent -s)",
		`eval $'\x65nv'`,
		"eval {env,}",
		`bash -c "$cmd"`,
		`sh -c "echo $v"`,
		`env -S "$cmd"`,
		// A shell fed its script by stdin.
		"bash <<< 'env'",
		`bash <<< "$cmd"`,
		"echo env | bash",
		"(echo env) | sh",
		"echo env | sudo bash -e",
		"bash < x",
		"bash -s < x",
		"sh -s",
		"bash -s arg <<< 'env'",
		"bash /dev/stdin < x",
		"bash <(echo env)",
		"source <(echo env)",
		". /dev/stdin <<< 'env'",
		"bash <<EOF\n$cmd\nEOF",
		"bash <<EOF\necho \\$GH_TOKEN\nEOF",
		// Unparseable and suspicious.
		`echo "$GH_TOKEN`,
		`env "unterminated`,
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			if blocked, _ := runEnvDumpGuard(t, envDumpPayload(t, command)); !blocked {
				t.Errorf("command %q was allowed", command)
			}
		})
	}
}

func TestEnvDumpGuardAllows(t *testing.T) {
	commands := []string{
		// Names only.
		"compgen -e",
		"compgen -e | grep TOKEN",
		"bash -c 'compgen -e'",
		// Presence checks.
		`test -n "$GH_TOKEN" && echo set`,
		`[ -n "$GH_TOKEN" ] && echo set || echo unset`,
		`[[ -z $GH_TOKEN ]] || echo set`,
		`echo "${GH_TOKEN:+set}"`,
		`echo ${#GH_TOKEN}`,
		// Using a secret without printing it.
		`curl -H "Authorization: Bearer $GH_TOKEN" https://example.com`,
		"GH_TOKEN=$GITHUB_PAT gh api user",
		"export GH_TOKEN",
		"export GH_TOKEN=$GITHUB_PAT",
		// Non-secret values.
		"echo $HOME",
		`echo "$PATH"`,
		"echo $GT_ROLE $key $token_count",
		"printenv HOME",
		"printf '%s\\n' \"$PWD\"",
		// Literal text that only mentions these.
		"echo '$GH_TOKEN'",
		`echo \$GH_TOKEN`,
		`echo "run env to check"`,
		`git commit -m "guard env and printenv"`,
		"grep -rn printenv .",
		"ls # env",
		"cat > notes.md <<'EOF'\nenv\necho $GH_TOKEN\nEOF",
		// Ordinary use of the same programs.
		"env FOO=1 go test ./...",
		"env -i PATH=/usr/bin make",
		"#!/usr/bin/env bash",
		"/usr/bin/env python3 script.py",
		"env --help",
		"command -v env",
		"which printenv",
		"set -euo pipefail",
		"set -e; make",
		"set -- a b",
		"set -o pipefail",
		"export FOO=bar",
		"declare -a items=()",
		"declare -f myfunc",
		"typeset -i count=0",
		"local API_KEY",
		"ps -ef",
		"ps aux",
		"ps -p 123 -o command=",
		"ls -l /proc/self/environ",
		"cat .env.example",
		`git commit -m "block cat /proc/*/environ"`,
		"git commit -m \"$(cat <<'EOF'\nfix: don't set the (export) env\n\nprintenv\nEOF\n)\"",
		"x=$( # it's the env\n  date )",
		"opts=(set export env); echo ${opts[0]}",
		"echo \"$(date) (set)\"",
		"go env GOMODCACHE",
		`echo $((1 + 2))`,
		// Names, keys and presence through indirection print no value.
		"echo ${!GH_*}",
		`echo "${!GH_@}"`,
		`echo "${!arr[@]}"`,
		"echo ${!arr[*]}",
		`echo "${!v:+set}"`,
		`test -n "${!v}" && echo set`,
		`echo $! "${!}"`,
		`echo '${!v}'`,
		// Literal names and literal scripts.
		"printenv HOME PATH",
		"declare -p HOME",
		`export PATH="$HOME/bin:$PATH"`,
		`local dir="$1" out`,
		`export "$k=$v"`,
		`env FOO="$bar" make`,
		`test -n "$GH_TOKEN" && echo set`,
		"eval 'echo $HOME'",
		"bash <<< 'compgen -e'",
		"bash -s <<< 'ls'",
		"bash <<'EOF'\nls \"$HOME\"\nEOF",
		`bash "$DIR/build.sh"`,
		`source "$HOME/.profile"`,
		"bash --version",
		`bash -c 'echo "$HOME"'`,
		// Shells running something harmless, or a script file.
		"bash -ec 'compgen -e'",
		"bash -o pipefail -c 'ls'",
		"bash -e build.sh",
		"bash build.sh -c env",
		"typeset -i count",
	}
	for _, command := range commands {
		t.Run(command, func(t *testing.T) {
			if blocked, out := runEnvDumpGuard(t, envDumpPayload(t, command)); blocked {
				t.Errorf("command %q was blocked:\n%s", command, out)
			}
		})
	}
}

func TestEnvDumpGuardFailsClosed(t *testing.T) {
	for name, payload := range map[string]string{
		"empty":      "",
		"not json":   "env",
		"no command": `{"tool_name":"Bash","tool_input":{}}`,
	} {
		if blocked, _ := runEnvDumpGuard(t, []byte(payload)); !blocked {
			t.Errorf("%s payload was allowed", name)
		}
	}
	nested := "env"
	for i := 0; i < 20; i++ {
		nested = "eval " + nested
	}
	if blocked, _ := runEnvDumpGuard(t, envDumpPayload(t, nested)); !blocked {
		t.Error("over-nested command was allowed")
	}
}

func TestEnvDumpGuardMessageNamesSafeCheck(t *testing.T) {
	_, out := runEnvDumpGuard(t, envDumpPayload(t, "echo $GH_TOKEN"))
	for _, want := range []string{
		`test -n "$VAR" && echo set`,
		"bash -c 'compgen -e'",
		"GH_TOKEN",
		"cmd --password-stdin < file",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("block message lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "cut ") {
		t.Errorf("block message recommends cut:\n%s", out)
	}
}

// Every command the guard allows here must keep a multi-line value off its
// output, including the lines that carry no = or end in one.
func TestEnvDumpGuardAllowedFormsHideMultiLineValues(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not found")
	}
	value := "marker-first\nmarker-bare\nmarker-padded==\nmarker-last"
	environment := []string{"PATH=" + os.Getenv("PATH"), "DEMO_PRIVATE_KEY=" + value}

	for command, advised := range map[string]bool{
		"compgen -e":                              true,
		"bash -c 'compgen -e'":                    true,
		"compgen -e | grep KEY":                   true,
		`test -n "$DEMO_PRIVATE_KEY" && echo set`: true,
		`echo "${DEMO_PRIVATE_KEY:+set}"`:         true,
		"env | cut -d= -f1":                       false,
		"env | cut -s -d= -f1":                    false,
		`env | awk -F= '{print $1}'`:              false,
		"printenv | cut -d= -f1":                  false,
	} {
		t.Run(command, func(t *testing.T) {
			blocked, _ := runEnvDumpGuard(t, envDumpPayload(t, command))
			if blocked {
				if advised {
					t.Errorf("command %q was blocked", command)
				}
				return
			}
			run := exec.Command(bash, "-c", command)
			run.Env = environment
			out, err := run.CombinedOutput()
			if err != nil {
				t.Fatalf("running %q: %v", command, err)
			}
			if strings.Contains(string(out), "marker-") {
				t.Errorf("allowed command %q prints the value", command)
			}
		})
	}
}
