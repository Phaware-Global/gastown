package witness

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestDetectZombieHoldMountainState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX tmux stub")
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "tmux"), []byte("#!/bin/sh\necho 'no server running' >&2\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	oldRestart := restartPolecatSession
	t.Cleanup(func() { restartPolecatSession = oldRestart })
	for _, held := range []bool{true, false} {
		t.Run(fmt.Sprintf("held=%v", held), func(t *testing.T) {
			root := t.TempDir()
			for _, dir := range []string{"mayor", "testrig/polecats/alpha"} {
				if err := os.MkdirAll(filepath.Join(root, dir), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(root, "mayor/town.json"), []byte(`{"name":"test"}`), 0644); err != nil {
				t.Fatal(err)
			}
			labels := []string{"task", "keep-me"}
			originalLabels := append([]string(nil), labels...)
			status := "open"
			writes, restarts := 0, 0
			restartPolecatSession = func(_, _, _ string) error { restarts++; return nil }
			bd := &BdCli{
				Exec: func(_ string, args ...string) (string, error) {
					switch args[0] {
					case "show":
						switch args[1] {
						case "test-task":
							data, _ := json.Marshal([]map[string]any{{"status": status, "labels": labels}})
							return string(data), nil
						case "test-convoy":
							return `[{"labels":["mountain"]}]`, nil
						default:
							return `[{"agent_state":"working","hook_bead":"test-task"}]`, nil
						}
					case "dep":
						return `[{"id":"test-convoy","type":"tracks"}]`, nil
					case "list":
						return `[]`, nil
					default:
						t.Fatalf("unexpected bd exec: %v", args)
						return "", nil
					}
				},
				Run: func(_ string, args ...string) error {
					writes++
					if len(args) < 2 || args[0] != "update" || args[1] != "test-task" {
						t.Fatalf("unexpected bd write: %v", args)
					}
					for i := 2; i < len(args); i++ {
						switch args[i] {
						case "--remove-label":
							i++
							for j, label := range labels {
								if label == args[i] {
									labels = append(labels[:j], labels[j+1:]...)
									break
								}
							}
						case "--add-label":
							i++
							labels = append(labels, args[i])
						case "--status=blocked":
							status = "blocked"
						case "--notes":
							i++
						default:
							t.Fatalf("unexpected update flag: %v", args)
						}
					}
					return nil
				},
			}
			for scan := 1; scan <= 3; scan++ {
				result := DetectZombiePolecatsWithOptions(bd, root, "testrig", nil, DetectZombieOptions{NoRestart: held})
				if result.Checked != 1 || len(result.Zombies) != 1 || len(result.Errors) != 0 {
					t.Fatalf("scan %d: %+v", scan, result)
				}
				z := result.Zombies[0]
				if z.Classification != ZombieSessionDeadActive || z.HookBead != "test-task" || !z.WasActive {
					t.Fatalf("detection lost: %+v", z)
				}
				if held {
					if z.Action != restartSuppressedAction(z.Classification) || restarts != 0 || writes != 0 || status != "open" || !reflect.DeepEqual(labels, originalLabels) || len(result.ConvoyFailures) != 0 {
						t.Errorf("held scan %d mutated state: zombie=%+v restarts=%d writes=%d status=%s labels=%v failures=%v", scan, z, restarts, writes, status, labels, result.ConvoyFailures)
					}
				} else {
					if restarts != scan || len(result.ConvoyFailures) != 1 || getMountainFailureCount(labels) != scan {
						t.Fatalf("normal scan %d failed recovery/tracking: restarts=%d labels=%v result=%+v", scan, restarts, labels, result)
					}
				}
			}
			if !held && (status != "blocked" || !strings.Contains(strings.Join(labels, ","), "mountain:skipped")) {
				t.Fatalf("normal tracking did not skip after 3 failures: %s %v", status, labels)
			}
		})
	}
}
