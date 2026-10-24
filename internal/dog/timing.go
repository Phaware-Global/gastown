package dog

import (
	"math"
	"strings"
	"time"
)

// DefaultWorkTimeout is the stuck threshold for work that names no plugin
// timeout of its own (infrastructure tasks).
const DefaultWorkTimeout = 10 * time.Minute

// pluginWorkPrefix marks Dog.Work assigned by `gt dog dispatch --plugin`.
const pluginWorkPrefix = "plugin:"

// WorkTiming reports how long a dog's current work has run against its timeout.
// Consumers read these fields instead of subtracting timestamps themselves:
// WorkStartedAt carries a UTC offset, and hand-computed durations have been
// off by exactly that offset.
type WorkTiming struct {
	WorkElapsedSeconds float64 `json:"work_elapsed_seconds"`
	TimeoutSeconds     float64 `json:"timeout_seconds"`
	// OverTimeoutRatio is elapsed/timeout: 1.0 is exactly at the timeout,
	// 1.4 is 40% past it.
	OverTimeoutRatio float64 `json:"over_timeout_ratio"`
}

// NewWorkTiming computes timing for work started at startedAt. A non-positive
// timeout falls back to DefaultWorkTimeout, and a start time in the future
// (clock skew) counts as zero elapsed.
func NewWorkTiming(startedAt time.Time, timeout time.Duration, now time.Time) WorkTiming {
	if timeout <= 0 {
		timeout = DefaultWorkTimeout
	}
	elapsed := now.Sub(startedAt)
	if elapsed < 0 {
		elapsed = 0
	}
	return WorkTiming{
		WorkElapsedSeconds: roundMillis(elapsed.Seconds()),
		TimeoutSeconds:     roundMillis(timeout.Seconds()),
		OverTimeoutRatio:   roundMillis(float64(elapsed) / float64(timeout)),
	}
}

// TimingFor returns the timing of d's current work, or nil when d is not
// working or has no recorded start time.
func TimingFor(d *Dog, timeout time.Duration, now time.Time) *WorkTiming {
	if d == nil || d.State != StateWorking || d.WorkStartedAt.IsZero() {
		return nil
	}
	t := NewWorkTiming(d.WorkStartedAt, timeout, now)
	return &t
}

// ResolveWorkTimeout returns the timeout for a dog's work string. Plugin work
// ("plugin:<name>") uses the plugin's [execution] timeout as reported by
// lookup; anything else, or a plugin with no usable timeout, gets
// DefaultWorkTimeout.
func ResolveWorkTimeout(work string, lookup func(pluginName string) (time.Duration, bool)) time.Duration {
	name, ok := strings.CutPrefix(work, pluginWorkPrefix)
	if !ok || name == "" || lookup == nil {
		return DefaultWorkTimeout
	}
	if d, found := lookup(name); found && d > 0 {
		return d
	}
	return DefaultWorkTimeout
}

func roundMillis(v float64) float64 {
	return math.Round(v*1000) / 1000
}
