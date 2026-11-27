package cmd

import (
	"encoding/json"
	"os"
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
		"env | cut -d= -f1",
		"env | cut -d '=' -f 1 | sort",
		"env | grep ^GT_ | cut -d= -f1",
		"env | sort | cut -s -d= -f1",
		"env 2>/dev/null | cut -d= -f1",
		"printenv | cut -d= -f1",
		`env | awk -F= '{print $1}'`,
		"compgen -e",
		"compgen -e | grep TOKEN",
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
	for _, want := range []string{`test -n "$VAR" && echo set`, "compgen -e"} {
		if !strings.Contains(out, want) {
			t.Errorf("block message lacks %q:\n%s", want, out)
		}
	}
}
