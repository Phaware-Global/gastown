package polecat

import (
	"errors"
	"fmt"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/rig"
)

const (
	WorkVerdictSafeToNuke    = "SAFE_TO_NUKE"
	WorkVerdictNeedsRecovery = "NEEDS_RECOVERY"
	WorkVerdictNeedsMQSubmit = "NEEDS_MQ_SUBMIT"
)

const (
	MQStatusSubmitted    = "submitted"
	MQStatusNotSubmitted = "not_submitted"
	MQStatusNotRequired  = "not_required"
	MQStatusUnknown      = "unknown"
)

// PolecatWorkState is the shared lifecycle/recovery verdict for a polecat.
// It intentionally separates cleanup safety, reuse safety, and slot-open
// eligibility so callers do not infer them independently.
type PolecatWorkState struct {
	Rig           string        `json:"rig"`
	Polecat       string        `json:"polecat"`
	State         State         `json:"state"`
	Branch        string        `json:"branch,omitempty"`
	Issue         string        `json:"issue,omitempty"`
	CleanupStatus CleanupStatus `json:"cleanup_status"`
	ActiveMR      string        `json:"active_mr,omitempty"`
	MQStatus      string        `json:"mq_status,omitempty"`

	NeedsRecovery        bool `json:"needs_recovery"`
	NeedsMQSubmit        bool `json:"needs_mq_submit"`
	SafeToNuke           bool `json:"safe_to_nuke"`
	Reusable             bool `json:"reusable"`
	SlotOpenEligible     bool `json:"slot_open_eligible"`
	CountsTowardCapacity bool `json:"counts_toward_capacity"`

	Verdict string   `json:"verdict"`
	Reason  string   `json:"reason,omitempty"`
	Reasons []string `json:"reasons,omitempty"`

	activeMRSubmitted bool
}

func (s *PolecatWorkState) addReason(reason string) {
	if reason == "" {
		return
	}
	s.Reasons = append(s.Reasons, reason)
	if s.Reason == "" {
		s.Reason = reason
	}
}

func (s *PolecatWorkState) setBlockReason(reason string) {
	if reason == "" {
		return
	}
	s.Reason = reason
	s.Reasons = append(s.Reasons, reason)
}

// EvaluateWorkState returns the shared polecat lifecycle/recovery verdict.
func (m *Manager) EvaluateWorkState(name string) (*PolecatWorkState, error) {
	p, err := m.Get(name)
	if err != nil {
		return nil, err
	}
	return m.evaluateWorkStateForPolecat(name, p)
}

// EvaluateCompletedSlotState evaluates a just-completed polecat for slot-open
// notifications. gt done notifies witness before the session necessarily exits,
// so a live session without current work must not by itself block SLOT_OPEN.
func (m *Manager) EvaluateCompletedSlotState(name string) (*PolecatWorkState, error) {
	p, err := m.Get(name)
	if err != nil {
		return nil, err
	}
	if p.Issue == "" && p.State == StateWorking {
		copy := *p
		copy.State = StateIdle
		p = &copy
	}
	return m.evaluateWorkStateForPolecat(name, p)
}

func (m *Manager) evaluateWorkStateForPolecat(name string, p *Polecat) (*PolecatWorkState, error) {
	if p == nil {
		return nil, fmt.Errorf("polecat %s not found", name)
	}

	state := &PolecatWorkState{
		Rig:     m.rig.Name,
		Polecat: name,
		State:   p.State,
		Branch:  p.Branch,
		Issue:   p.Issue,
		Verdict: WorkVerdictNeedsRecovery,
	}

	input := m.slotReuseInputForPolecat(name, p.State, state)
	decision := DecideSlotReuse(input)
	state.Reusable = decision.Reusable
	state.SlotOpenEligible = decision.Reusable
	if decision.Reason != "reusable" {
		state.addReason(decision.Reason)
	}

	m.populateRecoveryVerdict(name, p, state, input)

	// Reuse/slot-open are distinct from destructive cleanup. A clean polecat with
	// an open, submitted MR can be reused because its branch is in the merge
	// pipeline, even though it is not necessarily safe to nuke.
	if decision.Reusable && state.ActiveMR != "" && state.activeMRSubmitted {
		state.Reusable = true
		state.SlotOpenEligible = true
	} else if state.Verdict != WorkVerdictSafeToNuke {
		state.Reusable = false
		state.SlotOpenEligible = false
		if state.Reason == "" || state.Reason == "reusable" {
			state.setBlockReason(state.Verdict)
		} else {
			state.addReason(state.Verdict)
		}
	}

	state.NeedsMQSubmit = state.Verdict == WorkVerdictNeedsMQSubmit
	state.NeedsRecovery = state.Verdict != WorkVerdictSafeToNuke
	state.SafeToNuke = state.Verdict == WorkVerdictSafeToNuke
	state.CountsTowardCapacity = !state.Reusable && p.State != StateDone

	return state, nil
}

func (m *Manager) slotReuseInputForPolecat(name string, state State, ws *PolecatWorkState) SlotReuseInput {
	input := SlotReuseInput{State: state, CleanupStatus: CleanupUnknown}
	agentID := m.agentBeadID(name)
	_, fields, err := m.agentBeads().GetAgentBead(agentID)
	if err != nil {
		if !errors.Is(err, beads.ErrNotFound) {
			input.GitCheckFailed = true
			if ws != nil {
				ws.addReason("agent-bead-lookup-failed")
			}
		}
	}
	if err == nil && fields != nil {
		if fields.HookBead != "" && !m.isAssignedBeadTerminal(fields.HookBead) {
			input.HookBead = fields.HookBead
		}
		input.PushFailed = fields.PushFailed
		input.MRFailed = fields.MRFailed
		if fields.CleanupStatus != "" {
			input.CleanupStatus = CleanupStatus(fields.CleanupStatus)
		}
		if ws != nil {
			ws.ActiveMR = fields.ActiveMR
		}
	}

	clonePath := m.clonePath(name)
	g := git.NewGit(clonePath)
	branch, branchErr := g.CurrentBranch()
	if branchErr != nil {
		input.GitCheckFailed = true
		if ws != nil {
			ws.addReason("git-branch-failed")
		}
	} else {
		input.Branch = branch
		if ws != nil && ws.Branch == "" {
			ws.Branch = branch
		}
	}
	if status, err := g.CheckUncommittedWork(); err == nil {
		input.GitDirty = !status.CleanExcludingRuntime()
		input.StashCount = status.StashCount
		input.UnpushedCommits = status.UnpushedCommits
	} else {
		input.GitCheckFailed = true
		if ws != nil {
			ws.addReason("git-status-failed")
		}
	}
	if branch != "" {
		if pushed, unpushed, err := g.BranchPushedToRemote(branch, "origin"); err == nil {
			if !pushed && unpushed > input.UnpushedCommits {
				input.UnpushedCommits = unpushed
			}
		} else {
			input.GitCheckFailed = true
			if ws != nil {
				ws.addReason("git-remote-check-failed")
			}
		}
	}
	if input.CleanupStatus == CleanupUnknown && !input.GitCheckFailed && !input.GitDirty && input.StashCount == 0 && input.UnpushedCommits == 0 {
		input.CleanupStatus = CleanupClean
	}
	if ws != nil {
		ws.CleanupStatus = input.CleanupStatus
	}
	return input
}

func (m *Manager) populateRecoveryVerdict(name string, p *Polecat, ws *PolecatWorkState, input SlotReuseInput) {
	bd := m.beads
	agentID := m.agentBeadID(name)
	_, fields, err := m.agentBeads().GetAgentBead(agentID)

	if err != nil || fields == nil {
		m.populateGitFallbackVerdict(ws, input)
	} else {
		cleanupStatus := CleanupStatus(fields.CleanupStatus)
		ws.ActiveMR = fields.ActiveMR
		if cleanupStatus == "" || cleanupStatus == CleanupUnknown {
			m.populateGitFallbackVerdict(ws, input)
			terminal, submitted := m.activeMRState(fields.ActiveMR)
			ws.activeMRSubmitted = submitted
			if ws.Verdict == WorkVerdictSafeToNuke && submitted {
				ws.NeedsRecovery = false
				ws.Verdict = WorkVerdictNeedsRecovery
			} else if ws.Verdict == WorkVerdictSafeToNuke && !terminal {
				ws.NeedsRecovery = true
				ws.Verdict = WorkVerdictNeedsRecovery
			}
		} else if !input.GitCheckFailed && !input.GitDirty && input.StashCount == 0 && input.UnpushedCommits == 0 && m.isActiveMRTerminal(fields.ActiveMR) {
			ws.CleanupStatus = cleanupStatus
			if !cleanupStatus.IsSafe() {
				ws.CleanupStatus = CleanupClean
			}
			ws.NeedsRecovery = false
			ws.Verdict = WorkVerdictSafeToNuke
		} else if cleanupStatus.IsSafe() && !input.GitCheckFailed && !input.GitDirty && input.StashCount == 0 && input.UnpushedCommits == 0 {
			terminal, submitted := m.activeMRState(fields.ActiveMR)
			ws.activeMRSubmitted = submitted
			ws.CleanupStatus = cleanupStatus
			if terminal {
				ws.NeedsRecovery = false
				ws.Verdict = WorkVerdictSafeToNuke
			} else if submitted {
				ws.NeedsRecovery = false
				ws.Verdict = WorkVerdictNeedsRecovery
				ws.addReason("active-mr-submitted")
			} else {
				ws.NeedsRecovery = true
				ws.Verdict = WorkVerdictNeedsRecovery
			}
		} else {
			ws.CleanupStatus = cleanupStatus
			ws.NeedsRecovery = true
			ws.Verdict = WorkVerdictNeedsRecovery
		}
	}

	if ws.Verdict == WorkVerdictSafeToNuke && ws.Branch != "" {
		beadTerminal := m.isAssignedBeadTerminal(ws.Issue)
		hasSubmittableWork := m.hasSubmittableWork(name, input)
		m.applyMQWorkState(ws, bd, beadTerminal, hasSubmittableWork)
	}
	if ws.Verdict != WorkVerdictSafeToNuke {
		ws.NeedsRecovery = true
	}
	if p.State != StateIdle && p.State != StateDone {
		ws.CountsTowardCapacity = true
	}
}

func (m *Manager) populateGitFallbackVerdict(ws *PolecatWorkState, input SlotReuseInput) {
	ws.CleanupStatus = input.CleanupStatus
	switch {
	case input.GitCheckFailed:
		ws.CleanupStatus = CleanupUnknown
		ws.NeedsRecovery = true
		ws.Verdict = WorkVerdictNeedsRecovery
	case !input.GitDirty && input.UnpushedCommits == 0 && input.StashCount == 0:
		ws.CleanupStatus = CleanupClean
		ws.NeedsRecovery = false
		ws.Verdict = WorkVerdictSafeToNuke
	case input.UnpushedCommits > 0:
		ws.CleanupStatus = CleanupUnpushed
		ws.NeedsRecovery = true
		ws.Verdict = WorkVerdictNeedsRecovery
	case input.StashCount > 0:
		ws.CleanupStatus = CleanupStash
		ws.NeedsRecovery = true
		ws.Verdict = WorkVerdictNeedsRecovery
	default:
		ws.CleanupStatus = CleanupUncommitted
		ws.NeedsRecovery = true
		ws.Verdict = WorkVerdictNeedsRecovery
	}
}

func (m *Manager) hasSubmittableWork(name string, input SlotReuseInput) bool {
	if input.GitCheckFailed || input.UnpushedCommits > 0 {
		return true
	}
	if input.Branch == "" {
		return false
	}
	defaultBranch := "main"
	if cfg, err := rig.LoadRigConfig(m.rig.Path); err == nil && cfg.DefaultBranch != "" {
		defaultBranch = cfg.DefaultBranch
	}
	hasDiff, err := git.NewGit(m.clonePath(name)).HasDiff("origin/"+defaultBranch, "HEAD")
	if err != nil {
		return true
	}
	return hasDiff
}

func (m *Manager) isActiveMRTerminal(mrID string) bool {
	terminal, _ := m.activeMRState(mrID)
	return terminal
}

func (m *Manager) activeMRState(mrID string) (terminal bool, submitted bool) {
	if mrID == "" {
		return true, false
	}
	mr, err := m.beads.Show(mrID)
	if errors.Is(err, beads.ErrNotFound) {
		return true, false
	}
	if err != nil || mr == nil {
		return false, false
	}
	if beads.IssueStatus(mr.Status).IsTerminal() {
		return true, false
	}
	return false, true
}

func (m *Manager) isAssignedBeadTerminal(issueID string) bool {
	if issueID == "" {
		return false
	}
	issue, err := m.beads.Show(issueID)
	if err != nil || issue == nil {
		return false
	}
	return beads.IssueStatus(issue.Status).IsTerminal()
}

type workstateMRFinder interface {
	FindMRForBranchAny(branch string) (*beads.Issue, error)
}

func (m *Manager) applyMQWorkState(ws *PolecatWorkState, bd workstateMRFinder, beadTerminal, hasSubmittableWork bool) {
	if beadTerminal {
		ws.MQStatus = MQStatusSubmitted
		return
	}
	if !hasSubmittableWork {
		ws.MQStatus = MQStatusNotRequired
		return
	}
	mr, err := bd.FindMRForBranchAny(ws.Branch)
	if err != nil {
		ws.MQStatus = MQStatusUnknown
		ws.NeedsRecovery = true
		ws.SafeToNuke = false
		ws.Verdict = WorkVerdictNeedsRecovery
		ws.addReason("mq-lookup-failed")
		return
	}
	if mr != nil {
		ws.MQStatus = MQStatusSubmitted
		return
	}
	ws.MQStatus = MQStatusNotSubmitted
	ws.NeedsRecovery = true
	ws.NeedsMQSubmit = true
	ws.SafeToNuke = false
	ws.Verdict = WorkVerdictNeedsMQSubmit
	ws.addReason(WorkVerdictNeedsMQSubmit)
}
