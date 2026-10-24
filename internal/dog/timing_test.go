package dog

import (
	"encoding/json"
	"testing"
	"time"
)

func TestNewWorkTiming_IgnoresUTCOffset(t *testing.T) {
	// Same instant written in -06:00 and in UTC must give identical elapsed time.
	// This is the regression: the deacon reported ~6h for a 7m run.
	local, err := time.Parse(time.RFC3339Nano, "2026-10-17T11:53:53.827522-06:00")
	if err != nil {
		t.Fatal(err)
	}
	now := local.Add(7 * time.Minute).UTC()

	got := NewWorkTiming(local, 5*time.Minute, now)

	if got.WorkElapsedSeconds != 420 {
		t.Errorf("WorkElapsedSeconds = %v, want 420", got.WorkElapsedSeconds)
	}
	if got.TimeoutSeconds != 300 {
		t.Errorf("TimeoutSeconds = %v, want 300", got.TimeoutSeconds)
	}
	if got.OverTimeoutRatio != 1.4 {
		t.Errorf("OverTimeoutRatio = %v, want 1.4", got.OverTimeoutRatio)
	}
}

func TestNewWorkTiming_DefaultsAndClamps(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

	got := NewWorkTiming(now.Add(-5*time.Minute), 0, now)
	if got.TimeoutSeconds != DefaultWorkTimeout.Seconds() {
		t.Errorf("zero timeout: TimeoutSeconds = %v, want default %v", got.TimeoutSeconds, DefaultWorkTimeout.Seconds())
	}
	if got.OverTimeoutRatio != 0.5 {
		t.Errorf("OverTimeoutRatio = %v, want 0.5", got.OverTimeoutRatio)
	}

	future := NewWorkTiming(now.Add(time.Hour), time.Minute, now)
	if future.WorkElapsedSeconds != 0 || future.OverTimeoutRatio != 0 {
		t.Errorf("future start: got %+v, want zero elapsed", future)
	}
}

func TestTimingFor(t *testing.T) {
	now := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	started := now.Add(-time.Minute)

	tests := []struct {
		name string
		d    *Dog
		want bool
	}{
		{"nil dog", nil, false},
		{"idle", &Dog{State: StateIdle, WorkStartedAt: started}, false},
		{"working without start", &Dog{State: StateWorking}, false},
		{"working", &Dog{State: StateWorking, WorkStartedAt: started}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := TimingFor(tt.d, time.Minute, now)
			if (got != nil) != tt.want {
				t.Fatalf("TimingFor() = %v, want non-nil=%v", got, tt.want)
			}
			if got != nil && got.OverTimeoutRatio != 1 {
				t.Errorf("OverTimeoutRatio = %v, want 1", got.OverTimeoutRatio)
			}
		})
	}
}

func TestResolveWorkTimeout(t *testing.T) {
	lookup := func(name string) (time.Duration, bool) {
		switch name {
		case "stuck-agent-dog":
			return 5 * time.Minute, true
		case "no-timeout":
			return 0, true
		}
		return 0, false
	}

	tests := []struct {
		work string
		want time.Duration
	}{
		{"plugin:stuck-agent-dog", 5 * time.Minute},
		{"plugin:no-timeout", DefaultWorkTimeout},
		{"plugin:unknown", DefaultWorkTimeout},
		{"plugin:", DefaultWorkTimeout},
		{"gt-abc", DefaultWorkTimeout},
		{"", DefaultWorkTimeout},
	}
	for _, tt := range tests {
		if got := ResolveWorkTimeout(tt.work, lookup); got != tt.want {
			t.Errorf("ResolveWorkTimeout(%q) = %v, want %v", tt.work, got, tt.want)
		}
	}
	if got := ResolveWorkTimeout("plugin:stuck-agent-dog", nil); got != DefaultWorkTimeout {
		t.Errorf("nil lookup = %v, want default", got)
	}
}

func TestWorkTiming_JSONFieldNames(t *testing.T) {
	b, err := json.Marshal(NewWorkTiming(time.Unix(0, 0), time.Minute, time.Unix(90, 0)))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]float64
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]float64{
		"work_elapsed_seconds": 90,
		"timeout_seconds":      60,
		"over_timeout_ratio":   1.5,
	} {
		if m[k] != want {
			t.Errorf("%s = %v, want %v (json: %s)", k, m[k], want, b)
		}
	}
}
