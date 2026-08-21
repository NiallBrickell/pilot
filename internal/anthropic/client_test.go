package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testClient(roundTrip roundTripFunc) *Client {
	return &Client{
		apiKey:     "test-key",
		httpClient: &http.Client{Transport: roundTrip},
		endpoint:   "https://anthropic.test/v1/messages",
	}
}

func TestCallUsesCurrentPromptCacheShapeAndParsesCacheUsage(t *testing.T) {
	var requestBody map[string]any
	client := testClient(func(r *http.Request) (*http.Response, error) {
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			return nil, err
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body: io.NopCloser(strings.NewReader(`{
			"content":[{"type":"text","text":"{\"decision\":\"approve\",\"reason\":\"safe\"}"}],
			"usage":{
				"input_tokens":11,
				"output_tokens":7,
				"cache_creation_input_tokens":101,
				"cache_read_input_tokens":303
			}
		}`)),
		}, nil
	})

	_, usage, err := client.call(context.Background(), "claude-haiku-4-5", "fixed system", "dynamic input", approvalSchema)
	if err != nil {
		t.Fatalf("call: %v", err)
	}

	cacheControl, ok := requestBody["cache_control"].(map[string]any)
	if !ok || cacheControl["type"] != "ephemeral" {
		t.Fatalf("cache_control = %#v, want top-level ephemeral cache", requestBody["cache_control"])
	}
	if requestBody["system"] != "fixed system" {
		t.Fatalf("system = %#v", requestBody["system"])
	}
	if usage.InputTokens != 11 || usage.OutputTokens != 7 ||
		usage.CacheCreationInputTokens != 101 || usage.CacheReadInputTokens != 303 {
		t.Fatalf("usage = %#v", usage)
	}
	if usage.TotalInputTokens() != 415 {
		t.Fatalf("TotalInputTokens = %d, want 415", usage.TotalInputTokens())
	}
}

func TestBillingErrorOpensCircuitAndSkipsFutureHTTP(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{
			name:   "current billing error",
			status: http.StatusPaymentRequired,
			body:   `{"error":{"type":"billing_error","message":"credit exhausted"},"request_id":"req_test"}`,
		},
		{
			name:   "legacy low balance response",
			status: http.StatusBadRequest,
			body:   `{"error":{"type":"invalid_request_error","message":"Your credit balance is too low to access the Anthropic API."},"request_id":"req_legacy"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			client := testClient(func(r *http.Request) (*http.Response, error) {
				requests.Add(1)
				return &http.Response{
					StatusCode: tc.status,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(tc.body)),
				}, nil
			})
			_, _, firstErr := client.call(context.Background(), "claude-haiku-4-5", "system", "input", approvalSchema)
			if !IsBillingError(firstErr) || IsRetryable(firstErr) {
				t.Fatalf("first error billing=%v retryable=%v: %v", IsBillingError(firstErr), IsRetryable(firstErr), firstErr)
			}
			var apiErr *APIError
			if !errors.As(firstErr, &apiErr) || apiErr.RequestID == "" {
				t.Fatalf("first error was not structured: %#v", firstErr)
			}

			_, _, secondErr := client.call(context.Background(), "claude-haiku-4-5", "system", "input", approvalSchema)
			if !IsBillingError(secondErr) {
				t.Fatalf("circuit error = %v, want billing error", secondErr)
			}
			if !errors.As(secondErr, &apiErr) || !apiErr.CircuitOpen {
				t.Fatalf("second error = %#v, want open circuit", secondErr)
			}
			if got := requests.Load(); got != 1 {
				t.Fatalf("HTTP requests = %d, want 1", got)
			}
		})
	}
}

func TestAPIErrorRetryClassification(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"timeout", &TransportError{Err: context.DeadlineExceeded}, true},
		{"caller cancellation", &TransportError{Err: context.Canceled}, false},
		{"request timeout", &APIError{StatusCode: http.StatusRequestTimeout}, true},
		{"conflict", &APIError{StatusCode: http.StatusConflict}, true},
		{"rate limit", &APIError{StatusCode: http.StatusTooManyRequests}, true},
		{"server error", &APIError{StatusCode: http.StatusBadGateway}, true},
		{"bad request", &APIError{StatusCode: http.StatusBadRequest}, false},
		{"auth", &APIError{StatusCode: http.StatusUnauthorized}, false},
		{"billing", &APIError{StatusCode: http.StatusPaymentRequired, Type: "billing_error"}, false},
		{"parse failure", errors.New("parse response"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRetryable(tc.err); got != tc.want {
				t.Fatalf("IsRetryable(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestParseAPIErrorCapturesContractAndRetryAfter(t *testing.T) {
	header := make(http.Header)
	header.Set("Retry-After", "3")
	err := parseAPIError(
		http.StatusTooManyRequests,
		header,
		[]byte(`{"error":{"type":"rate_limit_error","message":"slow down"},"request_id":"req_123"}`),
	)
	if err.StatusCode != http.StatusTooManyRequests || err.Type != "rate_limit_error" ||
		err.Message != "slow down" || err.RequestID != "req_123" || err.RetryAfter != 3*time.Second {
		t.Fatalf("parsed error = %#v", err)
	}
}
