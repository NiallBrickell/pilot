// Package anthropic provides a lightweight client for the Anthropic Messages API.
// It calls the Anthropic Messages API directly for fast evaluations.
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const messagesEndpoint = "https://api.anthropic.com/v1/messages"

var (
	lowBalanceMu       sync.Mutex
	lowBalanceLastWarn time.Time
)

// warnLowBalance emits a prominent, throttled warning when the evaluator's
// Anthropic credits are exhausted. With no credits every evaluation fails and
// pilot falls through to asking the user about everything, so this needs to be
// loud and distinct rather than buried among per-call warnings.
func warnLowBalance() {
	lowBalanceMu.Lock()
	defer lowBalanceMu.Unlock()
	if !lowBalanceLastWarn.IsZero() && time.Since(lowBalanceLastWarn) < 5*time.Minute {
		return
	}
	lowBalanceLastWarn = time.Now()
	slog.Error("PILOT EVALUATOR DISABLED: Anthropic credit balance too low — routine calls still use deterministic approval, while residual danger/ambiguity calls fall through to a manual prompt. Top up at https://console.anthropic.com/settings/billing to restore evaluator judgments.")
}

// Client calls the Anthropic Messages API directly.
type Client struct {
	apiKey         string
	httpClient     *http.Client
	endpoint       string
	billingCircuit atomic.Bool
}

// APIError is Anthropic's standard non-2xx response. Keeping status, type and
// request ID structured lets callers distinguish capacity from malformed
// requests without brittle substring matching.
type APIError struct {
	StatusCode  int
	Type        string
	Message     string
	RequestID   string
	RetryAfter  time.Duration
	CircuitOpen bool
}

func (e *APIError) Error() string {
	detail := e.Message
	if detail == "" {
		detail = http.StatusText(e.StatusCode)
	}
	if e.Type != "" {
		detail = e.Type + ": " + detail
	}
	if e.RequestID != "" {
		detail += " (request_id=" + e.RequestID + ")"
	}
	return fmt.Sprintf("Anthropic API returned %d: %s", e.StatusCode, detail)
}

// TransportError is a request that failed before Anthropic returned an HTTP
// response. Connection failures and timeouts are retryable; caller
// cancellation is not.
type TransportError struct {
	Err error
}

func (e *TransportError) Error() string { return "Anthropic API call failed: " + e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// IsBillingError reports errors that cannot recover without changing account
// credit. Anthropic's current contract uses 402/billing_error; the legacy 400
// low-balance text remains recognized so an older edge response cannot create
// another request storm.
func IsBillingError(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode == http.StatusPaymentRequired ||
		apiErr.Type == "billing_error" ||
		strings.Contains(strings.ToLower(apiErr.Message), "credit balance is too low")
}

// IsRetryable follows Anthropic's error contract: connection failures,
// 408/409/429 and 5xx may be retried. Billing, auth, validation and other 4xx
// responses are terminal for the request.
func IsRetryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || IsBillingError(err) {
		return false
	}
	var transportErr *TransportError
	if errors.As(err, &transportErr) {
		return true
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode == http.StatusRequestTimeout ||
		apiErr.StatusCode == http.StatusConflict ||
		apiErr.StatusCode == http.StatusTooManyRequests ||
		apiErr.StatusCode >= http.StatusInternalServerError
}

// RetryDelay returns Anthropic's Retry-After delay when one was supplied.
func RetryDelay(err error) time.Duration {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.RetryAfter
	}
	return 0
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
	Usage    Usage
}

// IdleResult is the structured output from an idle/interrogation evaluation.
type IdleResult struct {
	ShouldRespond bool    `json:"should_respond"`
	Message       string  `json:"message"`
	Confidence    float64 `json:"confidence"`
	Reasoning     string  `json:"reasoning"`
	Usage         Usage   `json:"-"`
}

type Usage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// TotalInputTokens includes ordinary and cached prompt tokens for observability.
// Pricing still treats the three categories separately.
func (u Usage) TotalInputTokens() int {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

// ResolveAPIKey returns the Anthropic API key from the environment or from
// envFilePath (typically ~/.pilot/.env). Returns "" if neither is set.
func ResolveAPIKey(envFilePath string) string {
	if k := os.Getenv("ANTHROPIC_API_KEY"); k != "" {
		return k
	}
	return loadKeyFromEnvFile(envFilePath)
}

// NewClient creates an Anthropic API client.
// It resolves the API key from the environment or from envFilePath (typically ~/.pilot/.env).
func NewClient(timeout time.Duration, envFilePath string) (*Client, error) {
	apiKey := ResolveAPIKey(envFilePath)
	if apiKey == "" {
		return nil, fmt.Errorf("ANTHROPIC_API_KEY not set (checked env and %s)", envFilePath)
	}
	return &Client{
		apiKey: apiKey,
		httpClient: &http.Client{
			Timeout: timeout,
		},
		endpoint: messagesEndpoint,
	}, nil
}

// EvaluateApproval asks the model whether to approve or deny a tool call.
func (c *Client) EvaluateApproval(ctx context.Context, systemPrompt, toolName, toolInput, model string) (*ApprovalResult, error) {
	if model == "" {
		model = "claude-haiku-4-5"
	}
	userContent := fmt.Sprintf("Tool: %s\nInput: %s", toolName, truncate(toolInput, 2000))

	raw, usage, err := c.call(ctx, model, systemPrompt, userContent, approvalSchema)
	if err != nil {
		// Infra failure, not a verdict — the caller decides the fallback.
		// Returning a fabricated Deny here used to block sessions with the raw
		// error text as the escalation reason.
		return nil, err
	}

	var parsed struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("parse approval response: %w", err)
	}

	decision := Deny
	if parsed.Decision == "approve" {
		decision = Approve
	}
	return &ApprovalResult{Decision: decision, Reason: parsed.Reason, Usage: usage}, nil
}

// EvaluateIdle asks the model whether Claude's pause needs an auto-response.
func (c *Client) EvaluateIdle(ctx context.Context, systemPrompt, transcriptContext, model string) (*IdleResult, error) {
	if model == "" {
		model = "claude-haiku-4-5"
	}
	userContent := "Here is the recent Claude Code conversation. Claude has stopped. Should I auto-respond?\n\n---\n" + truncate(transcriptContext, 4000)

	raw, usage, err := c.call(ctx, model, systemPrompt, userContent, idleSchema)
	if err != nil {
		slog.Warn("Anthropic API error (idle)", "error", err)
		return &IdleResult{ShouldRespond: false, Confidence: 0, Reasoning: fmt.Sprintf("error: %v", err)}, nil
	}

	var result IdleResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return &IdleResult{ShouldRespond: false, Confidence: 0, Reasoning: fmt.Sprintf("parse error: %v", err), Usage: usage}, nil
	}
	result.Usage = usage
	return &result, nil
}

// call sends a single messages.create request and returns the raw JSON text content.
func (c *Client) call(ctx context.Context, model, systemPrompt, userContent string, schema map[string]any) (json.RawMessage, Usage, error) {
	if c.billingCircuit.Load() {
		return nil, Usage{}, &APIError{
			StatusCode:  http.StatusPaymentRequired,
			Type:        "billing_error",
			Message:     "evaluator billing circuit is open after an exhausted-credit response",
			CircuitOpen: true,
		}
	}
	body := map[string]any{
		"model":       model,
		"max_tokens":  512,
		"temperature": 0, // deterministic decisions — no sampling flapping on borderline calls
		"system":      systemPrompt,
		"messages":    []map[string]string{{"role": "user", "content": userContent}},
		// Current Messages API automatic caching. Anthropic silently skips
		// caching when a model's minimum cacheable prefix is not met; response
		// usage telemetry is the authority on whether this request hit the cache.
		"cache_control": map[string]string{"type": "ephemeral"},
		"output_config": map[string]any{
			"format": schema,
		},
	}

	reqBody, err := json.Marshal(body)
	if err != nil {
		return nil, Usage{}, fmt.Errorf("marshal request: %w", err)
	}

	endpoint := c.endpoint
	if endpoint == "" {
		endpoint = messagesEndpoint
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
	if err != nil {
		return nil, Usage{}, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("x-api-key", c.apiKey)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, Usage{}, &TransportError{Err: err}
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, Usage{}, &TransportError{Err: fmt.Errorf("read response: %w", err)}
	}

	if resp.StatusCode != http.StatusOK {
		apiErr := parseAPIError(resp.StatusCode, resp.Header, respBody)
		if IsBillingError(apiErr) {
			c.billingCircuit.Store(true)
			warnLowBalance()
		}
		return nil, Usage{}, apiErr
	}

	var apiResp struct {
		Content []struct {
			Type string          `json:"type"`
			Text json.RawMessage `json:"text"`
		} `json:"content"`
		Usage Usage `json:"usage"`
	}
	if err := json.Unmarshal(respBody, &apiResp); err != nil {
		return nil, Usage{}, fmt.Errorf("parse API response: %w", err)
	}

	if len(apiResp.Content) == 0 {
		return nil, apiResp.Usage, fmt.Errorf("empty content in API response")
	}

	// The text field is a JSON string — we need to unquote it first.
	var textStr string
	if err := json.Unmarshal(apiResp.Content[0].Text, &textStr); err != nil {
		// It might already be raw JSON (not quoted), try using it directly
		return apiResp.Content[0].Text, apiResp.Usage, nil
	}
	return json.RawMessage(textStr), apiResp.Usage, nil
}

func parseAPIError(statusCode int, header http.Header, body []byte) *APIError {
	var envelope struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
		RequestID string `json:"request_id"`
	}
	_ = json.Unmarshal(body, &envelope)
	message := envelope.Error.Message
	if message == "" {
		message = truncate(strings.TrimSpace(string(body)), 500)
	}
	return &APIError{
		StatusCode: statusCode,
		Type:       envelope.Error.Type,
		Message:    message,
		RequestID:  envelope.RequestID,
		RetryAfter: parseRetryAfter(header.Get("Retry-After")),
	}
}

func parseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		return max(time.Until(at), 0)
	}
	return 0
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
