package cmd

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/style"
)

var memoriesTypeFilter string
var memoriesCompact bool
var memoriesDryRun bool
var memoriesYes bool
var memoriesModel string
var memoriesInstructions string
var memoriesInstructionsFile string
var memoriesCompactTimeout time.Duration

func init() {
	memoriesCmd.Flags().StringVar(&memoriesTypeFilter, "type", "", "Filter by memory type: feedback, project, user, reference, general")
	memoriesCmd.Flags().BoolVar(&memoriesCompact, "compact", false, "LLM-assisted compaction: merge overlapping and drop stale memories")
	memoriesCmd.Flags().BoolVar(&memoriesDryRun, "dry-run", false, "With --compact: show the plan without writing changes")
	memoriesCmd.Flags().BoolVar(&memoriesYes, "yes", false, "With --compact: apply the plan without the confirmation prompt")
	memoriesCmd.Flags().StringVar(&memoriesModel, "model", "sonnet", "With --compact: model used for compaction reasoning")
	memoriesCmd.Flags().StringVar(&memoriesInstructions, "instructions", "", "With --compact: extra guidance for the compaction model (e.g. \"drop everything about the retired reviewer\")")
	memoriesCmd.Flags().StringVar(&memoriesInstructionsFile, "instructions-file", "", "With --compact: read --instructions from a file")
	memoriesCmd.Flags().DurationVar(&memoriesCompactTimeout, "timeout", 0, "With --compact: model timeout (default scales with the number of memories)")
	memoriesCmd.GroupID = GroupWork
	rootCmd.AddCommand(memoriesCmd)
}

var memoriesCmd = &cobra.Command{
	Use:   "memories [search-term]",
	Short: "List or search stored memories",
	Long: `List or search memories stored in the beads key-value store.

Without arguments, lists all memories. With a search term, filters
memories whose key or value contains the term (case-insensitive).

Use --type to filter by memory category:
  feedback   Guidance or corrections from users
  project    Ongoing work context, goals, deadlines
  user       Info about the user's role and preferences
  reference  Pointers to external resources
  general    Uncategorized memories

With --compact, use --instructions (or --instructions-file) to steer the
compaction: guidance there outranks the default merge/dedup goals, so it can
retire a topic outright or rewrite how a fact is phrased.

Compaction asks the model to re-emit the whole memory set, so it takes minutes
on a large store. The default --timeout scales with the number of memories;
raise it with --timeout if a very large store still runs out of time.

Examples:
  gt memories                    # List all memories
  gt memories --type feedback    # Show only behavioral corrections
  gt memories refinery           # Search for memories about refinery
  gt memories --compact          # LLM-assisted merge/dedup (preview + confirm)
  gt memories --compact --dry-run  # Preview the compaction plan only
  gt memories --compact --instructions "Remove references to augment — we no longer use it. Replace code reviewer notes about augment with internal reviewer instructions."
  gt memories --compact --instructions-file cleanup.md --timeout 20m`,
	Args: cobra.MaximumNArgs(1),
	RunE: runMemories,
}

func runMemories(cmd *cobra.Command, args []string) error {
	if err := validateMemoriesFlags(memoriesFlagUse{
		compact:          memoriesCompact,
		hasArgs:          len(args) > 0,
		dryRun:           cmd.Flags().Changed("dry-run"),
		yes:              cmd.Flags().Changed("yes"),
		model:            cmd.Flags().Changed("model"),
		typeFilter:       cmd.Flags().Changed("type"),
		instructions:     cmd.Flags().Changed("instructions"),
		instructionsFile: cmd.Flags().Changed("instructions-file"),
		timeout:          cmd.Flags().Changed("timeout"),
		timeoutValue:     memoriesCompactTimeout,
	}); err != nil {
		return err
	}

	if memoriesCompact {
		instructions, err := resolveCompactInstructions(
			memoriesInstructions,
			cmd.Flags().Changed("instructions"),
			memoriesInstructionsFile,
		)
		if err != nil {
			return err
		}
		return runMemoriesCompact(instructions)
	}

	kvs, err := bdKvListJSON()
	if err != nil {
		return fmt.Errorf("listing memories: %w", err)
	}

	var search string
	if len(args) > 0 {
		search = strings.ToLower(args[0])
	}

	typeFilter := strings.ToLower(strings.TrimSpace(memoriesTypeFilter))
	if typeFilter != "" {
		if _, ok := validMemoryTypes[typeFilter]; !ok {
			return fmt.Errorf("invalid memory type %q — valid types: feedback, project, user, reference, general", typeFilter)
		}
	}

	// Filter for memory.* keys and optional search/type
	type memory struct {
		memType  string
		shortKey string
		value    string
	}
	var memories []memory

	for k, v := range kvs {
		if !strings.HasPrefix(k, memoryKeyPrefix) {
			continue
		}

		memType, shortKey := parseMemoryKey(k)

		if typeFilter != "" && memType != typeFilter {
			continue
		}

		if search != "" {
			if !strings.Contains(strings.ToLower(shortKey), search) &&
				!strings.Contains(strings.ToLower(v), search) &&
				!strings.Contains(strings.ToLower(memType), search) {
				continue
			}
		}

		memories = append(memories, memory{memType: memType, shortKey: shortKey, value: v})
	}

	sort.Slice(memories, func(i, j int) bool {
		if memories[i].memType != memories[j].memType {
			return memTypeRank(memories[i].memType) < memTypeRank(memories[j].memType)
		}
		return memories[i].shortKey < memories[j].shortKey
	})

	if len(memories) == 0 {
		if search != "" {
			fmt.Printf("No memories matching %q\n", search)
		} else if typeFilter != "" {
			fmt.Printf("No %s memories stored.\n", typeFilter)
		} else {
			fmt.Println("No memories stored. Use 'gt remember \"insight\"' to add one.")
		}
		return nil
	}

	header := "Memories"
	if typeFilter != "" {
		header = fmt.Sprintf("Memories [%s]", typeFilter)
	}
	if search != "" {
		header = fmt.Sprintf("%s matching %q", header, search)
	}
	fmt.Printf("%s (%d):\n\n", style.Bold.Render(header), len(memories))

	lastType := ""
	for _, m := range memories {
		if m.memType != lastType {
			if lastType != "" {
				fmt.Println()
			}
			fmt.Printf("  %s\n", style.Dim.Render("["+m.memType+"]"))
			lastType = m.memType
		}
		fmt.Printf("  %s\n", style.Bold.Render(m.shortKey))
		fmt.Printf("    %s\n\n", m.value)
	}

	return nil
}

// memoriesFlagUse records which `gt memories` flags the user actually set (as
// opposed to their default values), so validation can reject combinations that
// would silently ignore input.
type memoriesFlagUse struct {
	compact          bool
	hasArgs          bool
	dryRun           bool
	yes              bool
	model            bool
	typeFilter       bool
	instructions     bool
	instructionsFile bool
	timeout          bool
	timeoutValue     time.Duration
}

// validateMemoriesFlags rejects flag combinations that would silently ignore
// user input. The --compact-only flags are meaningless for plain listing, and a
// search term is ignored under --compact; surfacing an error beats quietly
// dropping the input.
func validateMemoriesFlags(u memoriesFlagUse) error {
	if !u.compact {
		compactOnly := []struct {
			set  bool
			name string
		}{
			{u.dryRun, "--dry-run"},
			{u.yes, "--yes"},
			{u.model, "--model"},
			{u.instructions, "--instructions"},
			{u.instructionsFile, "--instructions-file"},
			{u.timeout, "--timeout"},
		}
		var offenders []string
		for _, f := range compactOnly {
			if f.set {
				offenders = append(offenders, f.name)
			}
		}
		if len(offenders) > 0 {
			return fmt.Errorf("%s only applies with --compact", strings.Join(offenders, ", "))
		}
		return nil
	}
	// compact mode
	if u.hasArgs {
		return fmt.Errorf("--compact does not take a search term")
	}
	if u.typeFilter {
		return fmt.Errorf("--type cannot be combined with --compact (compaction always considers all memories)")
	}
	if u.instructions && u.instructionsFile {
		return fmt.Errorf("--instructions and --instructions-file are mutually exclusive")
	}
	// An explicit non-positive --timeout would otherwise fall back to the scaled
	// default, silently ignoring what the user asked for.
	if u.timeout && u.timeoutValue <= 0 {
		return fmt.Errorf("--timeout must be positive (got %s)", u.timeoutValue)
	}
	return nil
}

// memTypeRank returns the sort order for a memory type (lower = first).
func memTypeRank(memType string) int {
	for i, t := range memoryTypeOrder {
		if t == memType {
			return i
		}
	}
	return len(memoryTypeOrder)
}
