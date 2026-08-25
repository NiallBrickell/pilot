package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NiallBrickell/pilot/internal/anthropic"
	"github.com/NiallBrickell/pilot/internal/config"
)

type countingReader struct {
	reads int
}

func (r *countingReader) Read(_ []byte) (int, error) {
	r.reads++
	return 0, io.EOF
}

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

func TestHTTPServerBindsOnlyToIPv4Loopback(t *testing.T) {
	srv := &Server{port: 9721}
	httpServer := srv.newHTTPServer(http.NotFoundHandler())

	if got, want := httpServer.Addr, "127.0.0.1:9721"; got != want {
		t.Fatalf("HTTP server address = %q, want %q", got, want)
	}
}

func TestServerTokenAuthenticationRunsBeforePostHandlerAndBody(t *testing.T) {
	tests := []struct {
		name          string
		authorization string
		wantStatus    int
		wantCalls     int
	}{
		{name: "missing", wantStatus: http.StatusUnauthorized},
		{name: "wrong token", authorization: "Bearer wrong", wantStatus: http.StatusUnauthorized},
		{name: "wrong scheme", authorization: "Basic test-token", wantStatus: http.StatusUnauthorized},
		{name: "lowercase scheme", authorization: "bearer test-token", wantStatus: http.StatusUnauthorized},
		{name: "missing space", authorization: "Bearertest-token", wantStatus: http.StatusUnauthorized},
		{name: "extra whitespace", authorization: "Bearer  test-token", wantStatus: http.StatusUnauthorized},
		{name: "correct", authorization: "Bearer test-token", wantStatus: http.StatusNoContent, wantCalls: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := &Server{serverToken: "test-token"}
			calls := 0
			handler := srv.authenticateRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				_, _ = io.ReadAll(r.Body)
				w.WriteHeader(http.StatusNoContent)
			}))
			body := &countingReader{}
			req := httptest.NewRequest(http.MethodPost, "/internal/evaluate", body)
			if tt.authorization != "" {
				req.Header.Set("Authorization", tt.authorization)
			}
			res := httptest.NewRecorder()

			handler.ServeHTTP(res, req)

			if res.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", res.Code, tt.wantStatus)
			}
			if calls != tt.wantCalls {
				t.Fatalf("handler calls = %d, want %d", calls, tt.wantCalls)
			}
			if tt.wantStatus == http.StatusUnauthorized && res.Header().Get("WWW-Authenticate") != `Bearer realm="pilot"` {
				t.Fatalf("WWW-Authenticate = %q, want bearer challenge", res.Header().Get("WWW-Authenticate"))
			}
			if tt.wantCalls == 0 && body.reads != 0 {
				t.Fatalf("request body read %d times before authentication", body.reads)
			}
		})
	}
}

func TestServerTokenAuthenticationLeavesReadOnlyRequestsPublic(t *testing.T) {
	srv := &Server{serverToken: "test-token"}
	handler := srv.authenticateRequests(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	res := httptest.NewRecorder()

	handler.ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("read-only status = %d, want %d", res.Code, http.StatusNoContent)
	}
}

func TestServerTokenAuthenticationDisabledByDefault(t *testing.T) {
	srv := &Server{}
	handler := srv.authenticateRequests(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "/internal/evaluate", strings.NewReader("payload"))
	res := httptest.NewRecorder()

	handler.ServeHTTP(res, req)

	if res.Code != http.StatusNoContent {
		t.Fatalf("local-mode POST status = %d, want %d", res.Code, http.StatusNoContent)
	}
}

func TestAuthCheckChallengesOnlyInTokenMode(t *testing.T) {
	tests := []struct {
		name          string
		token         string
		authorization string
		wantStatus    int
	}{
		{name: "local mode", wantStatus: http.StatusNoContent},
		{name: "missing token", token: "test-token", wantStatus: http.StatusUnauthorized},
		{name: "wrong token", token: "test-token", authorization: "Bearer wrong", wantStatus: http.StatusUnauthorized},
		{name: "valid token", token: "test-token", authorization: "Bearer test-token", wantStatus: http.StatusNoContent},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := &Server{serverToken: tt.token}
			req := httptest.NewRequest(http.MethodGet, "/internal/auth-check", nil)
			if tt.authorization != "" {
				req.Header.Set("Authorization", tt.authorization)
			}
			res := httptest.NewRecorder()

			srv.handler().ServeHTTP(res, req)

			if res.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", res.Code, tt.wantStatus)
			}
		})
	}
}

func TestNewLoadsServerTokenFromEnvironment(t *testing.T) {
	t.Setenv("PILOT_SERVER_TOKEN", "test-token")

	if got := New(&config.PilotConfig{}).serverToken; got != "test-token" {
		t.Fatalf("server token = %q, want configured token", got)
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
