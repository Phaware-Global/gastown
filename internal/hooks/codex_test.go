package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRun records each hook call and answers from a table keyed by command.
type fakeRun struct {
	calls   []string
	stdins  map[string][]string
	answers map[string]struct {
		stdout, stderr string
		code           int
	}
}

func newFakeRun() *fakeRun {
	return &fakeRun{stdins: map[string][]string{}, answers: map[string]struct {
		stdout, stderr string
		code           int
	}{}}
}

func (f *fakeRun) answer(command, stdout, stderr string, code int) {
	f.answers[command] = struct {
		stdout, stderr string
		code           int
	}{stdout, stderr, code}
}

func (f *fakeRun) run(command string, stdin []byte, _ time.Duration) ([]byte, []byte, int) {
	f.calls = append(f.calls, command)
	f.stdins[command] = append(f.stdins[command], string(stdin))
	a := f.answers[command]
	return []byte(a.stdout), []byte(a.stderr), a.code
}

func hook(command string) []Hook { return []Hook{{Type: "command", Command: command}} }

// Payload shapes copied from the Codex hook schemas (codex-rs/hooks):
// Bash and exec_command both arrive as tool_name "Bash" with a string
// tool_input.command; apply_patch carries the patch text in command.
func codexToolPayload(tool, command string) []byte {
	b, _ := json.Marshal(map[string]any{
		"session_id": "s1", "turn_id": "t1", "transcript_path": nil, "cwd": "/work/rig",
		"hook_event_name": "PreToolUse", "model": "gpt-test", "permission_mode": "default",
		"tool_name": tool, "tool_use_id": "u1", "tool_input": map[string]any{"command": command},
	})
	return b
}

func TestClaudeMatcherMatches(t *testing.T) {
	cases := []struct {
		matcher, value, command string
		want                    bool
	}{
		{"", "Bash", "ls", true},
		{"*", "anything", "", true},
		{"Bash", "Bash", "ls", true},
		{"Bash", "Edit", "", false},
		{"Write|Edit|NotebookEdit", "Edit", "", true},
		{"Write|Edit|NotebookEdit", "EditX", "", false},
		{"Bash(gh pr create*)", "Bash", "gh pr create --fill", true},
		{"Bash(gh pr create*)", "Bash", "cd rig && gh pr create --fill", true},
		{"Bash(gh pr create*)", "Bash", "echo gh pr create", false},
		{"Bash(gh pr create*)", "Edit", "gh pr create", false},
		{"Bash(*while true*)", "Bash", "while true; do sleep 1; done", true},
		{"Bash(*for *seq*)", "Bash", "for i in $(seq 1 3); do x; done", true},
		{"Bash(*for *seq*)", "Bash", "git status", false},
		{"Bash(git push*)", "Bash", "git status\ngit push origin main", true},
		{"startup|resume", "compact", "", false},
		{"startup|resume", "resume", "", true},
	}
	for _, c := range cases {
		if got := claudeMatcherMatches(c.matcher, c.value, c.command); got != c.want {
			t.Errorf("claudeMatcherMatches(%q, %q, %q) = %v, want %v", c.matcher, c.value, c.command, got, c.want)
		}
	}
}

// The role's Claude hooks run for the matching Codex tool call, a block
// stops at the first exit 2, and an inline guard that writes its reason to
// stdout still gives Codex a non-empty reason (Codex ignores exit 2 with an
// empty stderr).
func TestDispatchCodex_PreToolUseBash(t *testing.T) {
	cfg := &HooksConfig{PreToolUse: []HookEntry{
		{Matcher: "Bash(gh pr create*)", Hooks: hook("guard pr-workflow")},
		{Matcher: "Bash(*while true*)", Hooks: hook("echo 'BLOCKED: no loops' && exit 2")},
		{Matcher: "Bash", Hooks: hook("guard command-substitution")},
		{Matcher: "Write|Edit", Hooks: hook("guard memory-write")},
	}}

	f := newFakeRun()
	res := DispatchCodex("PreToolUse", codexToolPayload("Bash", "git status"), cfg, f.run)
	if res.ExitCode != 0 || strings.Join(f.calls, ",") != "guard command-substitution" {
		t.Fatalf("git status: exit %d, calls %v; want only command-substitution, allowed", res.ExitCode, f.calls)
	}
	if !strings.Contains(f.stdins["guard command-substitution"][0], `"command":"git status"`) {
		t.Errorf("guard did not receive the Codex payload: %s", f.stdins["guard command-substitution"][0])
	}

	f = newFakeRun()
	f.answer("guard pr-workflow", "", "use gt done\n", 2)
	res = DispatchCodex("PreToolUse", codexToolPayload("Bash", "gh pr create --fill"), cfg, f.run)
	if res.ExitCode != 2 || string(res.Stderr) != "use gt done\n" {
		t.Errorf("gh pr create: exit %d stderr %q; want blocked with the guard's reason", res.ExitCode, res.Stderr)
	}
	if strings.Join(f.calls, ",") != "guard pr-workflow" {
		t.Errorf("hooks after a block still ran: %v", f.calls)
	}

	f = newFakeRun()
	f.answer("echo 'BLOCKED: no loops' && exit 2", "BLOCKED: no loops\n", "", 2)
	res = DispatchCodex("PreToolUse", codexToolPayload("Bash", "while true; do x; done"), cfg, f.run)
	if res.ExitCode != 2 || string(res.Stderr) != "BLOCKED: no loops\n" {
		t.Errorf("inline guard: exit %d stderr %q; want blocked with the stdout reason", res.ExitCode, res.Stderr)
	}
}

// apply_patch becomes one Edit call per file, with tool_input.file_path
// resolved against cwd, so Claude's file guards apply.
func TestDispatchCodex_ApplyPatchAsEdit(t *testing.T) {
	cfg := &HooksConfig{PreToolUse: []HookEntry{
		{Matcher: "Write|Edit|NotebookEdit", Hooks: hook("guard memory-write")},
		{Matcher: "Bash", Hooks: hook("guard bash-only")},
	}}
	patch := "*** Begin Patch\n*** Add File: notes.md\n+x\n*** Update File: /abs/old.go\n*** Move to: /abs/new.go\n@@\n-a\n+b\n*** Delete File: gone.txt\n*** End Patch\n"
	f := newFakeRun()
	res := DispatchCodex("PreToolUse", codexToolPayload("apply_patch", patch), cfg, f.run)
	if res.ExitCode != 0 {
		t.Fatalf("exit %d", res.ExitCode)
	}
	var paths []string
	for _, in := range f.stdins["guard memory-write"] {
		var p struct {
			ToolName  string `json:"tool_name"`
			ToolInput struct {
				FilePath string `json:"file_path"`
			} `json:"tool_input"`
		}
		if err := json.Unmarshal([]byte(in), &p); err != nil {
			t.Fatal(err)
		}
		if p.ToolName != "Edit" {
			t.Errorf("tool_name = %q, want Edit", p.ToolName)
		}
		paths = append(paths, p.ToolInput.FilePath)
	}
	want := "/work/rig/notes.md,/abs/old.go,/abs/new.go,/work/rig/gone.txt"
	if strings.Join(paths, ",") != want {
		t.Errorf("file paths = %v, want %s", paths, want)
	}
	if len(f.stdins["guard bash-only"]) != 0 {
		t.Error("a Bash hook ran for apply_patch")
	}

	f = newFakeRun()
	f.answer("guard memory-write", "", "memory write blocked\n", 2)
	res = DispatchCodex("PreToolUse", codexToolPayload("apply_patch", patch), cfg, f.run)
	if res.ExitCode != 2 || len(f.calls) != 1 {
		t.Errorf("block on first file: exit %d calls %v", res.ExitCode, f.calls)
	}
}

func TestDispatchCodex_RequestUserInputAsAskUserQuestion(t *testing.T) {
	cfg := &HooksConfig{PreToolUse: []HookEntry{{Matcher: "AskUserQuestion", Hooks: hook("guard interactive-input")}}}
	f := newFakeRun()
	f.answer("guard interactive-input", "", "no human here\n", 2)
	res := DispatchCodex("PreToolUse", codexToolPayload("request_user_input", ""), cfg, f.run)
	if res.ExitCode != 2 || !strings.Contains(f.stdins["guard interactive-input"][0], `"tool_name":"AskUserQuestion"`) {
		t.Errorf("exit %d stdin %v", res.ExitCode, f.stdins["guard interactive-input"])
	}
}

// Context events pass stdout through; Stop keeps only a JSON object;
// PreCompact drops stdout; SessionStart matchers test the source.
func TestDispatchCodex_EventOutputs(t *testing.T) {
	cfg := &HooksConfig{
		SessionStart:     []HookEntry{{Matcher: "", Hooks: hook("prime")}, {Matcher: "resume", Hooks: hook("resume-only")}},
		UserPromptSubmit: []HookEntry{{Matcher: "", Hooks: []Hook{{Type: "command", Command: "mail"}, {Type: "command", Command: "more"}}}},
		Stop:             []HookEntry{{Matcher: "", Hooks: []Hook{{Type: "command", Command: "costs &"}, {Type: "command", Command: "stop-check"}}}},
		PreCompact:       []HookEntry{{Matcher: "", Hooks: hook("handoff")}},
	}
	f := newFakeRun()
	f.answer("prime", "ROLE CONTEXT", "", 0)
	f.answer("mail", "you have mail", "", 0)
	f.answer("more", "second\n", "", 0)
	f.answer("costs &", "recorded 3 tokens\n", "", 0)
	f.answer("stop-check", "", "commits pending\n", 0)
	f.answer("handoff", "cycling\n", "", 0)

	start, _ := json.Marshal(map[string]any{"session_id": "s1", "source": "compact", "cwd": "/w", "hook_event_name": "SessionStart"})
	if res := DispatchCodex("SessionStart", start, cfg, f.run); string(res.Stdout) != "ROLE CONTEXT" {
		t.Errorf("SessionStart stdout %q", res.Stdout)
	}
	if strings.Contains(strings.Join(f.calls, ","), "resume-only") {
		t.Error("a resume-only hook ran for a compact SessionStart")
	}
	if res := DispatchCodex("UserPromptSubmit", []byte(`{"prompt":"hi"}`), cfg, f.run); string(res.Stdout) != "you have mail\nsecond\n" {
		t.Errorf("UserPromptSubmit stdout %q", res.Stdout)
	}
	res := DispatchCodex("Stop", []byte(`{"stop_hook_active":false}`), cfg, f.run)
	if len(res.Stdout) != 0 || res.ExitCode != 0 || string(res.Stderr) != "commits pending\n" {
		t.Errorf("Stop: stdout %q exit %d stderr %q; want no stdout, exit 0, the reminder on stderr", res.Stdout, res.ExitCode, res.Stderr)
	}
	f.answer("stop-check", `{"decision":"block","reason":"run gt done"}`, "", 0)
	if res := DispatchCodex("Stop", []byte(`{}`), cfg, f.run); string(res.Stdout) != `{"decision":"block","reason":"run gt done"}` {
		t.Errorf("Stop JSON stdout %q", res.Stdout)
	}
	if res := DispatchCodex("PreCompact", []byte(`{"trigger":"auto"}`), cfg, f.run); len(res.Stdout) != 0 {
		t.Errorf("PreCompact stdout %q", res.Stdout)
	}
}

func TestDispatchCodex_UnreadablePayloadRunsNothing(t *testing.T) {
	f := newFakeRun()
	res := DispatchCodex("PreToolUse", []byte("not json"), DefaultBase(), f.run)
	if len(f.calls) != 0 || len(res.Stderr) == 0 {
		t.Errorf("calls %v stderr %q", f.calls, res.Stderr)
	}
}

func TestTargetForRole(t *testing.T) {
	for in, want := range map[string]string{
		"mayor": "mayor", "assistant": "assistant", "deacon": "deacon", "deacon/boot": "boot", "dog": "dog",
		"gastown/witness": "gastown/witness", "gastown/refinery": "gastown/refinery", "gastown/reviewer": "gastown/reviewer",
		"gastown/polecats/rust": "gastown/polecats", "gastown/crew/max": "gastown/crew",
	} {
		if got := TargetForRole(in); got != want {
			t.Errorf("TargetForRole(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCodexRequirementsRoundTrip(t *testing.T) {
	gt := "/Users/agent/.local/bin/gt"
	toml := CodexRequirementsTOML(gt)
	if err := CheckCodexRequirements([]byte(toml), gt); err != nil {
		t.Fatalf("generated requirements fail the check: %v\n%s", err, toml)
	}
	for _, want := range []string{"hooks = true", "memories = false", "additionalContextLimit = 0", `"/Users/agent/.local/bin/gt hooks codex run PreToolUse"`} {
		if !strings.Contains(toml, want) {
			t.Errorf("requirements missing %q", want)
		}
	}
	if err := CheckCodexRequirements([]byte(toml), "/opt/other/gt"); err == nil {
		t.Error("check passed for a different gt binary")
	}
	broken := []struct{ name, from, to string }{
		{"hooks off", "hooks = true", "hooks = false"},
		{"memories on", "memories = false", "memories = true"},
		{"Stop missing", `"` + gt + ` hooks codex run Stop"`, `"/bin/true"`},
	}
	for _, b := range broken {
		if err := CheckCodexRequirements([]byte(strings.Replace(toml, b.from, b.to, 1)), gt); err == nil {
			t.Errorf("%s: check passed", b.name)
		}
	}
	filtered := strings.Replace(toml, "[[hooks.PreToolUse]]\n", "[[hooks.PreToolUse]]\nmatcher = \"^Bash$\"\n", 1)
	if err := CheckCodexRequirements([]byte(filtered), gt); err == nil {
		t.Error("check passed when the PreToolUse dispatcher only runs for Bash")
	}
}

func TestVerifyCodexManagedHooksAt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "requirements.toml")
	if err := verifyCodexManagedHooksAt(path, "/x/gt"); err == nil || !strings.Contains(err.Error(), "sudo tee") {
		t.Errorf("missing file: %v", err)
	}
	if err := os.WriteFile(path, []byte(CodexRequirementsTOML("/x/gt")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifyCodexManagedHooksAt(path, "/x/gt"); err != nil {
		t.Errorf("installed file: %v", err)
	}
}

// RunCodexHookCommand returns when the shell exits even if the hook
// backgrounds a process that keeps the output open.
func TestRunCodexHookCommand(t *testing.T) {
	out, errOut, code := RunCodexHookCommand(`read x; echo "got $x"; echo oops >&2; exit 2`, []byte("payload\n"), 5*time.Second)
	if string(out) != "got payload\n" || string(errOut) != "oops\n" || code != 2 {
		t.Errorf("out %q err %q code %d", out, errOut, code)
	}
	start := time.Now()
	_, _, code = RunCodexHookCommand("sleep 3 &", nil, 5*time.Second)
	if code != 0 || time.Since(start) > 2*time.Second {
		t.Errorf("backgrounded hook: code %d after %s", code, time.Since(start))
	}
	_, errOut, code = RunCodexHookCommand("sleep 5", nil, 200*time.Millisecond)
	if code != 1 || !strings.Contains(string(errOut), "timed out") {
		t.Errorf("timeout: code %d err %q", code, errOut)
	}
}
