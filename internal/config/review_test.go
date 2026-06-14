package config

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestReviewConfig_GetPerspectives_DefaultsWhenEmpty(t *testing.T) {
	// nil receiver
	var r *ReviewConfig
	if got := r.GetPerspectives(); !reflect.DeepEqual(got, DefaultPerspectives()) {
		t.Errorf("nil.GetPerspectives() = %v, want %v", got, DefaultPerspectives())
	}
	// empty list
	r = &ReviewConfig{}
	if got := r.GetPerspectives(); !reflect.DeepEqual(got, DefaultPerspectives()) {
		t.Errorf("empty.GetPerspectives() = %v, want %v", got, DefaultPerspectives())
	}
	// explicit list is preserved verbatim
	r = &ReviewConfig{Perspectives: []string{"security", "go-idioms"}}
	if got := r.GetPerspectives(); !reflect.DeepEqual(got, []string{"security", "go-idioms"}) {
		t.Errorf("GetPerspectives() = %v, want [security go-idioms]", got)
	}
}

func TestReviewConfig_GetMaxFindingsPerPerspective(t *testing.T) {
	var r *ReviewConfig
	if got := r.GetMaxFindingsPerPerspective(); got != DefaultMaxFindingsPerPerspective {
		t.Errorf("nil = %d, want %d", got, DefaultMaxFindingsPerPerspective)
	}
	r = &ReviewConfig{MaxFindingsPerPerspective: 0}
	if got := r.GetMaxFindingsPerPerspective(); got != DefaultMaxFindingsPerPerspective {
		t.Errorf("zero = %d, want %d", got, DefaultMaxFindingsPerPerspective)
	}
	r = &ReviewConfig{MaxFindingsPerPerspective: 3}
	if got := r.GetMaxFindingsPerPerspective(); got != 3 {
		t.Errorf("explicit = %d, want 3", got)
	}
}

// TestReviewIterations covers the per-rig "number of review iterations" knob
// resolution order: review.max_rounds > merge_queue.pr_review_loop_max > default.
func TestReviewIterations(t *testing.T) {
	tests := []struct {
		name string
		s    *RigSettings
		want int
	}{
		{"nil settings", nil, DefaultPRReviewLoopMax},
		{"empty settings", &RigSettings{}, DefaultPRReviewLoopMax},
		{
			"review.max_rounds wins",
			&RigSettings{
				Review:     &ReviewConfig{MaxRounds: 7},
				MergeQueue: &MergeQueueConfig{PRReviewLoopMax: 2},
			},
			7,
		},
		{
			"falls back to pr_review_loop_max",
			&RigSettings{MergeQueue: &MergeQueueConfig{PRReviewLoopMax: 5}},
			5,
		},
		{
			"review present but max_rounds zero falls back to loop max",
			&RigSettings{
				Review:     &ReviewConfig{Perspectives: []string{"adversarial"}},
				MergeQueue: &MergeQueueConfig{PRReviewLoopMax: 4},
			},
			4,
		},
		{
			"both zero -> default",
			&RigSettings{Review: &ReviewConfig{}, MergeQueue: &MergeQueueConfig{}},
			DefaultPRReviewLoopMax,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.s.ReviewIterations(); got != tt.want {
				t.Errorf("ReviewIterations() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestGetReviewerTokenEnv(t *testing.T) {
	c := &MergeQueueConfig{}
	if got := c.GetReviewerTokenEnv(); got != DefaultReviewerTokenEnv {
		t.Errorf("default = %q, want %q", got, DefaultReviewerTokenEnv)
	}
	c = &MergeQueueConfig{ReviewerTokenEnv: "MY_TOKEN"}
	if got := c.GetReviewerTokenEnv(); got != "MY_TOKEN" {
		t.Errorf("explicit = %q, want MY_TOKEN", got)
	}
	var nilc *MergeQueueConfig
	if got := nilc.GetReviewerTokenEnv(); got != DefaultReviewerTokenEnv {
		t.Errorf("nil = %q, want %q", got, DefaultReviewerTokenEnv)
	}
}

func TestValidateRigSettings_ReviewerLocalRequiresReviewer(t *testing.T) {
	s := &RigSettings{
		Type: "rig-settings",
		MergeQueue: &MergeQueueConfig{
			MergeStrategy: MergeStrategyPR,
			ReviewerLocal: true,
			// PRReviewer intentionally empty; PRApprover set so only the
			// reviewer_local rule trips.
			PRApprover: "human",
		},
	}
	if err := validateRigSettings(s); err == nil {
		t.Fatal("expected error when reviewer_local=true with empty pr_reviewer")
	}

	s.MergeQueue.PRReviewer = "reviewer-bot"
	if err := validateRigSettings(s); err != nil {
		t.Fatalf("unexpected error with pr_reviewer set: %v", err)
	}
}

func TestValidateReviewConfig(t *testing.T) {
	if err := validateReviewConfig(&ReviewConfig{MaxRounds: -1}); err == nil {
		t.Error("expected error for negative max_rounds")
	}
	if err := validateReviewConfig(&ReviewConfig{MaxFindingsPerPerspective: -1}); err == nil {
		t.Error("expected error for negative max_findings_per_perspective")
	}
	if err := validateReviewConfig(&ReviewConfig{Perspectives: []string{"adversarial", "  "}}); err == nil {
		t.Error("expected error for blank perspective name")
	}
	if err := validateReviewConfig(&ReviewConfig{
		Perspectives:              []string{"adversarial", "security"},
		MaxFindingsPerPerspective: 8,
		MaxRounds:                 4,
	}); err != nil {
		t.Errorf("unexpected error for valid config: %v", err)
	}
}

// TestRigSettings_ReviewRoundTrip ensures the new review block and reviewer
// fields survive a JSON marshal/unmarshal cycle.
func TestRigSettings_ReviewRoundTrip(t *testing.T) {
	in := &RigSettings{
		Type:    "rig-settings",
		Version: CurrentRigSettingsVersion,
		MergeQueue: &MergeQueueConfig{
			MergeStrategy:    MergeStrategyPR,
			PRReviewer:       "reviewer-bot",
			PRApprover:       "human",
			ReviewerLocal:    true,
			ReviewerTokenEnv: "RIG_REVIEWER_TOKEN",
		},
		Review: &ReviewConfig{
			Perspectives:              []string{"adversarial", "security", "go-idioms"},
			MaxFindingsPerPerspective: 5,
			MaxRounds:                 6,
		},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out RigSettings
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !out.MergeQueue.ReviewerLocal {
		t.Error("ReviewerLocal lost in round-trip")
	}
	if out.MergeQueue.ReviewerTokenEnv != "RIG_REVIEWER_TOKEN" {
		t.Errorf("ReviewerTokenEnv = %q, want RIG_REVIEWER_TOKEN", out.MergeQueue.ReviewerTokenEnv)
	}
	if out.Review == nil || out.Review.MaxRounds != 6 {
		t.Errorf("Review.MaxRounds not preserved: %+v", out.Review)
	}
	if out.ReviewIterations() != 6 {
		t.Errorf("ReviewIterations() = %d, want 6", out.ReviewIterations())
	}
}
