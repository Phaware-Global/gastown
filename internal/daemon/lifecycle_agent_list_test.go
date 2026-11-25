package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// The listing finds agents by label and by type=agent. It must not pass
// --include-infra, which makes bd scan every infrastructure record in a town
// database that is dominated by wisps.
func TestListAgentBeadsJSONUsesLabelAndTypeQueries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a shell bd stub")
	}
	// Each route has a record only it returns, plus one that both return. The
	// label route is truncated unless --limit=0 is passed.
	stub := `#!/bin/sh
echo "$*" >> "$0.log"
[ "$1" = mol ] && { echo '{"count":0,"wisps":[]}'; exit 0; }
labeled='{"id":"gt-demo-polecat-labeled","issue_type":"task","labels":["gt:agent"]}'
typed='{"id":"gt-demo-polecat-typed","issue_type":"agent"}'
both='{"id":"gt-demo-polecat-both","issue_type":"agent","labels":["gt:agent"]}'
case " $* " in
  *" --type=agent "*) echo "[$typed,$both]" ;;
  *" --label=gt:agent "*)
    case " $* " in
      *" --limit=0 "*) echo "[$both,$labeled]" ;;
      *) echo "[$both]" ;;
    esac ;;
  *) echo '[]' ;;
esac
`
	bdPath := filepath.Join(t.TempDir(), "bd")
	if err := os.WriteFile(bdPath, []byte(stub), 0o700); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}

	d := &Daemon{config: &Config{TownRoot: t.TempDir()}, bdPath: bdPath}

	var agents []struct {
		ID string `json:"id"`
	}
	if err := d.listAgentBeadsJSON(&agents); err != nil {
		t.Fatalf("listAgentBeadsJSON: %v", err)
	}
	var got []string
	for _, a := range agents {
		got = append(got, a.ID)
	}
	slices.Sort(got)
	want := []string{"gt-demo-polecat-both", "gt-demo-polecat-labeled", "gt-demo-polecat-typed"}
	if !slices.Equal(got, want) {
		t.Errorf("agent IDs = %v, want %v", got, want)
	}

	calls, err := os.ReadFile(bdPath + ".log")
	if err != nil {
		t.Fatalf("read bd call log: %v", err)
	}
	if strings.Contains(string(calls), "--include-infra") {
		t.Errorf("listing passed --include-infra:\n%s", calls)
	}
}
