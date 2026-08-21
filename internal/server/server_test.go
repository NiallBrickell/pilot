package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/NiallBrickell/pilot/internal/anthropic"
	"github.com/NiallBrickell/pilot/internal/config"
)

func TestEvaluatorRetryDelayUsesBoundedRetryAfter(t *testing.T) {
	if got := evaluatorRetryDelay(&anthropic.APIError{StatusCode: http.StatusBadGateway}); got != 300*time.Millisecond {
		t.Fatalf("default delay = %s", got)
	}
	if got := evaluatorRetryDelay(&anthropic.APIError{StatusCode: http.StatusTooManyRequests, RetryAfter: 3 * time.Second}); got != 3*time.Second {
		t.Fatalf("Retry-After delay = %s", got)
	}
	if got := evaluatorRetryDelay(&anthropic.APIError{StatusCode: http.StatusTooManyRequests, RetryAfter: time.Minute}); got != 5*time.Second {
		t.Fatalf("bounded Retry-After delay = %s", got)
	}
}

func TestEstimateUsageCostIncludesPromptCacheTelemetry(t *testing.T) {
	cfg := &config.PilotConfig{}
	cfg.General.InputCostPerMTokUSD = 1
	cfg.General.OutputCostPerMTokUSD = 5
	usage := anthropic.Usage{
		InputTokens:              1_000_000,
		OutputTokens:             1_000_000,
		CacheCreationInputTokens: 1_000_000,
		CacheReadInputTokens:     1_000_000,
	}
	if got := estimateUsageCostUSD(cfg, usage); got != 7.35 {
		t.Fatalf("estimated cached cost = %.2f, want 7.35", got)
	}
}
