// Package anthropic provides a lightweight client for the Anthropic Messages API.
// It calls the Anthropic Messages API directly for fast evaluations.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"
)

// Client calls the Anthropic Messages API directly.
type Client struct {
	apiKey     string
	httpClient *http.Client
}

// ApprovalDecision represents the outcome of an approval evaluation.
type ApprovalDecision int

const (
	Approve ApprovalDecision = iota
	Deny
)

// ApprovalResult is the structured output from an approval evaluation.
type ApprovalResult struct {
	Decision ApprovalDecision
	Reason   string
}

// IdleResult is the structured output from an idle/interrogation evaluation.
type IdleResult struct {
	ShouldRespond bool    `json:"should_respond"`
	Message       string  `json:"message"`
	Confidence    float64 `json:"confidence"`
	Reasoning     string  `json:"reasoning"`
}

// NewClient creates an Anthropic API client.
// It resolves the API key from the environment or from envFilePath (typically ~/.pilot/.env).
func NewClient(timeout time.Duration, envFilePath string) (*Client, error) {
	apiKey := os.Getenv("ANTHROPIC_API_KEY")
	if apiKey == "" {
		apiKey = loadKeyFromEnvFile(envFilePath)
	}
	if apiKey == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY not set (checked env and %s)", envFilePath)
	}
	return &Client{
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}, nil
}

// EvaluateApproval asks the model whether to approve or deny a tool call.
func (c *Client) EvaluateApproval(ctx context.Context, systemPrompt, toolName, toolInput, model string) (*ApprovalResult, error) {
	if model == "" {
		model = "claude-haiku-4-5"
	}
	userContent := fmt.Sprintf("Tool: %s\nInput: %s", toolName, truncate(toolInput, 2000))

	raw, err := c.call(ctx, model, systemPrompt, userContent, approvalSchema)
	if err != nil {
		slog.Warn("Anthropic API error (approval)", "error", err)
		return &ApprovalResult{Decision: Deny, Reason: fmt.Sprintf("error: %v", err)}, nil
	}

	var parsed struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return &ApprovalResult{Decision: Deny, Reason: fmt.Sprintf("parse error: %v", err)}, nil
	}

	decision := Deny
	if parsed.Decision == "approve" {
		decision = Approve
	}
	return &ApprovalResult{Decision: decision, Reason: parsed.Reason}, nil
}

// EvaluateIdle asks the model whether Claude's pause needs an auto-response.
func (c *Client) EvaluateIdle(ctx context.Context, systemPrompt, transcriptContext, model string) (*IdleResult, error) {
	if model == "" {
		model = "claude-haiku-4-5"
	}
	userContent := "Here is the recent Claude Code conversation. Claude has stopped. Should I auto-respond?\n\n---\n" + truncate(transcriptContext, 4000)

	raw, err := c.call(ctx, model, systemPrompt, userContent, idleSchema)
	if err != nil {
		slog.Warn("Anthropic API error (idle)", "error", err)
		return &IdleResult{ShouldRespond: false, Confidence: 0, Reasoning: fmt.Sprintf("error: %v", err)}, nil
	}

	var result IdleResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return &IdleResult{ShouldRespond: false, Confidence: 0, Reasoning: fmt.Sprintf("parse error: %v", err)}, nil
	}
	return &result, nil
}

// call sends a single messages.create request and returns the raw JSON text content.
func (c *Client) call(ctx context.Context, model, systemPrompt, userContent string, schema map[string]any) (json.RawMessage, error) {
	body := map[string]any{
		"model":      model,
		"max_tokens": 512,
		"system":     systemPrompt,
		"messages":   []map[string]string{{"role": "user", "content": userContent}},
		"output_config": map[string]any{
			"format": schema,
		},
	}

	reqBody, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.anthropic.com/v1/messages", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("API call failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned %d: %s", resp.StatusCode, truncate(string(respBody), 500))
	}

	var apiResp struct {
		Content []struct {
			Type string          `json:"type"`
			Text json.RawMessage `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, fmt.Errorf("parse API response: %w", err)
	}

	if len(apiResp.Content) == 0 {
		return nil, fmt.Errorf("empty content in API response")
	}

	// The text field is a JSON string — we need to unquote it first.
	var textStr string
	if err := json.Unmarshal(apiResp.Content[0].Text, &textStr); err != nil {
		// It might already be raw JSON (not quoted), try using it directly
		return apiResp.Content[0].Text, nil
	}
	return json.RawMessage(textStr), nil
}

// Schemas for structured output.
var approvalSchema = map[string]any{
	"type": "json_schema",
	"schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"decision": map[string]any{"type": "string", "enum": []string{"approve", "deny"}},
			"reason":   map[string]any{"type": "string"},
		},
		"required":             []string{"decision", "reason"},
		"additionalProperties": false,
	},
}

var idleSchema = map[string]any{
	"type": "json_schema",
	"schema": map[string]any{
		"type": "object",
		"properties": map[string]any{
			"should_respond": map[string]any{"type": "boolean"},
			"message":        map[string]any{"type": "string"},
			"confidence":     map[string]any{"type": "number"},
			"reasoning":      map[string]any{"type": "string"},
		},
		"required":             []string{"should_respond", "message", "confidence", "reasoning"},
		"additionalProperties": false,
	},
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

// loadKeyFromEnvFile parses a simple .env file for ANTHROPIC_API_KEY.
func loadKeyFromEnvFile(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eqIdx := strings.Index(line, "=")
		if eqIdx == -1 {
			continue
		}
		key := line[:eqIdx]
		val := line[eqIdx+1:]
		if key == "ANTHROPIC_API_KEY" {
			return val
		}
	}
	return ""
}
