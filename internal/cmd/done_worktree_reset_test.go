package cmd

import "testing"

func TestSuspectedWorktreeReset(t *testing.T) {
	const base = "e92c0ee272ff64ae363716204f641a84d5d354c3"
	const polecatCommit = "aaaa1111bbbb2222cccc3333dddd4444eeee5555"

	tests := []struct {
		name          string
		isPolecat     bool
		cleanupStatus string
		isNoMergeTask bool
		headSHA       string
		baseTipSHA    string
		want          bool
	}{
		{
			name:      "reset: code polecat HEAD equals base tip",
			isPolecat: true, headSHA: base, baseTipSHA: base,
			want: true, // the hga-y3jm false-completion shape
		},
		{
			name:      "genuine work: HEAD is a distinct polecat commit",
			isPolecat: true, headSHA: polecatCommit, baseTipSHA: base,
			want: false,
		},
		{
			name:          "report-only task exempt even at base tip",
			isPolecat:     true,
			cleanupStatus: "clean",
			headSHA:       base, baseTipSHA: base,
			want: false,
		},
		{
			name:          "no_merge task exempt even at base tip",
			isPolecat:     true,
			isNoMergeTask: true,
			headSHA:       base, baseTipSHA: base,
			want: false,
		},
		{
			name:      "non-polecat (crew/mayor) exempt",
			isPolecat: false, headSHA: base, baseTipSHA: base,
			want: false,
		},
		{
			name:      "fails open when base tip unknown",
			isPolecat: true, headSHA: base, baseTipSHA: "",
			want: false,
		},
		{
			name:      "fails open when HEAD unknown",
			isPolecat: true, headSHA: "", baseTipSHA: base,
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := suspectedWorktreeReset(tt.isPolecat, tt.cleanupStatus, tt.isNoMergeTask, tt.headSHA, tt.baseTipSHA)
			if got != tt.want {
				t.Errorf("suspectedWorktreeReset() = %v, want %v", got, tt.want)
			}
		})
	}
}
