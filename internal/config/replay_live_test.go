//go:build replay

package config_test

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/NiallBrickell/pilot/internal/anthropic"
	"github.com/NiallBrickell/pilot/internal/approve"
	"github.com/NiallBrickell/pilot/internal/config"
	"github.com/NiallBrickell/pilot/internal/paths"
)

// TestReplayAgainstEvaluator runs the corpus through the real pipeline —
// Layer 2, then the evaluator with the embedded prompt — and reports every
// case that lands the wrong way. Costs a few cents per run; build-tagged so it
// stays off `go test ./...`.
//
//	go test -tags=replay ./internal/config -run TestReplayAgainstEvaluator -v
func TestReplayAgainstEvaluator(t *testing.T) {
	ai, err := anthropic.NewClient(30*time.Second, paths.EnvFile())
	if err != nil {
		t.Skipf("no Anthropic key available: %v", err)
	}
	// Evaluate against the prompt compiled into this build, not whatever the
	// local ~/.pilot/pilot.toml happens to hold.
	var promptCfg config.PilotConfig
	if _, err := toml.Decode(config.EmbeddedConfig(), &promptCfg); err != nil {
		t.Fatalf("decode embedded config: %v", err)
	}
	model := config.Load().General.Model

	type outcome struct {
		c      replayCase
		got    string
		source string
		reason string
	}
	results := make([]outcome, len(replayCorpus))

	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, tc := range replayCorpus {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			var parsed map[string]any
			_ = json.Unmarshal([]byte(tc.input), &parsed)
			if d := approve.CheckPilotRules(&promptCfg, tc.tool, parsed, "/Users/niall/work/projects/acme"); d != "" {
				results[i] = outcome{tc, d, "pilot_rules", "matched pilot rule"}
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			res, err := ai.EvaluateApproval(ctx, promptCfg.Prompts.Approval, tc.tool, tc.input, model)
			if err != nil {
				results[i] = outcome{tc, "error", "api", err.Error()}
				return
			}
			got := "deny"
			if res.Decision == anthropic.Approve {
				got = "approve"
			}
			results[i] = outcome{tc, got, "llm", res.Reason}
		}()
	}
	wg.Wait()

	var wrong int
	for _, r := range results {
		mark := "ok  "
		if r.got != r.c.want {
			mark = "WRONG"
			wrong++
		}
		t.Logf("%s %-28s want=%-7s got=%-7s [%s] %s", mark, r.c.name, r.c.want, r.got, r.source, truncate(r.reason, 130))
	}
	if wrong > 0 {
		t.Errorf("%d/%d replay cases decided the wrong way", wrong, len(results))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
