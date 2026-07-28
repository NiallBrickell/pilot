package config

import (
	"slices"
	"testing"

	"github.com/erdoai/pilot/internal/paths"
)

// TestEmbeddedPromptHashIsRecorded guards the one maintenance step the upgrade
// mechanism depends on. If a prompt change ships without its hash recorded here,
// every user who already has that prompt looks "customised" to UpgradeDefaults
// and stops receiving default prompts for good.
func TestEmbeddedPromptHashIsRecorded(t *testing.T) {
	hash, err := paths.PromptHashFromTOML(EmbeddedConfig())
	if err != nil {
		t.Fatalf("hash embedded config: %v", err)
	}
	if slices.Contains(ShippedPromptHashes(), hash) {
		return
	}
	t.Fatalf("prompts in pilot.toml changed but the new hash isn't in prompt_history.txt.\nAppend this line to internal/config/prompt_history.txt:\n\n%s\n", hash)
}

func TestShippedPromptHashesSkipsCommentsAndBlanks(t *testing.T) {
	hashes := ShippedPromptHashes()
	if len(hashes) < 2 {
		t.Fatalf("expected the shipped history to hold every past default, got %d", len(hashes))
	}
	seen := map[string]bool{}
	for _, h := range hashes {
		if len(h) != 64 {
			t.Errorf("not a sha256 hex digest: %q", h)
		}
		if seen[h] {
			t.Errorf("duplicate hash in prompt_history.txt: %s", h)
		}
		seen[h] = true
	}
}
