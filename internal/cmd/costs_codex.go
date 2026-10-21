package cmd

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Codex agents run on a ChatGPT subscription, which has no per-token price.
// Their usage is priced at the OpenAI API rate instead, so Codex and Claude
// spend are directly comparable in gt costs.

// openAIPricing is the API price per million tokens for the models Codex
// offers, keyed by the model slug Codex records. Columns: input, output,
// cached input, cache write. OpenAI publishes no separate cache-write price,
// so cache writes are charged at the input rate. Requests over 272K input
// tokens are billed at a higher tier, which is not modelled.
// Source: OpenAI API pricing via https://benchlm.ai/openai/api-pricing and
// https://openai.com/index/introducing-gpt-6-1-sol/ (2026-10-20).
var openAIPricing = map[string]modelPrice{
	"gpt-6.1-sol":   {2, 10, 0.10, 2},
	"gpt-6-sol":     {2, 10, 0.20, 2},
	"gpt-6-astra":   {10, 50, 1, 10},
	"gpt-6-luna":    {0.10, 0.50, 0.01, 0.10},
	"gpt-5.6-sol":   {4, 20, 0.40, 4},
	"gpt-5.6-terra": {2, 12, 0.20, 2},
	"gpt-5.6-luna":  {0.20, 1.20, 0.02, 0.20},
}

// codexRolloutMaxAgeDays bounds the search for a work directory's rollout to
// the most recent date directories under the Codex sessions tree.
const codexRolloutMaxAgeDays = 8

// codexSessionsDir is where Codex writes session rollouts.
func codexSessionsDir() (string, error) {
	if home := os.Getenv("CODEX_HOME"); home != "" {
		return filepath.Join(home, "sessions"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".codex", "sessions"), nil
}

// findLatestCodexRollout returns the most recently modified rollout whose
// session started in workDir. Rollouts live in sessions/YYYY/MM/DD/, so only
// the last few days' directories are searched.
func findLatestCodexRollout(sessionsDir, workDir string, now time.Time) (string, time.Time, error) {
	type candidate struct {
		path string
		mod  time.Time
	}
	var candidates []candidate
	for d := 0; d < codexRolloutMaxAgeDays; d++ {
		dir := filepath.Join(sessionsDir, now.AddDate(0, 0, -d).Format("2006/01/02"))
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasPrefix(e.Name(), "rollout-") || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			info, err := e.Info()
			if err != nil {
				continue
			}
			candidates = append(candidates, candidate{filepath.Join(dir, e.Name()), info.ModTime()})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].mod.After(candidates[j].mod) })
	for _, c := range candidates {
		if rolloutCwd(c.path) == workDir {
			return c.path, c.mod, nil
		}
	}
	return "", time.Time{}, fmt.Errorf("no Codex rollout for %s in %s", workDir, sessionsDir)
}

// rolloutCwd reads the session_meta line that opens a rollout.
func rolloutCwd(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	line, err := bufio.NewReader(f).ReadBytes('\n')
	if err != nil && err != io.EOF {
		return ""
	}
	var meta struct {
		Type    string `json:"type"`
		Payload struct {
			Cwd string `json:"cwd"`
		} `json:"payload"`
	}
	if json.Unmarshal(line, &meta) != nil || meta.Type != "session_meta" {
		return ""
	}
	return meta.Payload.Cwd
}

// codexUsage is the token count of one model response in a rollout.
// input_tokens includes the cached and cache-write tokens; output_tokens
// includes reasoning tokens.
type codexUsage struct {
	InputTokens           int `json:"input_tokens"`
	CachedInputTokens     int `json:"cached_input_tokens"`
	CacheWriteInputTokens int `json:"cache_write_input_tokens"`
	OutputTokens          int `json:"output_tokens"`
}

// parseCodexRolloutCost prices every model response in a rollout. Each
// token_usage_record line carries one response's usage; the model is the one
// named by the latest turn_context line before it. Returns the total and the
// models that have no price, whose tokens are left out.
func parseCodexRolloutCost(path string) (float64, []string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, nil, err
	}
	defer f.Close()

	var (
		total    float64
		model    string
		unpriced []string
		seen     = map[string]bool{}
	)
	r := bufio.NewReader(f)
	for {
		line, err := r.ReadBytes('\n')
		// Other line types (tool output, world state) can be large; decode
		// only the two this needs.
		isUsage := bytes.Contains(line, []byte(`"type":"token_usage_record"`))
		if isUsage || bytes.Contains(line, []byte(`"type":"turn_context"`)) {
			var rec struct {
				Type    string `json:"type"`
				Payload struct {
					Model string     `json:"model"`
					Usage codexUsage `json:"usage"`
				} `json:"payload"`
			}
			if json.Unmarshal(line, &rec) == nil {
				switch rec.Type {
				case "turn_context":
					model = rec.Payload.Model
				case "token_usage_record":
					p, ok := openAIPricing[model]
					if !ok {
						if !seen[model] {
							seen[model] = true
							unpriced = append(unpriced, model)
						}
						break
					}
					total += codexUsageCost(rec.Payload.Usage, p)
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return 0, nil, err
		}
	}
	return total, unpriced, nil
}

func codexUsageCost(u codexUsage, p modelPrice) float64 {
	uncached := max(u.InputTokens-u.CachedInputTokens-u.CacheWriteInputTokens, 0)
	return (float64(uncached)*p.InputPerMillion +
		float64(u.CachedInputTokens)*p.CacheReadPerMillion +
		float64(u.CacheWriteInputTokens)*p.CacheCreatePerMillion +
		float64(u.OutputTokens)*p.OutputPerMillion) / 1_000_000
}

// extractCodexCostFromWorkDir prices the latest Codex rollout for workDir.
func extractCodexCostFromWorkDir(workDir string) (float64, string, time.Time, error) {
	dir, err := codexSessionsDir()
	if err != nil {
		return 0, "", time.Time{}, err
	}
	path, mod, err := findLatestCodexRollout(dir, workDir, time.Now())
	if err != nil {
		return 0, "", time.Time{}, err
	}
	cost, unpriced, err := parseCodexRolloutCost(path)
	if err != nil {
		return 0, "", time.Time{}, fmt.Errorf("parsing Codex rollout: %w", err)
	}
	for _, m := range unpriced {
		fmt.Fprintf(os.Stderr, "gt costs: no API price for Codex model %q in %s; its tokens are not counted\n", m, filepath.Base(path))
	}
	return cost, filepath.Base(path), mod, nil
}
