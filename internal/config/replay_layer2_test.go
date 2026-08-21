package config_test

import (
	"encoding/json"
	"testing"

	"github.com/NiallBrickell/pilot/internal/approve"
	"github.com/NiallBrickell/pilot/internal/config"
)

// TestReplayCorpusLayer2 checks the deterministic boundary against the replay
// corpus without spending an API call. Every routine case must settle locally;
// every deliberate manual gate must retain the residual evaluator path.
func TestReplayCorpusLayer2(t *testing.T) {
	cfg := &config.PilotConfig{}
	decided := 0
	for _, tc := range replayCorpus {
		var parsed map[string]any
		if err := json.Unmarshal([]byte(tc.input), &parsed); err != nil {
			t.Fatalf("%s: bad corpus input: %v", tc.name, err)
		}
		got := approve.CheckPilotRules(cfg, tc.tool, parsed, "/Users/niall/work/projects/myapp")
		if got == "approve" {
			decided++
			if tc.want == "deny" {
				t.Errorf("%s: Layer 2 fast-approved a call that must reach the human", tc.name)
			}
		} else if tc.want == "approve" {
			t.Errorf("%s: routine call still falls through to the paid evaluator", tc.name)
		}
	}
	t.Logf("Layer 2 settled %d/%d corpus cases without an LLM call", decided, len(replayCorpus))
}
