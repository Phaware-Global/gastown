package doctor

import (
	"testing"
	"time"
)

func TestCountAbandonedWispsFromJSON(t *testing.T) {
	// Fixed reference cutoff so test cases are deterministic.
	cutoff, err := time.Parse(time.RFC3339, "2026-05-24T12:00:00Z")
	if err != nil {
		t.Fatalf("setup: parse cutoff: %v", err)
	}

	tests := []struct {
		name string
		in   string
		want int
	}{
		{
			name: "bd >=1.0.3 envelope, one old open wisp",
			in: `{
				"schema_version": 1,
				"count": 1,
				"wisps": [
					{"id":"a","status":"open","ephemeral":true,"updated_at":"2026-05-24T10:00:00Z"}
				]
			}`,
			want: 1,
		},
		{
			name: "bd >=1.0.3 envelope, closed wisps ignored",
			in: `{
				"schema_version": 1,
				"count": 2,
				"wisps": [
					{"id":"a","status":"closed","updated_at":"2026-05-24T10:00:00Z"},
					{"id":"b","status":"open","updated_at":"2026-05-24T10:00:00Z"}
				]
			}`,
			want: 1,
		},
		{
			name: "bd >=1.0.3 envelope, recent wisps ignored",
			in: `{
				"schema_version": 1,
				"wisps": [
					{"id":"a","status":"open","updated_at":"2026-05-24T13:00:00Z"}
				]
			}`,
			want: 0,
		},
		{
			name: "bd >=1.0.3 envelope, empty wisps array",
			in:   `{"schema_version": 1, "count": 0, "wisps": []}`,
			want: 0,
		},
		{
			name: "bd >=1.0.3 envelope, malformed updated_at skipped",
			in: `{
				"schema_version": 1,
				"wisps": [
					{"id":"a","status":"open","updated_at":"not-a-date"},
					{"id":"b","status":"open","updated_at":"2026-05-24T10:00:00Z"}
				]
			}`,
			want: 1,
		},
		{
			name: "legacy bare-array shape returns 0 (no wisps key)",
			in:   `[{"id":"a","status":"open","updated_at":"2026-05-24T10:00:00Z"}]`,
			want: 0,
		},
		{
			name: "invalid json returns 0",
			in:   `not json`,
			want: 0,
		},
		{
			name: "empty envelope",
			in:   `{"schema_version": 1}`,
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := countAbandonedWispsFromJSON([]byte(tt.in), cutoff)
			if got != tt.want {
				t.Errorf("countAbandonedWispsFromJSON = %d, want %d", got, tt.want)
			}
		})
	}
}
