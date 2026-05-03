package pilot

import (
	"os"
	"path/filepath"

	"github.com/BurntSushi/toml"
)

type PilotConfig struct {
	General PilotGeneralConfig `json:"general" toml:"general"`
	Prompts PilotPromptsConfig `json:"prompts" toml:"prompts"`
}

type PilotGeneralConfig struct {
	Model                   string  `json:"model" toml:"model"`
	ConfidenceThreshold     float64 `json:"confidence_threshold" toml:"confidence_threshold"`
	IdleTimeoutMs           uint64  `json:"idle_timeout_ms" toml:"idle_timeout_ms"`
	PendingResponseMaxAge   int64   `json:"pending_response_max_age_s" toml:"pending_response_max_age_s"`
	GracePeriodS            float64 `json:"grace_period_s" toml:"grace_period_s"`
	EscalationTimeoutS      float64 `json:"escalation_timeout_s" toml:"escalation_timeout_s"`
	SSEPort                 int     `json:"sse_port" toml:"sse_port"`
	MaxConcurrentEvals      int     `json:"max_concurrent_evals" toml:"max_concurrent_evals"`
	EvaluatorTimeoutMs      int     `json:"evaluator_timeout_ms" toml:"evaluator_timeout_ms"`
	MonthlySpendCapUSD      float64 `json:"monthly_spend_cap_usd" toml:"monthly_spend_cap_usd"`
	InputCostPerMTokUSD     float64 `json:"input_cost_per_mtok_usd" toml:"input_cost_per_mtok_usd"`
	OutputCostPerMTokUSD    float64 `json:"output_cost_per_mtok_usd" toml:"output_cost_per_mtok_usd"`
	InterrogationConfidence float64 `json:"interrogation_confidence" toml:"interrogation_confidence"`
	CodexStopHookReplies    bool    `json:"codex_stop_hook_replies" toml:"codex_stop_hook_replies"`
}

type PilotPromptsConfig struct {
	Approval    string `json:"approval" toml:"approval"`
	AutoRespond string `json:"auto_respond" toml:"auto_respond"`
}

func pilotConfigPath() string {
	return filepath.Join(pilotDir(), "pilot.toml")
}

func ReadPilotConfig() (PilotConfig, error) {
	var cfg PilotConfig
	md, err := toml.DecodeFile(pilotConfigPath(), &cfg)
	if err == nil {
		if cfg.General.Model == "" || cfg.General.Model == "haiku" {
			cfg.General.Model = "claude-haiku-4-5"
		}
		if !md.IsDefined("general", "monthly_spend_cap_usd") {
			cfg.General.MonthlySpendCapUSD = 20.0
		}
		if !md.IsDefined("general", "input_cost_per_mtok_usd") || cfg.General.InputCostPerMTokUSD <= 0 {
			cfg.General.InputCostPerMTokUSD = 1.0
		}
		if !md.IsDefined("general", "output_cost_per_mtok_usd") || cfg.General.OutputCostPerMTokUSD <= 0 {
			cfg.General.OutputCostPerMTokUSD = 5.0
		}
		if !md.IsDefined("general", "codex_stop_hook_replies") {
			cfg.General.CodexStopHookReplies = true
		}
	}
	return cfg, err
}

func WritePilotConfig(cfg PilotConfig) error {
	f, err := os.Create(pilotConfigPath())
	if err != nil {
		return err
	}
	defer f.Close()

	f.WriteString("# pilot configuration\n# Edit this file to tune behavior without recompiling.\n\n")

	enc := toml.NewEncoder(f)
	return enc.Encode(cfg)
}

func readSSEPort() int {
	cfg, err := ReadPilotConfig()
	if err != nil {
		return 9721
	}
	if cfg.General.SSEPort == 0 {
		return 9721
	}
	return cfg.General.SSEPort
}
