package config

import (
	"strings"
	"testing"
)

func TestNormalizePRUpdateStrategy(t *testing.T) {
	for in, want := range map[string]string{
		"":       PRUpdateStrategyRebase, // default stays as before
		"rebase": PRUpdateStrategyRebase,
		"merge":  PRUpdateStrategyMerge,
		"none":   PRUpdateStrategyNone,
	} {
		got, err := NormalizePRUpdateStrategy(in)
		if err != nil || got != want {
			t.Errorf("NormalizePRUpdateStrategy(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"force", "Merge", "rebase ", "squash"} {
		if _, err := NormalizePRUpdateStrategy(bad); err == nil {
			t.Errorf("NormalizePRUpdateStrategy(%q) accepted an invalid value", bad)
		}
	}
}

func TestGetPRUpdateStrategy(t *testing.T) {
	var nilCfg *MergeQueueConfig
	if got := nilCfg.GetPRUpdateStrategy(); got != PRUpdateStrategyRebase {
		t.Errorf("nil config = %q, want rebase", got)
	}
	if got := (&MergeQueueConfig{}).GetPRUpdateStrategy(); got != PRUpdateStrategyRebase {
		t.Errorf("unset = %q, want rebase", got)
	}
	if got := (&MergeQueueConfig{PRUpdateStrategy: "merge"}).GetPRUpdateStrategy(); got != PRUpdateStrategyMerge {
		t.Errorf("merge = %q", got)
	}
}

func TestValidateMergeQueue_PRUpdateStrategy(t *testing.T) {
	// Valid in both merge strategies: the refinery syncs behind-base branches
	// before merging either way.
	for _, ms := range []string{MergeStrategyDirect, MergeStrategyPR} {
		for _, us := range []string{"", "rebase", "merge", "none"} {
			cfg := prConfig("alice", "")
			cfg.MergeStrategy = ms
			cfg.PRUpdateStrategy = us
			if err := validateMergeQueueConfig(cfg); err != nil {
				t.Errorf("merge_strategy=%s pr_update_strategy=%q rejected: %v", ms, us, err)
			}
		}
		cfg := prConfig("alice", "")
		cfg.MergeStrategy = ms
		cfg.PRUpdateStrategy = "force"
		err := validateMergeQueueConfig(cfg)
		if err == nil || !strings.Contains(err.Error(), "pr_update_strategy") {
			t.Errorf("merge_strategy=%s: invalid pr_update_strategy not rejected by name, got %v", ms, err)
		}
	}
}
