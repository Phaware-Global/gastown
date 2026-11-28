package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/doltserver"
)

// fakeDoltBackup puts a dolt on PATH that runs list for "dolt backup" and sync
// for "dolt backup sync <name>".
func fakeDoltBackup(t *testing.T, list, sync string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs a shell script on PATH")
	}
	bin := t.TempDir()
	script := "#!/bin/sh\nif [ \"$2\" = sync ]; then\n" + sync + "\nelse\n" + list + "\nfi\n"
	if err := os.WriteFile(filepath.Join(bin, "dolt"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestMaintainRewritesHistoryOnlyAfterBackup(t *testing.T) {
	const listed = "echo hq-backup"
	tests := []struct {
		name        string
		list, sync  string
		listTimeout time.Duration
		wantRewrite bool
		wantErr     string
	}{
		{name: "listing times out", list: "exec sleep 5", sync: "exit 0",
			listTimeout: 200 * time.Millisecond, wantErr: "hq"},
		{name: "listing errors", list: "echo locked >&2; exit 1", sync: "exit 0", wantErr: "hq"},
		{name: "sync fails", list: listed, sync: "echo disk full >&2; exit 1", wantErr: "hq"},
		{name: "no backup configured", list: "echo origin", sync: "exit 0"},
		{name: "backup succeeds", list: listed, sync: "exit 0", wantRewrite: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fakeDoltBackup(t, tt.list, tt.sync)
			dataDir := t.TempDir()
			if err := os.Mkdir(filepath.Join(dataDir, "hq"), 0o755); err != nil {
				t.Fatal(err)
			}
			if tt.listTimeout == 0 {
				tt.listTimeout = 30 * time.Second
			}
			count := func(string) (int, error) { return 500, nil }

			var flattened, collected []string
			infos := maintainPlanDatabases(dataDir, []string{"hq"}, count, tt.listTimeout)
			err := maintainExecute(dataDir, infos, 100, maintainOps{
				reap:    func(string) (int, error) { return 0, nil },
				count:   count,
				flatten: func(db string) error { flattened = append(flattened, db); return nil },
				gc:      func(db string) error { collected = append(collected, db); return nil },
			})

			var want []string
			if tt.wantRewrite {
				want = []string{"hq"}
			}
			if !slices.Equal(flattened, want) {
				t.Errorf("flattened = %v, want %v", flattened, want)
			}
			if !slices.Equal(collected, want) {
				t.Errorf("gc ran on %v, want %v", collected, want)
			}
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("error = %v, want one naming %q", err, tt.wantErr)
			}
		})
	}
}

func TestMaintainQuoteDBName(t *testing.T) {
	tests := []struct {
		name string
		want string // empty: the name is refused
	}{
		{name: "hq", want: "`hq`"},
		{name: "My_rig2", want: "`My_rig2`"},
		{name: ""},
		{name: "a`b"},
		{name: "a'b"},
		{name: `a"b`},
		{name: "a;b"},
		{name: "a b"},
		{name: "a-b"},
		{name: "a.b"},
		{name: "hq\n"},
	}
	for _, tt := range tests {
		got, err := maintainQuoteDBName(tt.name)
		if got != tt.want || (err == nil) != (tt.want != "") {
			t.Errorf("maintainQuoteDBName(%q) = %q, %v; want %q", tt.name, got, err, tt.want)
		}
	}
}

func TestMaintainRefusesInvalidDBName(t *testing.T) {
	const bad = "a`b"
	count := func(string) (int, error) { return 500, nil }
	infos := maintainPlanDatabases(t.TempDir(), []string{bad}, count, time.Second)
	if err := infos[0].listErr; err == nil || !strings.Contains(err.Error(), "invalid database name") {
		t.Errorf("plan error = %v, want invalid database name", err)
	}
	for step, err := range map[string]error{
		"count":   func() error { _, err := maintainCountCommits(&doltserver.Config{}, bad); return err }(),
		"flatten": maintainFlattenDB(&doltserver.Config{Port: 1}, bad),
	} {
		if err == nil || !strings.Contains(err.Error(), "invalid database name") {
			t.Errorf("%s error = %v, want invalid database name", step, err)
		}
	}
}
