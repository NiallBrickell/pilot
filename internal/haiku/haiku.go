package haiku

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/NiallBrickell/pilot/internal/approve"
	"github.com/NiallBrickell/pilot/internal/config"
)

func init() {
	loadPilotEnv()
}

type EvalAction int

const (
	Allow EvalAction = iota
	Deny
)

type EvalDecision struct {
	Action EvalAction
	Reason string
}

type AutoResponse struct {
	ShouldRespond bool    `json:"should_respond"`
	Message       string  `json:"message"`
	Confidence    float64 `json:"confidence"`
	Reasoning     string  `json:"reasoning"`
}

// EvaluateApproval evaluates whether to approve a tool call.
// Returns nil if Claude would auto-approve this tool — the caller should
// pass through without interfering.
func EvaluateApproval(cfg *config.PilotConfig, toolName, toolInput string) (*EvalDecision, error) {
	// Check layers 1+2 before falling through to haiku
	if decision := approve.Evaluate(cfg, toolName, toolInput, ""); decision != nil {
		slog.Debug("Decided without haiku", "tool", toolName, "action", decision.Action, "source", decision.Source)
		if decision.Action == "deny" {
			return &EvalDecision{Action: Deny, Reason: decision.Reason}, nil
		}
		return nil, nil // passthrough
	}

	// This tool would normally prompt the user — ask Haiku to decide.
	userMessage := fmt.Sprintf("Tool: %s\nInput: %s", toolName, truncate(toolInput, 2000))

	result, err := callAnthropic(cfg.General.Model, cfg.Prompts.Approval, userMessage)
	if err != nil {
		return nil, err
	}

	var parsed struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(result), &parsed); err != nil {
		return nil, fmt.Errorf("parsing approval response: %w (raw: %s)", err, result)
	}

	action := Deny
	if parsed.Decision == "approve" {
		action = Allow
	}
	slog.Info("Haiku decision", "tool", toolName, "action", parsed.Decision, "reason", parsed.Reason)
	return &EvalDecision{Action: action, Reason: parsed.Reason}, nil
}

// EvaluateIdle evaluates whether Claude's pause needs an auto-response.
func EvaluateIdle(cfg *config.PilotConfig, ctx string) (*AutoResponse, error) {
	userMessage := fmt.Sprintf(
		"Here is the recent Claude Code output. Claude has stopped and is showing the input prompt. Should I auto-respond?\n\n---\n%s",
		truncate(ctx, 4000),
	)

	return evaluateIdleInternal(cfg.General.Model, cfg.Prompts.AutoRespond, userMessage, cfg.General.ConfidenceThreshold)
}

// EvaluateIdleWithPrompt is like EvaluateIdle but uses a custom system prompt and model.
// Used by the interrogation system which has its own prompt and model config.
func EvaluateIdleWithPrompt(systemPrompt, userMessage, model string) (*AutoResponse, error) {
	return evaluateIdleInternal(model, systemPrompt, userMessage, 0)
}

func evaluateIdleInternal(model, systemPrompt, userMessage string, confidenceThreshold float64) (*AutoResponse, error) {
	result, err := callAnthropic(model, systemPrompt, userMessage)
	if err != nil {
		return nil, err
	}

	var resp AutoResponse
	if err := json.Unmarshal([]byte(result), &resp); err != nil {
		return nil, fmt.Errorf("parsing idle response: %w (raw: %s)", err, result)
	}

	if confidenceThreshold > 0 {
		if resp.ShouldRespond && resp.Confidence >= confidenceThreshold {
			slog.Info("Auto-responding", "confidence", fmt.Sprintf("%.0f%%", resp.Confidence*100), "message", resp.Message)
		} else if resp.ShouldRespond {
			slog.Info("Would auto-respond but confidence too low",
				"confidence", fmt.Sprintf("%.0f%%", resp.Confidence*100),
				"threshold", fmt.Sprintf("%.0f%%", confidenceThreshold*100),
				"message", resp.Message,
			)
			resp.ShouldRespond = false
		} else {
			slog.Debug("Not auto-responding", "reasoning", resp.Reasoning)
		}
	}

	return &resp, nil
}

func callAnthropic(model, systemPrompt, userMessage string) (string, error) {
	client := anthropic.NewClient()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	msg, err := client.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     model,
		MaxTokens: 1024,
		System: []anthropic.TextBlockParam{
			{Text: systemPrompt + "\n\nRespond with ONLY valid JSON, no markdown or explanation."},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(userMessage)),
		},
	})
	if err != nil {
		return "", fmt.Errorf("anthropic API error: %w", err)
	}

	// Extract text from response
	for _, block := range msg.Content {
		if block.Type == "text" {
			text := strings.TrimSpace(block.Text)
			// Strip markdown code fences if present
			text = strings.TrimPrefix(text, "```json")
			text = strings.TrimPrefix(text, "```")
			text = strings.TrimSuffix(text, "```")
			return strings.TrimSpace(text), nil
		}
	}

	return "", fmt.Errorf("no text content in anthropic response")
}

// loadPilotEnv loads env vars from ~/.pilot/.env if ANTHROPIC_API_KEY isn't already set.
func loadPilotEnv() {
	if os.Getenv("ANTHROPIC_API_KEY") != "" {
		return
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	envPath := filepath.Join(home, ".pilot", ".env")
	f, err := os.Open(envPath)
	if err != nil {
		return
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if ok {
			os.Setenv(strings.TrimSpace(key), strings.TrimSpace(val))
		}
	}
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[len(s)-maxLen:]
}
