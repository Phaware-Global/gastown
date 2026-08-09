package cmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/style"
)

const memoryKeyPrefix = "memory."

// validMemoryTypes are the recognized memory type categories.
// Typed memories are stored as memory.<type>.<key> in the kv store.
// Legacy untyped memories (memory.<key>) are treated as "general".
var validMemoryTypes = map[string]string{
	"feedback":  "Guidance or corrections from users — behavioral rules for future work",
	"project":   "Ongoing work context, goals, deadlines, decisions",
	"user":      "Info about the user's role, preferences, expertise",
	"reference": "Pointers to external resources (URLs, tools, dashboards)",
	"general":   "Uncategorized memories (default)",
}

// memoryTypeOrder defines the injection priority during gt prime.
// Feedback first (behavioral corrections), then user context, then the rest.
var memoryTypeOrder = []string{"feedback", "user", "project", "reference", "general"}

var rememberKey string
var rememberType string

func init() {
	rememberCmd.Flags().StringVar(&rememberKey, "key", "", "Explicit key slug (default: auto-generated from content)")
	rememberCmd.Flags().StringVar(&rememberType, "type", "", "Memory type: feedback, project, user, reference (default: general)")
	rememberCmd.GroupID = GroupWork
	rootCmd.AddCommand(rememberCmd)
}

var rememberCmd = &cobra.Command{
	Use:   `remember "insight"`,
	Short: "Store a persistent memory",
	Long: `Store a persistent memory in the beads key-value store.

Memories persist across sessions and are injected during gt prime.
This replaces filesystem-based MEMORY.md with bead-backed storage.

The key is auto-generated from the content if not specified.
Use --key to provide an explicit slug for easy retrieval.

Memory types help organize memories and prioritize injection:
  feedback   Guidance or corrections from users
  project    Ongoing work context, goals, deadlines
  user       Info about the user's role and preferences
  reference  Pointers to external resources

Examples:
  gt remember "Refinery uses worktree, cannot checkout main"
  gt remember --type feedback "Don't mock the database in integration tests"
  gt remember --type user --key senior-go-dev "User has 10 years Go experience"
  gt remember --key refinery-worktree "Refinery uses worktree, cannot checkout main"`,
	Args: cobra.ExactArgs(1),
	RunE: runRemember,
}

func runRemember(cmd *cobra.Command, args []string) error {
	content := args[0]
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("memory content cannot be empty")
	}

	// Validate --type if provided
	memType := strings.ToLower(strings.TrimSpace(rememberType))
	if memType != "" {
		if _, ok := validMemoryTypes[memType]; !ok {
			return fmt.Errorf("invalid memory type %q — valid types: feedback, project, user, reference", memType)
		}
	}
	if memType == "" {
		memType = "general"
	}

	key := rememberKey
	if key == "" {
		key = autoKey(content)
	}

	// Sanitize key: lowercase, hyphens instead of spaces, strip dots
	key = sanitizeKey(key)

	fullKey := memoryKeyPrefix + memType + "." + key

	// Check if key already exists
	existing, _ := bdKvGet(fullKey)
	verb := "Stored"
	if existing != "" {
		verb = "Updated"
	}

	if err := bdKvSet(fullKey, content); err != nil {
		return fmt.Errorf("storing memory: %w", err)
	}

	displayKey := key
	if memType != "general" {
		displayKey = memType + "/" + key
	}
	fmt.Printf("%s %s memory: %s\n", style.Success.Render("✓"), verb, style.Bold.Render(displayKey))
	return nil
}

// parseMemoryKey extracts the type and short key from a full kv key.
// Handles both typed keys (memory.<type>.<key>) and legacy keys (memory.<key>).
func parseMemoryKey(kvKey string) (memType, shortKey string) {
	rest := strings.TrimPrefix(kvKey, memoryKeyPrefix)
	if rest == "" {
		return "general", ""
	}

	// Check if first segment is a known type
	if dotIdx := strings.Index(rest, "."); dotIdx > 0 {
		candidate := rest[:dotIdx]
		if _, ok := validMemoryTypes[candidate]; ok {
			return candidate, rest[dotIdx+1:]
		}
	}

	// Legacy untyped memory
	return "general", rest
}

// autoKey generates a short key from content using first few meaningful words.
func autoKey(content string) string {
	// Take first ~5 words, lowercase, hyphenate
	words := strings.Fields(strings.ToLower(content))
	if len(words) > 5 {
		words = words[:5]
	}

	// Strip non-alphanumeric chars from each word
	var clean []string
	for _, w := range words {
		w = strings.Map(func(r rune) rune {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				return r
			}
			return -1
		}, w)
		if w != "" {
			clean = append(clean, w)
		}
	}

	if len(clean) == 0 {
		// Fallback to hash
		h := sha256.Sum256([]byte(content))
		return hex.EncodeToString(h[:4])
	}

	slug := strings.Join(clean, "-")
	// Cap length
	if len(slug) > 40 {
		slug = slug[:40]
	}
	return slug
}

// sanitizeKey normalizes a key slug.
func sanitizeKey(key string) string {
	key = strings.ToLower(key)
	key = strings.ReplaceAll(key, " ", "-")
	key = strings.ReplaceAll(key, ".", "-")

	// Strip anything that isn't alphanumeric or hyphen
	key = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			return r
		}
		return -1
	}, key)

	// Collapse multiple hyphens
	for strings.Contains(key, "--") {
		key = strings.ReplaceAll(key, "--", "-")
	}
	key = strings.Trim(key, "-")

	return key
}

// Memory writes go through `bd remember` / `bd forget`, not `bd kv set|clear`.
//
// bd reserves the kv namespace this package stores memories in and rejects
// writes to it outright:
//
//	invalid key: key cannot start with "memory." (reserved for persistent
//	memories; use 'bd remember' / 'bd forget')
//
// which broke every memory write — `gt remember`, `gt forget`, and the apply
// step of `gt memories --compact`, the last of which failed only AFTER the
// operator confirmed a plan.
//
// The two sides use different key forms: `bd remember --key`/`bd forget` take
// the key WITHOUT the memory. prefix and add it themselves, while `bd kv list`
// (still the read path, and still permitted) returns it WITH the prefix. So the
// prefix is stripped on the way in and kept on the way out.
//
// bdMemoryKey converts a stored kv key (memory.<type>.<slug>) to the bare key
// bd's memory commands expect.
func bdMemoryKey(kvKey string) string {
	return strings.TrimPrefix(kvKey, memoryKeyPrefix)
}

// bdMemoryResult is the --json envelope shared by `bd remember` and
// `bd forget`.
type bdMemoryResult struct {
	Action  string     `json:"action"`  // remember: "remembered" | "updated" | "recalled"
	Deleted bdJSONBool `json:"deleted"` // forget, on a hit
	Found   bdJSONBool `json:"found"`   // forget, on a miss: false
	Key     string     `json:"key"`
}

// bdJSONBool decodes a field bd currently emits as the STRING "true"/"false"
// but is free to emit as a real JSON boolean.
//
// Declaring these as Go strings made the verification guards fail open: a real
// boolean is a type mismatch, json.Unmarshal fails for the whole struct, and
// the "undecodable means an older bd" fallback then reported the write as
// successful. The guards must not evaporate the day bd fixes its envelope.
type bdJSONBool struct {
	Set   bool
	Value bool
}

func (b *bdJSONBool) UnmarshalJSON(data []byte) error {
	var asBool bool
	if err := json.Unmarshal(data, &asBool); err == nil {
		b.Set, b.Value = true, asBool
		return nil
	}
	var asString string
	if err := json.Unmarshal(data, &asString); err == nil {
		switch strings.ToLower(strings.TrimSpace(asString)) {
		case "true":
			b.Set, b.Value = true, true
			return nil
		case "false":
			b.Set, b.Value = true, false
			return nil
		}
	}
	// Unknown shape: leave Set false so callers treat it as "not reported"
	// rather than silently as false.
	return nil
}

// bdRememberArgs builds the argv for storing one memory.
//
// "--" terminates flag parsing: a value beginning with a dash is otherwise
// consumed as an unknown flag and the write fails ("unknown flag: -x ...").
func bdRememberArgs(key, value string) []string {
	return []string{"remember", "--key", bdMemoryKey(key), "--json", "--", value}
}

// bdForgetArgs builds the argv for removing one memory. The key follows "--"
// for the same reason values do: a stored key CAN begin with a dash (nothing
// sanitizes keys on the read path), and `bd forget -x` fails as a flag.
func bdForgetArgs(key string) []string {
	return []string{"forget", "--json", "--", bdMemoryKey(key)}
}

// bdRememberActions are the envelope actions that mean bd actually WROTE.
//
// "recalled" is deliberately absent: bd has a branch that reads instead of
// writing and still reports an action, which would make a store that never
// happened look successful.
var bdRememberActions = map[string]bool{"remembered": true, "updated": true}

// interpretRememberResult reports whether `bd remember` actually stored.
func interpretRememberResult(key string, out []byte) error {
	var res bdMemoryResult
	if err := json.Unmarshal(out, &res); err != nil {
		// Not JSON at all — a bd without --json on this command still signals
		// success by exit status, and the caller only gets here on a zero exit.
		return nil
	}
	if !bdRememberActions[res.Action] {
		return fmt.Errorf("bd remember did not store %s (action %q): %s",
			key, res.Action, bdOutputExcerpt(out))
	}
	return nil
}

// interpretForgetResult reports whether `bd forget` actually removed anything.
//
// A delete that removed nothing must be an error: callers delete keys they just
// read from the store, so a no-op means the key form is wrong or the store
// moved underneath, and reporting a removal that did not happen would let
// applyCompactPlan claim memories were dropped while they remain.
//
// bd 1.1.0 signals the miss by exiting 1 with {"found":false} on stdout, so
// bdKvClear runs this over the payload REGARDLESS of exit status — checking it
// only on the success path left the guard unreachable on the very case it was
// written for, and the error said only "exit status 1".
func interpretForgetResult(key string, out []byte) error {
	var res bdMemoryResult
	if err := json.Unmarshal(out, &res); err != nil {
		return nil
	}
	if res.Found.Set && !res.Found.Value {
		return fmt.Errorf("bd forget found no memory %s (bd key %q)", key, bdMemoryKey(key))
	}
	return nil
}

// bdKvSet stores a memory value under a memory.* kv key.
func bdKvSet(key, value string) error {
	out, err := runBdMemoryCmd(bdRememberArgs(key, value))
	if err != nil {
		return bdMemoryError(err, out)
	}
	return interpretRememberResult(key, out)
}

// bdKvGet calls bd kv get <key> and returns the value. Reads are unaffected by
// the reservation above — only writes are refused.
func bdKvGet(key string) (string, error) {
	cmd := exec.Command("bd", "kv", "get", key)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// bdKvClear removes a memory by its memory.* kv key.
func bdKvClear(key string) error {
	out, runErr := runBdMemoryCmd(bdForgetArgs(key))
	// Read the payload first: bd reports a miss by exiting 1 with
	// {"found":false} on stdout, so the precise message lives there, not in the
	// exit status.
	if err := interpretForgetResult(key, out); err != nil {
		return err
	}
	if runErr != nil {
		return bdMemoryError(runErr, out)
	}
	return nil
}

// runBdMemoryCmd runs a bd memory subcommand, capturing stdout for the JSON
// envelope while letting stderr through to the terminal.
//
// bd writes operator-facing hygiene warnings to stderr (".beads has permissions
// 0755 (recommended: 0700)"). Capturing it — which cmd.Output() does — meant
// those were surfaced only when the command FAILED, so on the normal path they
// vanished; before this feature moved off `bd kv set` they always reached the
// operator.
func runBdMemoryCmd(args []string) ([]byte, error) {
	var stdout bytes.Buffer
	cmd := exec.Command("bd", args...)
	cmd.Stdout = &stdout
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	return stdout.Bytes(), err
}

// bdOutputExcerpt bounds bd output before it reaches an error message. bd
// quotes the offending value back on failure, so an oversized memory produced a
// ~70 KB error that `gt remember` printed to the terminal in full.
func bdOutputExcerpt(out []byte) string {
	return compactTruncate(strings.TrimSpace(string(out)), bdOutputExcerptRunes)
}

// bdOutputExcerptRunes bounds a quoted bd payload in an error message.
const bdOutputExcerptRunes = 500

// bdMemoryError annotates a failed bd memory command with whatever it printed.
func bdMemoryError(err error, stdout []byte) error {
	if s := bdOutputExcerpt(stdout); s != "" {
		return fmt.Errorf("%w: %s", err, s)
	}
	return err
}

// parseBdKvListJSON parses bd kv list --json output, keeping only string values.
//
// bd >=1.0.3 injects a top-level "schema_version": N (number) sibling field
// into every object-shaped --json output ({"schema_version": 1, "key1": ...}).
// Decoding into RawMessage and keeping only entries that parse as strings
// silently drops the version sibling and any future non-string envelope fields.
func parseBdKvListJSON(data []byte) (map[string]string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing kv list: %w", err)
	}

	kvs := make(map[string]string, len(raw))
	for k, v := range raw {
		var s *string
		if err := json.Unmarshal(v, &s); err != nil || s == nil {
			continue
		}
		kvs[k] = *s
	}
	return kvs, nil
}

// bdKvListJSON calls bd kv list --json and returns the parsed string values.
func bdKvListJSON() (map[string]string, error) {
	// Bound the shell-out so callers on latency-sensitive paths (the
	// compact/resume prime hook in particular, which must stay within tight
	// non-Claude runtime hook timeouts) can't stall indefinitely if bd hangs.
	ctx, cancel := context.WithTimeout(context.Background(), bdKvListTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bd", "kv", "list", "--json")
	out, err := cmd.Output()
	if err != nil {
		return nil, err
	}

	return parseBdKvListJSON(out)
}

// bdKvListTimeout bounds the `bd kv list` shell-out used for memory injection.
var bdKvListTimeout = 5 * time.Second
