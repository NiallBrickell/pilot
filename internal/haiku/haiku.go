package haiku

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/NiallBrickell/pilot/internal/approve"
	"github.com/NiallBrickell/pilot/internal/config"
)

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
	schema := `{"type":"object","properties":{"decision":{"type":"string","enum":["approve","deny"]},"reason":{"type":"string"}},"required":["decision","reason"]}`

	result, err := callHaiku(cfg, cfg.Prompts.Approval, userMessage, schema)
	if err != nil {
		return nil, err
	}

	decisionStr, _ := result["decision"].(string)
	reason, _ := result["reason"].(string)
	action := Deny
	if decisionStr == "approve" {
		action = Allow
	}
	slog.Info("Haiku decision", "tool", toolName, "action", decisionStr, "reason", reason)
	return &EvalDecision{Action: action, Reason: reason}, nil
}

// EvaluateIdle evaluates whether Claude's pause needs an auto-response.
func EvaluateIdle(cfg *config.PilotConfig, context string) (*AutoResponse, error) {
	userMessage := fmt.Sprintf(
		"Here is the recent Claude Code output. Claude has stopped and is showing the input prompt. Should I auto-respond?\n\n---\n%s",
		truncate(context, 4000),
	)
	schema := `{"type":"object","properties":{"should_respond":{"type":"boolean"},"message":{"type":"string"},"confidence":{"type":"number"},"reasoning":{"type":"string"}},"required":["should_respond","message","confidence","reasoning"]}`

	result, err := callHaiku(cfg, cfg.Prompts.AutoRespond, userMessage, schema)
	if err != nil {
		return nil, err
	}

	shouldRespond, _ := result["should_respond"].(bool)
	message, _ := result["message"].(string)
	confidence, _ := result["confidence"].(float64)
	reasoning, _ := result["reasoning"].(string)

	resp := &AutoResponse{
		ShouldRespond: shouldRespond,
		Message:       message,
		Confidence:    confidence,
		Reasoning:     reasoning,
	}

	if resp.ShouldRespond && resp.Confidence >= cfg.General.ConfidenceThreshold {
		slog.Info("Auto-responding", "confidence", fmt.Sprintf("%.0f%%", resp.Confidence*100), "message", resp.Message)
		return resp, nil
	} else if resp.ShouldRespond {
		slog.Info("Would auto-respond but confidence too low",
			"confidence", fmt.Sprintf("%.0f%%", resp.Confidence*100),
			"threshold", fmt.Sprintf("%.0f%%", cfg.General.ConfidenceThreshold*100),
			"message", resp.Message,
		)
		resp.ShouldRespond = false
		return resp, nil
	}

	slog.Debug("Not auto-responding", "reasoning", resp.Reasoning)
	return resp, nil
}

func callHaiku(cfg *config.PilotConfig, systemPrompt, userMessage, jsonSchema string) (map[string]any, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "claude",
		"-p",
		"--model", cfg.General.Model,
		"--output-format", "json",
		"--json-schema", jsonSchema,
		"--system-prompt", systemPrompt,
		"--tools", "",
		"--no-session-persistence",
		"--settings", `{"hooks":{}}`,
	)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("creating stdin pipe: %w", err)
	}

	cmd.Stderr = io.Discard

	out, err := func() ([]byte, error) {
		go func() {
			io.WriteString(stdin, userMessage)
			stdin.Close()
		}()
		return cmd.Output()
	}()

	if ctx.Err() == context.DeadlineExceeded {
		return nil, fmt.Errorf("claude -p timed out after 30s")
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return nil, fmt.Errorf("claude -p failed (%d): %s", exitErr.ExitCode(), string(exitErr.Stderr))
		}
		return nil, fmt.Errorf("claude -p failed: %w", err)
	}

	raw := strings.TrimSpace(string(out))
	var envelope map[string]any
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	structured, ok := envelope["structured_output"]
	if !ok {
		return nil, fmt.Errorf("no structured_output in response: %s", raw)
	}

	result, ok := structured.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("structured_output is not an object: %s", raw)
	}

	return result, nil
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[len(s)-maxLen:]
}
