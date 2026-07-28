//go:build replay

package config_test

import (
	"context"
	"encoding/json"
	"os"
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
//
// Set PILOT_REPLAY_REQUIRE_KEY=1 to make a missing key fatal instead of a skip.
// CI sets it, so a secret that goes missing turns the build red rather than
// quietly passing a job whose whole purpose is to have made the API calls.
func TestReplayAgainstEvaluator(t *testing.T) {
	ai, err := anthropic.NewClient(30*time.Second, paths.EnvFile())
	if err != nil {
		if os.Getenv("PILOT_REPLAY_REQUIRE_KEY") != "" {
			t.Fatalf("PILOT_REPLAY_REQUIRE_KEY is set but no key resolved: %v", err)
		}
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
			// A timeout or 5xx is infra, not a verdict — retry once before
			// calling it a failure, the same distinction the server makes.
			var res *anthropic.ApprovalResult
			var err error
			for attempt := range 2 {
				if attempt > 0 {
					time.Sleep(time.Second)
				}
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				res, err = ai.EvaluateApproval(ctx, promptCfg.Prompts.Approval, tc.tool, tc.input, model)
				cancel()
				if err == nil {
					break
				}
			}
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

	var wrong, viaRules int
	for _, r := range results {
		mark := "ok  "
		if r.got != r.c.want {
			mark = "WRONG"
			wrong++
		}
		if r.source == "pilot_rules" {
			viaRules++
		}
		t.Logf("%s %-28s want=%-7s got=%-7s [%s] %s", mark, r.c.name, r.c.want, r.got, r.source, truncate(r.reason, 130))
	}
	t.Logf("%d/%d correct (%d settled by pilot rules, %d by the evaluator)",
		len(results)-wrong, len(results), viaRules, len(results)-viaRules)
	if wrong > 0 {
		t.Errorf("%d/%d replay cases decided the wrong way — the prompt regressed, see the WRONG lines above", wrong, len(results))
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
