package redact

import (
	"strings"
	"testing"
)

// All secret values below are obviously-fake placeholders invented for the
// test. This file is the one sanctioned home for secret-shaped literals — it is
// excluded from the committed-secret fence (secret_fence_test.go) precisely so
// the redactor can be exercised against realistic shapes.

func TestSecrets(t *testing.T) {
	cases := []struct {
		name string
		in   string
		// want is the exact expected output. wantContains, when set, is checked
		// as a substring instead (for long/PEM inputs).
		want         string
		wantContains []string
		wantAbsent   []string
	}{
		{
			// The exact leak shape that reached a public commit: an inline
			// PGPASSWORD alongside a connection string.
			name:       "the leak shape",
			in:         `PGPASSWORD='abc' psql "postgresql://u:p@1.2.3.4:5432/db"`,
			want:       `PGPASSWORD='***REDACTED***' psql "postgresql://u:***REDACTED***@1.2.3.4:5432/db"`,
			wantAbsent: []string{"'abc'", ":p@"},
		},
		{
			name: "pgpassword bare",
			in:   `PGPASSWORD=s3cr3tvalue psql`,
			want: `PGPASSWORD=***REDACTED*** psql`,
		},
		{
			name: "generic password env",
			in:   `MYSQL_PASSWORD="hunter2hunter2" mysql`,
			want: `MYSQL_PASSWORD="***REDACTED***" mysql`,
		},
		{
			name: "token env",
			in:   `export GITHUB_TOKEN=ghtoken_literalvalue123`,
			want: `export GITHUB_TOKEN=***REDACTED***`,
		},
		{
			name: "api key env",
			in:   `ACME_API_KEY=acme_livekey_00998877665544332211 acme run`,
			want: `ACME_API_KEY=***REDACTED*** acme run`,
		},
		{
			name: "secret env",
			in:   `CLIENT_SECRET='shhhh-do-not-tell-anyone'`,
			want: `CLIENT_SECRET='***REDACTED***'`,
		},
		{
			name: "shell ref not redacted",
			in:   `GH_TOKEN=$TOKEN gh pr view 1`,
			want: `GH_TOKEN=$TOKEN gh pr view 1`,
		},
		{
			name: "command-substitution ref not redacted",
			in:   `API_TOKEN=$(cat ~/.token) curl x`,
			want: `API_TOKEN=$(cat ~/.token) curl x`,
		},
		{
			name: "postgres conn string with user and password",
			in:   `psql "postgresql://claude_code:wLraSecretPass2xKpq7@203.0.113.71:5432/db?sslmode=require"`,
			want: `psql "postgresql://claude_code:***REDACTED***@203.0.113.71:5432/db?sslmode=require"`,
		},
		{
			name: "redis conn string",
			in:   `redis-cli -u redis://default:topsecretpassword@10.0.0.5:6379`,
			want: `redis-cli -u redis://default:***REDACTED***@10.0.0.5:6379`,
		},
		{
			name: "mongodb conn string",
			in:   `mongosh "mongodb://admin:letmein12345@db:27017/app"`,
			want: `mongosh "mongodb://admin:***REDACTED***@db:27017/app"`,
		},
		{
			name: "amqp conn string",
			in:   `amqp://guest:guestpassword123@rabbit:5672/`,
			want: `amqp://guest:***REDACTED***@rabbit:5672/`,
		},
		{
			name: "authorization bearer header",
			in:   `curl -H "Authorization: Bearer phx9fJ2kLmQverysecret" https://x`,
			want: `curl -H "Authorization: Bearer ***REDACTED***" https://x`,
		},
		{
			name: "bare bearer",
			in:   `Bearer eyJhbGcitsatokenvalue.payload.sig`,
			want: `Bearer ***REDACTED***`,
		},
		{
			name: "bearer shell ref not redacted",
			in:   `-H "Authorization: Bearer $TOKEN"`,
			want: `-H "Authorization: Bearer $TOKEN"`,
		},
		{
			name: "openai key",
			in:   `OPENAI="sk-proj-abcdef0123456789abcdef0123456789"`,
			// Env rule fires first on KEY? no — name is OPENAI, so only the
			// sk- shape matches.
			want: `OPENAI="***REDACTED***"`,
		},
		{
			name: "github pat",
			in:   `git remote add o https://ghp_0123456789abcdefghij0123456789abcdef@github.com/o/r`,
			want: `git remote add o https://***REDACTED***@github.com/o/r`,
		},
		{
			name: "slack token",
			in:   `SLACK=xoxb-fake-slack-token-placeholder-value`,
			want: `SLACK=***REDACTED***`,
		},
		{
			name: "aws access key",
			in:   `aws_access_key_id = AKIAIOSFODNN7EXAMPLE`,
			want: `aws_access_key_id = ***REDACTED***`,
		},
		{
			name:         "pem private key block",
			in:           "before\n-----BEGIN RSA PRIVATE KEY-----\nMIIabc123\nlines\n-----END RSA PRIVATE KEY-----\nafter",
			wantContains: []string{"before", "***REDACTED***", "after"},
			wantAbsent:   []string{"MIIabc123", "BEGIN RSA"},
		},
		{
			name: "no secret is unchanged",
			in:   `cd /tmp && ls -la && git status`,
			want: `cd /tmp && ls -la && git status`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Secrets(tc.in)
			if tc.want != "" && got != tc.want {
				t.Errorf("Secrets(%q)\n  got  %q\n  want %q", tc.in, got, tc.want)
			}
			for _, sub := range tc.wantContains {
				if !strings.Contains(got, sub) {
					t.Errorf("Secrets(%q) = %q, missing %q", tc.in, got, sub)
				}
			}
			for _, sub := range tc.wantAbsent {
				if strings.Contains(got, sub) {
					t.Errorf("Secrets(%q) = %q, still contains secret %q", tc.in, got, sub)
				}
			}
		})
	}
}

// TestSecretsIdempotent proves running redaction twice is a no-op: the
// placeholder never re-triggers a pattern.
func TestSecretsIdempotent(t *testing.T) {
	inputs := []string{
		`PGPASSWORD='abc' psql "postgresql://u:p@1.2.3.4:5432/db"`,
		`curl -H "Authorization: Bearer phx9fJ2kLmQverysecret" https://x`,
		`ACME_API_KEY=acme_livekey_00998877665544332211`,
	}
	for _, in := range inputs {
		once := Secrets(in)
		twice := Secrets(once)
		if once != twice {
			t.Errorf("not idempotent for %q:\n  once  %q\n  twice %q", in, once, twice)
		}
	}
}
