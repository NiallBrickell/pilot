package config

import (
	_ "embed"
	"fmt"
	"os"
	"sync"

	"github.com/BurntSushi/toml"
	"github.com/NiallBrickell/pilot/internal/paths"
)

//go:embed pilot.toml
var embeddedConfig string

// EmbeddedConfig returns the compiled-in default config for auto-setup.
func EmbeddedConfig() string {
	return embeddedConfig
}

var (
	cfg  *PilotConfig
	once sync.Once
)

type PilotConfig struct {
	General  GeneralConfig  `toml:"general"`
	Prompts  PromptsConfig  `toml:"prompts"`
	Webhooks []WebhookConfig `toml:"webhooks"`
}

type GeneralConfig struct {
	Model                 string  `toml:"model"`
	ConfidenceThreshold   float64 `toml:"confidence_threshold"`
	IdleTimeoutMs         uint64  `toml:"idle_timeout_ms"`
	PendingResponseMaxAge int64   `toml:"pending_response_max_age_s"`
	GracePeriodS          float64 `toml:"grace_period_s"`
	EscalationTimeoutS    float64 `toml:"escalation_timeout_s"`
	SSEPort               int     `toml:"sse_port"`

	// Evaluator settings
	EvaluatorPort       int `toml:"evaluator_port"`
	MaxConcurrentEvals  int `toml:"max_concurrent_evals"`
	EvaluatorTimeoutMs  int `toml:"evaluator_timeout_ms"`

	// Interrogation settings
	InterrogationConfidence float64 `toml:"interrogation_confidence"`
	InterrogationModel      string  `toml:"interrogation_model"`
	InterrogationEnabled    *bool   `toml:"interrogation_enabled"`

	// Auto-approve all tool calls without evaluation (for autonomous/sandboxed use).
	// Interrogation still runs on schedule.
	AutoApproveAll bool `toml:"auto_approve_all"`
}

type PromptsConfig struct {
	Approval    string `toml:"approval"`
	AutoRespond string `toml:"auto_respond"`
}

// WebhookConfig defines an HTTP endpoint that receives pilot events.
type WebhookConfig struct {
	URL    string   `toml:"url"`
	Events []string `toml:"events"` // e.g. ["action", "pending_approval", "approval_resolved"] — empty means all
	Secret string   `toml:"secret"` // optional HMAC signing secret
}

func configPath() string {
	if p := os.Getenv("PILOT_CONFIG"); p != "" {
		return p
	}
	return paths.ConfigFile()
}

func StateFilePath() string {
	if p := os.Getenv("PILOT_STATE_FILE"); p != "" {
		return p
	}
	return paths.StateFile()
}

func SSEBaseURL(cfg *PilotConfig) string {
	port := cfg.General.SSEPort
	if port == 0 {
		port = 9721
	}
	return fmt.Sprintf("http://localhost:%d", port)
}


func PidFilePath() string {
	return paths.PidFile()
}

func ConfigPath() string {
	return configPath()
}

func Load() *PilotConfig {
	once.Do(func() {
		cfg = &PilotConfig{}
		path := configPath()
		content, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not read %s: %v. Using defaults.\n", path, err)
			if _, err2 := toml.Decode(embeddedConfig, cfg); err2 != nil {
				panic("embedded pilot.toml must be valid: " + err2.Error())
			}
			return
		}
		if _, err := toml.Decode(string(content), cfg); err != nil {
			fmt.Fprintf(os.Stderr, "Warning: could not parse pilot.toml: %v. Using defaults.\n", err)
			if _, err2 := toml.Decode(embeddedConfig, cfg); err2 != nil {
				panic("embedded pilot.toml must be valid: " + err2.Error())
			}
		}
	})
	return cfg
}
