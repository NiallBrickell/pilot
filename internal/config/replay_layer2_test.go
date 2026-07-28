package config_test

import (
	"encoding/json"
	"testing"

	"github.com/erdoai/pilot/internal/approve"
	"github.com/erdoai/pilot/internal/config"
)

// TestReplayCorpusLayer2 checks the deterministic layer against the replay
// corpus without spending an API call. Layer 2 only ever approves, so the bar
// is: it must approve the cases we put there for it, and it must never approve
// one of the calls the user wants to review by hand.
func TestReplayCorpusLayer2(t *testing.T) {
	cfg := &config.PilotConfig{}
	decided := 0
	for _, tc := range replayCorpus {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(tc.input), &parsed); err != nil {
			t.Fatalf("%s: bad corpus input: %v", tc.name, err)
		}
		got := approve.CheckPilotRules(cfg, tc.tool, parsed, "/Users/niall/work/erdo/erdo")
		if got == "approve" {
			decided++
			if tc.want == "deny" {
				t.Errorf("%s: Layer 2 fast-approved a call that must reach the human", tc.name)
			}
		}
	}
	// Web reads and the git commands the evaluator kept over-denying should all
	// be settled here, off the LLM path entirely.
	if decided < 10 {
		t.Errorf("Layer 2 settled only %d corpus cases; expected the web and git families to be handled deterministically", decided)
	}
	t.Logf("Layer 2 settled %d/%d corpus cases without an LLM call", decided, len(replayCorpus))
}
