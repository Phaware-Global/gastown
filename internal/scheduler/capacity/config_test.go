package capacity

import "testing"

func TestGetMaxPolecatsPerRig(t *testing.T) {
	three := 3
	zero := 0
	tests := []struct {
		name string
		cfg  *SchedulerConfig
		want int
	}{
		{"nil config", nil, 0},
		{"unset field", &SchedulerConfig{}, 0},
		{"explicit zero", &SchedulerConfig{MaxPolecatsPerRig: &zero}, 0},
		{"explicit value", &SchedulerConfig{MaxPolecatsPerRig: &three}, 3},
		{"default config has no per-rig limit", DefaultSchedulerConfig(), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.cfg.GetMaxPolecatsPerRig(); got != tt.want {
				t.Errorf("GetMaxPolecatsPerRig() = %d, want %d", got, tt.want)
			}
		})
	}
}
