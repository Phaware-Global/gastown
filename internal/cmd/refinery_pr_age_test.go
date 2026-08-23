package cmd

import (
	"errors"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/refinery"
)

// ageFakeProvider implements only what prAge touches; every other PRProvider
// method panics, so a change that made prAge reach further would fail loudly
// rather than silently pull in network calls on the escalation path.
type ageFakeProvider struct {
	refinery.PRProvider
	created time.Time
	err     error
}

func (f *ageFakeProvider) CreatedAt(int) (time.Time, error) { return f.created, f.err }

// The regression this guards is structural rather than behavioural: the age
// rail previously shipped with no caller able to populate ConvergenceInput.Age,
// so it could never fire. Every case here must therefore distinguish a real
// duration from the zero that means "unknown".
func TestPRAge(t *testing.T) {
	tests := []struct {
		name     string
		provider refinery.PRProvider
		pr       int
		wantZero bool
	}{
		{
			name:     "known creation time yields a real age",
			provider: &ageFakeProvider{created: time.Now().Add(-10 * 24 * time.Hour)},
			pr:       120,
			wantZero: false,
		},
		{
			// Bitbucket's answer. Must read as unknown, not as an age of zero.
			name:     "ErrUnsupported yields unknown",
			provider: &ageFakeProvider{err: refinery.ErrUnsupported},
			pr:       120,
			wantZero: true,
		},
		{
			name:     "provider error yields unknown",
			provider: &ageFakeProvider{err: errors.New("gh: token expired")},
			pr:       120,
			wantZero: true,
		},
		{
			// The inversion that would eject every PR: a zero time.Time read as
			// the epoch reports an age of decades.
			name:     "zero creation time yields unknown, not an epoch age",
			provider: &ageFakeProvider{created: time.Time{}},
			pr:       120,
			wantZero: true,
		},
		{
			name:     "creation time in the future is clock skew, not a new PR",
			provider: &ageFakeProvider{created: time.Now().Add(24 * time.Hour)},
			pr:       120,
			wantZero: true,
		},
		{name: "nil provider yields unknown", provider: nil, pr: 120, wantZero: true},
		{
			name:     "non-positive PR yields unknown",
			provider: &ageFakeProvider{created: time.Now().Add(-10 * 24 * time.Hour)},
			pr:       0,
			wantZero: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := prAge(tt.provider, tt.pr)
			if tt.wantZero && got != 0 {
				t.Errorf("prAge = %v, want 0 (unknown)", got)
			}
			if !tt.wantZero {
				if got <= 0 {
					t.Fatalf("prAge = %v, want a positive age", got)
				}
				// Guard the units: 10 days must not come back as 10 hours.
				if got < 9*24*time.Hour || got > 11*24*time.Hour {
					t.Errorf("prAge = %v, want roughly 10 days", got)
				}
			}
		})
	}
}
