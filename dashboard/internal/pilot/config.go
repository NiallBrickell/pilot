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
	InterrogationConfidence float64 `json:"interrogation_confidence" toml:"interrogation_confidence"`
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
	_, err := toml.DecodeFile(pilotConfigPath(), &cfg)
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
