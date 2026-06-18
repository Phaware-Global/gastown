package mail

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/telemetry"
	"github.com/steveyegge/gastown/internal/util"
)

const (
	// bdReadTimeout is the timeout for bd read operations (list, show, query).
	// 120s covers read operations under Dolt memory pressure (was 60s, caused
	// signal:killed when issues.jsonl > 40MB slows bd past the previous ceiling).
	// Investigation: hq-2dr55.
	bdReadTimeout = 120 * time.Second
	// bdWriteTimeout is the timeout for bd write operations (create, close, label, reopen).
	// 120s covers write paths (bd q/delete: 3-4s each) under concurrent agent load
	// where Dolt paging pushes write latency well past the previous 60s ceiling.
	// Investigation: hq-2dr55.
	bdWriteTimeout = 120 * time.Second
	// bdProbeTimeout is a short timeout for "is this no-op?" probe reads that
	// run before a write to skip an already-satisfied UPDATE+DOLT_COMMIT cycle.
	// Bounded at 5s so a slow/dead Dolt cannot double the tail latency of
	// MarkRead / closeInDir on the failure path: worst case is one probe miss
	// (5s) followed by the actual write hitting bdWriteTimeout (120s), instead
	// of two back-to-back 120s timeouts. The probe failing falls through to
	// the unguarded write — semantically a no-op, just no fast path.
	bdProbeTimeout = 5 * time.Second
)

// bdError represents an error from running a bd command.
// It wraps the underlying error and includes the stderr output for inspection.
type bdError struct {
	Err    error
	Stderr string
}

// Error implements the error interface.
func (e *bdError) Error() string {
	if e.Stderr != "" {
		return e.Stderr
	}
	if e.Err != nil {
		return e.Err.Error()
	}
	return "unknown bd error"
}

// Unwrap returns the underlying error for errors.Is/As compatibility.
func (e *bdError) Unwrap() error {
	return e.Err
}

// ContainsError checks if the stderr message contains the given substring.
func (e *bdError) ContainsError(substr string) bool {
	return strings.Contains(e.Stderr, substr)
}

// runBdCommand executes a bd command with a context timeout and proper environment setup.
// ctx controls the deadline/timeout for the subprocess.
// workDir is the directory to run the command in.
// beadsDir is the BEADS_DIR environment variable value.
// extraEnv contains additional environment variables to set (e.g., "BD_IDENTITY=...").
// Returns stdout bytes on success, or a *bdError on failure.
func runBdCommand(ctx context.Context, args []string, workDir, beadsDir string, extraEnv ...string) (_ []byte, retErr error) {
	defer func() { telemetry.RecordMail(ctx, "bd."+firstArg(args), retErr) }()

	// Remove stale dolt-server.pid before spawning bd. A stale PID file causes
	// bd to connect to port 3307 which may be occupied by a different Dolt server
	// serving different databases, resulting in hangs until the read timeout kills it.
	beads.CleanStaleDoltServerPID(beadsDir)

	// bd v0.59+ requires --flat for list --json to produce JSON output.
	// Without it, bd returns human-readable tree format that fails JSON parsing.
	// The mail package calls bd directly (not via beads.Run), so it needs its
	// own injection. (GH#2746)
	args = beads.InjectFlatForListJSON(args)

	cmd := exec.CommandContext(ctx, "bd", args...) //nolint:gosec // G204: bd is a trusted internal tool
	cmd.Dir = workDir
	util.SetDetachedProcessGroup(cmd)

	cmd.Env = bdSubprocessEnv(cmd.Environ(), beadsDir, isMailBdReadCommand(args), extraEnv)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()

	// If bd doesn't support --flat (< v0.59), retry without it.
	// Same fallback pattern as beads.Run. (GH#2746)
	if runErr != nil && strings.Contains(stderr.String(), "unknown flag: --flat") {
		retryArgs := make([]string, 0, len(args))
		for _, a := range args {
			if a != "--flat" {
				retryArgs = append(retryArgs, a)
			}
		}
		stdout.Reset()
		stderr.Reset()
		retryCmd := exec.CommandContext(ctx, "bd", retryArgs...) //nolint:gosec // G204: bd is a trusted internal tool
		retryCmd.Dir = workDir
		util.SetDetachedProcessGroup(retryCmd)
		retryCmd.Env = cmd.Env
		retryCmd.Stdout = &stdout
		retryCmd.Stderr = &stderr
		runErr = retryCmd.Run()
	}

	if runErr != nil {
		return nil, &bdError{
			Err:    runErr,
			Stderr: strings.TrimSpace(stderr.String()),
		}
	}

	return stdout.Bytes(), nil
}

// firstArg returns args[0] or "" when the slice is empty.
func firstArg(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return ""
}

// resolveBdTimeout returns the configured timeout, honoring GT_BD_TIMEOUT_SEC
// env var override (same var as beads.resolveBdSubprocessTimeout).
func resolveBdTimeout(defaultTimeout time.Duration) time.Duration {
	if v := os.Getenv("GT_BD_TIMEOUT_SEC"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	return defaultTimeout
}

func bdSubprocessEnv(baseEnv []string, beadsDir string, readOnly bool, extraEnv []string) []string {
	base := append(append([]string{}, baseEnv...), extraEnv...)
	mode := beads.MutationRouting
	if readOnly {
		mode = beads.ReadOnlyRouting
	}
	if beadsDir != "" {
		if readOnly {
			mode = beads.ReadOnlyPinned
		} else {
			mode = beads.MutationPinned
		}
	}
	env := beads.EnvForSubprocessMode(base, beadsDir, mode)
	env = append(env, telemetry.OTELEnvForSubprocess()...)
	return env
}

func filterEnvKey(env []string, key string) []string {
	prefix := key + "="
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func isMailBdReadCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "list", "show", "search":
		return true
	case "message":
		return len(args) >= 2 && args[1] == "thread"
	case "mol":
		return len(args) >= 3 && args[1] == "wisp" && args[2] == "list"
	case "sql":
		query := ""
		for i := len(args) - 1; i >= 1; i-- {
			if !strings.HasPrefix(args[i], "-") {
				query = args[i]
				break
			}
		}
		q := strings.ToLower(strings.TrimSpace(query))
		return strings.HasPrefix(q, "select") || strings.HasPrefix(q, "show") || strings.HasPrefix(q, "explain") || strings.HasPrefix(q, "describe") || strings.HasPrefix(q, "with")
	default:
		return false
	}
}

func filterBdTargetEnv(env []string) []string {
	return beads.StripBDTargetEnv(env)
}

// bdReadCtx returns a context with the standard bd read timeout.
//
//nolint:gosec // The cancel function is returned to callers, who are responsible for invoking it.
func bdReadCtx() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), resolveBdTimeout(bdReadTimeout))
	return ctx, cancel
}

// bdWriteCtx returns a context with the standard bd write timeout.
//
//nolint:gosec // The cancel function is returned to callers, who are responsible for invoking it.
func bdWriteCtx() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), resolveBdTimeout(bdWriteTimeout))
	return ctx, cancel
}

// bdProbeCtx returns a context with the short probe timeout for is-this-noop
// reads that gate a subsequent write. Bounded so a slow Dolt cannot double
// the tail latency of the gated operation.
//
//nolint:gosec // The cancel function is returned to callers, who are responsible for invoking it.
func bdProbeCtx() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithTimeout(context.Background(), bdProbeTimeout)
	return ctx, cancel
}
