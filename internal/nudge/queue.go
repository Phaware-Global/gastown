// Package nudge provides non-destructive nudge delivery for Gas Town agents.
//
// The nudge queue allows messages to be delivered cooperatively: instead of
// sending text directly to a tmux session (which cancels in-flight tool calls),
// nudges are written to a queue directory and picked up by the agent's
// UserPromptSubmit hook at the next natural turn boundary.
//
// Queue location: <townRoot>/.runtime/nudge_queue/<session>/
// Each nudge is a JSON file named by timestamp for FIFO ordering.
package nudge

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
)

// Priority levels for nudge delivery.
const (
	// PriorityNormal is the default — delivered at next turn boundary.
	PriorityNormal = "normal"
	// PriorityUrgent means the agent should handle this promptly.
	PriorityUrgent = "urgent"
)

// Operational limits and defaults.
// These are compiled-in fallbacks. Configurable via operational.nudge
// in settings/config.json (ZFC pattern).
const (
	// DefaultNormalTTL is the time-to-live for normal-priority nudges.
	DefaultNormalTTL = 30 * time.Minute

	// DefaultUrgentTTL is the time-to-live for urgent-priority nudges.
	DefaultUrgentTTL = 2 * time.Hour

	// MaxQueueDepth is the maximum number of pending nudges per session.
	MaxQueueDepth = 50

	// staleClaimThreshold is how long a .claimed file must be untouched
	// before Drain considers it orphaned (from a crashed drainer) and removes it.
	staleClaimThreshold = 5 * time.Minute
)

// nudgeConfig loads nudge-specific thresholds from town settings.
func nudgeConfig(townRoot string) *config.NudgeThresholds {
	return config.LoadOperationalConfig(townRoot).GetNudgeConfig()
}

// QueuedNudge represents a nudge message stored in the queue.
type QueuedNudge struct {
	Sender    string    `json:"sender"`
	Message   string    `json:"message"`
	Priority  string    `json:"priority"`
	Kind      string    `json:"kind,omitempty"`
	ThreadID  string    `json:"thread_id,omitempty"`
	Severity  string    `json:"severity,omitempty"`
	Timestamp time.Time `json:"timestamp"`
	ExpiresAt time.Time `json:"expires_at,omitempty"`
	// DeliverAfter, if non-zero, defers delivery until this time has passed.
	// Drain skips (but does not discard) the nudge until the deadline is met.
	DeliverAfter time.Time `json:"deliver_after,omitempty"`
}

// queueDir returns the nudge queue directory for a given session.
// Path: <townRoot>/.runtime/nudge_queue/<session>/
//
// The session name is reduced to a single, traversal-safe path component. In the
// current threat model the name comes from a trusted parent (GT_SESSION / the
// canonical session resolver), never attacker-controlled input, but this is
// defense-in-depth: neutralizing separators and parent-dir components guarantees
// a malformed name can never escape <townRoot>/.runtime/nudge_queue, no matter
// which side (enqueue or drain) constructs the path.
func queueDir(townRoot, session string) string {
	safe := strings.ReplaceAll(session, "/", "_")
	safe = strings.ReplaceAll(safe, `\`, "_")
	safe = strings.ReplaceAll(safe, "..", "_")
	if safe == "" || safe == "." {
		safe = "_"
	}
	return filepath.Join(townRoot, constants.DirRuntime, "nudge_queue", safe)
}

// randomSuffix returns a short random hex string to disambiguate filenames
// when multiple processes enqueue within the same nanosecond.
func randomSuffix() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// gcExpired removes queued (unclaimed) nudges past their ExpiresAt from dir.
// It runs on every Enqueue call so that dead entries never occupy cap space —
// nothing else drains a queue belonging to a long-lived agent that never
// calls Drain, so without this the cap wedges permanently full of expired
// messages and every subsequent Enqueue is silently refused (gt-770z).
//
// Only .json (unclaimed) entries are considered: a .claimed file is already
// mid-delivery to some caller and is handled by the orphan-claim sweep in
// DrainClaim, not here. Malformed entries are left alone — that cleanup is
// DrainClaim's job, not the cap check's.
func gcExpired(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	now := time.Now()
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var n QueuedNudge
		if err := json.Unmarshal(data, &n); err != nil {
			continue
		}
		if !n.ExpiresAt.IsZero() && now.After(n.ExpiresAt) {
			_ = os.Remove(path)
		}
	}
}

// Enqueue writes a nudge to the queue for the given session.
// The nudge will be picked up by the agent's hook at the next turn boundary.
// Returns an error if the queue is full (MaxQueueDepth reached).
func Enqueue(townRoot, session string, nudge QueuedNudge) error {
	dir := queueDir(townRoot, session)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("creating nudge queue dir: %w", err)
	}

	// GC expired entries before enforcing the cap: an entry past its expiry
	// must never occupy cap space and block a live message.
	gcExpired(dir)

	// Check queue depth before writing to prevent runaway senders.
	maxDepth := nudgeConfig(townRoot).MaxQueueDepthV()
	pending, _ := Pending(townRoot, session)
	if pending >= maxDepth {
		return fmt.Errorf("nudge queue for %s is full (%d/%d pending)", session, pending, maxDepth)
	}

	if nudge.Timestamp.IsZero() {
		nudge.Timestamp = time.Now()
	}
	if nudge.Priority == "" {
		nudge.Priority = PriorityNormal
	}

	// Set expiry if not already specified by the caller.
	if nudge.ExpiresAt.IsZero() {
		switch nudge.Priority {
		case PriorityUrgent:
			nudge.ExpiresAt = nudge.Timestamp.Add(DefaultUrgentTTL)
		default:
			nudge.ExpiresAt = nudge.Timestamp.Add(DefaultNormalTTL)
		}
	}

	data, err := json.MarshalIndent(nudge, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling nudge: %w", err)
	}

	// Use nanosecond timestamp + random suffix for unique, ordered filenames.
	// The random suffix prevents collisions when multiple agents enqueue
	// nudges for the same session within the same nanosecond.
	filename := fmt.Sprintf("%d-%s.json", nudge.Timestamp.UnixNano(), randomSuffix())
	path := filepath.Join(dir, filename)

	if err := os.WriteFile(path, data, 0644); err != nil {
		return fmt.Errorf("writing nudge to queue: %w", err)
	}

	return nil
}

// Requeue writes previously drained nudges back to the queue for later delivery.
// Existing timestamps are preserved so FIFO ordering remains stable relative to
// one another; only expired nudges are skipped.
func Requeue(townRoot, session string, nudges []QueuedNudge) error {
	for _, n := range nudges {
		if !n.ExpiresAt.IsZero() && time.Now().After(n.ExpiresAt) {
			continue
		}
		if err := Enqueue(townRoot, session, n); err != nil {
			return err
		}
	}
	return nil
}

// Drain reads and removes all ready queued nudges for a session, returning them
// in FIFO order. It is DrainClaim immediately followed by CommitClaims: the
// delivered nudges are removed from the queue before returning.
//
// Callers that run inside a hook whose stdout is discarded on timeout, and that
// therefore must NOT lose nudges if the process is killed before its output is
// accepted, should use DrainClaim + CommitClaims instead — claiming here, then
// committing only once the nudges have actually been handed off.
func Drain(townRoot, session string) ([]QueuedNudge, error) {
	nudges, claims, err := DrainClaim(townRoot, session)
	if err != nil {
		return nil, err
	}
	// Claim removal is best-effort and already logged inside CommitClaims. Do
	// NOT surface its error: a removal failure never loses nudges (the orphan
	// sweep requeues any leftover claim), and propagating it would make existing
	// callers (nudge_poller, propulsion) that treat a non-nil error as "nothing
	// to deliver" discard the nudges they just read. This preserves the
	// pre-split contract — Drain's error reflects only the queue read, so a nil
	// error means the returned nudges are the authoritative drain result.
	_ = CommitClaims(claims)
	return nudges, nil
}

// DrainClaim reads all ready queued nudges for a session in FIFO order, claiming
// each by atomically renaming it to a ".claimed" file, and returns the nudges
// together with the claimed file paths. Unlike Drain, it does NOT remove the
// claimed files: the caller MUST call CommitClaims(paths) once the nudges have
// been delivered.
//
// This two-phase drain prevents nudge loss when the caller runs inside a hook
// whose output is discarded on timeout. If the process dies (or is killed) after
// claiming but before CommitClaims, the ".claimed" files survive on disk and the
// orphan-requeue sweep below restores them to ".json" for redelivery on a later
// drain — rather than the file being removed before its content ever reached the
// agent. Expired, malformed, and not-yet-due (deferred) nudges are still handled
// inline (removed or unclaimed) since they are never delivered.
//
// Uses rename-then-process to prevent concurrent drainers from delivering the
// same nudge twice: each file is atomically renamed to a unique .claimed suffix
// before reading, so only one caller can claim each nudge.
//
// Orphaned .claimed files from crashed/killed drainers are swept if older than
// the configured stale-claim threshold.
func DrainClaim(townRoot, session string) ([]QueuedNudge, []string, error) {
	dir := queueDir(townRoot, session)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("reading nudge queue: %w", err)
	}

	// Requeue orphaned .claimed files from abandoned drainers. A .claimed file
	// older than staleClaimThreshold is treated as orphaned and renamed back to
	// .json so it is picked up on a future drain, rather than deleted (which
	// would permanently drop the nudge).
	//
	// INVARIANT: staleClaimThreshold must exceed the longest time any caller
	// legitimately holds a claim before CommitClaims. Drain commits within
	// milliseconds, but DrainClaim callers (the mail-check inject hook) hold a
	// claim for the whole hook lifetime — bounded by Claude Code's hook timeout
	// (~30s). The default threshold (5m) clears that by ~10x. Lowering
	// operational.nudge.stale_claim_threshold below the max drain lifetime would
	// let this sweep requeue a still-in-flight claim, delivering the same nudge
	// twice.
	staleThreshold := nudgeConfig(townRoot).StaleClaimThresholdD()
	now := time.Now()
	for _, entry := range entries {
		if !strings.Contains(entry.Name(), ".claimed") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > staleThreshold {
			orphanPath := filepath.Join(dir, entry.Name())
			// Strip everything from ".claimed" onward to restore original .json filename
			name := entry.Name()
			claimedIdx := strings.Index(name, ".claimed")
			restoredPath := filepath.Join(dir, name[:claimedIdx])
			if err := os.Rename(orphanPath, restoredPath); err != nil {
				// Rename failed — remove as last resort to prevent infinite accumulation
				fmt.Fprintf(os.Stderr, "Warning: failed to requeue orphaned claim %s: %v\n", entry.Name(), err)
				_ = os.Remove(orphanPath)
			}
		}
	}

	// Sort by name (timestamp-based) for FIFO ordering
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	var nudges []QueuedNudge
	var claims []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		path := filepath.Join(dir, entry.Name())

		// Atomically claim the file by renaming it. If another Drain call
		// is racing us, only one rename will succeed — the loser gets
		// ENOENT and moves on. This prevents double-delivery.
		//
		// Each drainer uses a unique claim suffix to avoid destination
		// collisions. On Windows, os.Rename to a shared destination is
		// not atomic — two goroutines can both "succeed" via
		// MOVEFILE_REPLACE_EXISTING, causing data loss. Unique suffixes
		// ensure each rename has a distinct target.
		claimPath := path + ".claimed." + randomSuffix()
		if err := os.Rename(path, claimPath); err != nil {
			// Another Drain got it first, or file was already removed
			continue
		}

		data, err := os.ReadFile(claimPath)
		if err != nil {
			if os.IsNotExist(err) {
				// File vanished between rename and read — treat as lost race
				continue
			}
			// Transient read error (e.g., Windows AV/indexer holding a share
			// lock) — unclaim so the nudge can be retried on a future Drain
			// call rather than permanently lost.
			_ = os.Rename(claimPath, path) // best-effort unclaim; orphan sweep catches failures
			continue
		}

		var n QueuedNudge
		if err := json.Unmarshal(data, &n); err != nil {
			// Malformed — clean up
			if rmErr := os.Remove(claimPath); rmErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to remove malformed claim %s: %v\n", entry.Name(), rmErr)
			}
			continue
		}

		// Skip expired nudges — stale messages create noise, not value.
		if !n.ExpiresAt.IsZero() && now.After(n.ExpiresAt) {
			if rmErr := os.Remove(claimPath); rmErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to remove expired nudge %s: %v\n", entry.Name(), rmErr)
			}
			continue
		}

		// Deferred nudge: not ready yet — unclaim and leave in queue.
		if !n.DeliverAfter.IsZero() && now.Before(n.DeliverAfter) {
			if renameErr := os.Rename(claimPath, path); renameErr != nil {
				fmt.Fprintf(os.Stderr, "Warning: failed to unclaim deferred nudge %s: %v\n", entry.Name(), renameErr)
			}
			continue
		}

		nudges = append(nudges, n)

		// Defer removal to CommitClaims. If the caller dies before committing,
		// the claim survives and the orphan sweep above redelivers it.
		claims = append(claims, claimPath)
	}

	return nudges, claims, nil
}

// CommitClaims removes the claimed nudge files returned by DrainClaim. Call it
// only after the nudges have been delivered (e.g. the hook's output was
// accepted). Files that are already gone — removed by a concurrent drainer or
// restored by the orphan sweep — are ignored.
//
// Removal is retried once on a transient error: a claim that survives an
// already-accepted delivery is worse than a delay, because the orphan sweep will
// requeue it and the agent then sees the SAME nudge a second time. A duplicate
// wakeup is low-harm (and never a lost nudge), so a persistent failure is logged
// and returned rather than treated as fatal.
func CommitClaims(claimPaths []string) error {
	var firstErr error
	for _, claimPath := range claimPaths {
		if err := removeClaim(claimPath); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: failed to remove committed nudge claim %s: %v\n", claimPath, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// removeClaim deletes a committed claim file. A missing file counts as success.
// It retries once on a transient error (e.g. a Windows AV/indexer holding a
// share lock) to avoid leaving an already-delivered claim for the orphan sweep
// to redeliver as a duplicate.
func removeClaim(claimPath string) error {
	err := os.Remove(claimPath)
	if err == nil || os.IsNotExist(err) {
		return nil
	}
	time.Sleep(10 * time.Millisecond)
	if retryErr := os.Remove(claimPath); retryErr == nil || os.IsNotExist(retryErr) {
		return nil
	}
	return err
}

// Pending returns the count of queued nudges for a session without draining.
// This is an approximate count — it does not check expiry or read file contents.
func Pending(townRoot, session string) (int, error) {
	dir := queueDir(townRoot, session)

	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading nudge queue: %w", err)
	}

	count := 0
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			count++
		}
	}

	return count, nil
}

// QueueLen returns the number of pending nudges for a session without draining.
// Returns 0 on error — callers use this for quick checks. Missing queue
// directories are expected (no nudges yet) and silenced; other filesystem
// errors are logged to stderr so they don't go unnoticed.
func QueueLen(townRoot, session string) int {
	n, err := Pending(townRoot, session)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: nudge queue check failed for %s: %v\n", session, err)
	}
	return n
}

// RemoveKindByThread deletes queued nudges for a session that match both the
// provided kind and thread ID. It only removes queued .json files, leaving any
// in-flight claimed files alone so concurrent drainers can finish safely.
func RemoveKindByThread(townRoot, session, kind, threadID string) (int, error) {
	if kind == "" || threadID == "" {
		return 0, nil
	}

	dir := queueDir(townRoot, session)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, fmt.Errorf("reading nudge queue: %w", err)
	}

	removed := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}

		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return removed, fmt.Errorf("reading queued nudge %s: %w", entry.Name(), err)
		}

		var n QueuedNudge
		if err := json.Unmarshal(data, &n); err != nil {
			continue
		}
		if n.Kind != kind || n.ThreadID != threadID {
			continue
		}

		if err := os.Remove(path); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return removed, fmt.Errorf("removing queued nudge %s: %w", entry.Name(), err)
		}
		removed++
	}

	return removed, nil
}

// FormatForInjection formats queued nudges as a system-reminder block
// suitable for Claude Code hook output.
func FormatForInjection(nudges []QueuedNudge) string {
	if len(nudges) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("<system-reminder>\n")

	// Separate urgent from normal
	var urgent, normal []QueuedNudge
	for _, n := range nudges {
		if n.Priority == PriorityUrgent {
			urgent = append(urgent, n)
		} else {
			normal = append(normal, n)
		}
	}

	if len(urgent) > 0 {
		b.WriteString(fmt.Sprintf("QUEUED NUDGE (%d urgent):\n\n", len(urgent)))
		for _, n := range urgent {
			b.WriteString(fmt.Sprintf("  [URGENT from %s] %s\n", n.Sender, n.Message))
		}
		if len(normal) > 0 {
			b.WriteString(fmt.Sprintf("\nPlus %d non-urgent nudge(s):\n", len(normal)))
			for _, n := range normal {
				b.WriteString(fmt.Sprintf("  [from %s] %s\n", n.Sender, n.Message))
			}
		}
		b.WriteString("\nHandle urgent nudges before continuing current work.\n")
	} else {
		b.WriteString(fmt.Sprintf("QUEUED NUDGE (%d message(s)):\n\n", len(normal)))
		for _, n := range normal {
			b.WriteString(fmt.Sprintf("  [from %s] %s\n", n.Sender, n.Message))
		}
		b.WriteString("\nThis is a background notification. Continue current work unless the nudge is higher priority.\n")
	}

	b.WriteString("</system-reminder>\n")
	return b.String()
}
