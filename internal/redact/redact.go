// Package redact strips secret material out of free-form text before pilot
// persists it anywhere durable — the pilot.db actions/logs tables, and any
// fixture drawn from them. A real approval session once carried an inline
// Postgres password all the way from a Bash tool call into a committed replay
// fixture, leaking a live credential into a public repo. The fix is to make
// redaction a mandatory pass at every write chokepoint (see internal/state):
// command text is never trusted to be secret-free, so we scrub it on the way in.
//
// Redaction preserves structure — it replaces only the secret token, leaving the
// surrounding command intact — so the recorded action stays meaningful for the
// dashboard, the replay corpus, and the human reading it back.
package redact

import "regexp"

// Placeholder is what every redacted secret collapses to. It is chosen so that
// it never itself matches any secret pattern here or in the regression fence
// (secret_fence_test.go): the '*' characters fall outside every value class.
const Placeholder = "***REDACTED***"

var (
	// envSecretRe matches `NAME=value` shell/env assignments where NAME denotes
	// a credential (PGPASSWORD, or anything ending in _PASSWORD/_TOKEN/_KEY/
	// _SECRET). The value may be single-quoted, double-quoted, or bare; the
	// quotes and the name are preserved, only the value is scrubbed.
	envSecretRe = regexp.MustCompile(`(?i)([A-Za-z0-9_]*(?:PASSWORD|TOKEN|SECRET|KEY))=(['"]?)([^\s'"]+)(['"]?)`)

	// connStringRe matches an inline password in a database/broker connection
	// string: scheme://user:PASSWORD@host. Only the password segment is scrubbed
	// so the scheme, user, and host stay readable.
	connStringRe = regexp.MustCompile(`(?i)\b(postgres(?:ql)?|mysql|redis|mongodb|amqp)://([^:/?#\s]+):([^@\s]+)@`)

	// bearerRe matches a bearer token, with or without the Authorization: prefix.
	bearerRe = regexp.MustCompile(`(?i)((?:Authorization:\s*)?Bearer\s+)([A-Za-z0-9._~+/-]+=*)`)

	// apiKeyRe matches common provider key shapes that are secret in their
	// entirety: OpenAI (sk-), GitHub (ghp_/gho_/ghu_/ghs_/ghr_), Slack
	// (xoxb-/xoxa-/xoxp-/xoxr-/xoxs-), and AWS access keys (AKIA...).
	apiKeyRe = regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|xox[baprs]-[A-Za-z0-9-]{10,}|AKIA[0-9A-Z]{12,}`)

	// privateKeyRe matches a PEM private key block in full.
	privateKeyRe = regexp.MustCompile(`(?s)-----BEGIN[ A-Z0-9]*PRIVATE KEY-----.*?-----END[ A-Z0-9]*PRIVATE KEY-----`)
)

// shellRef reports whether a captured value is a shell reference ($VAR,
// ${VAR}, $(cmd)) rather than a literal secret. We leave references alone: they
// carry no credential material and scrubbing them only obscures the command.
func shellRef(v string) bool {
	return len(v) > 0 && v[0] == '$'
}

// Secrets returns s with every recognised secret replaced by Placeholder,
// preserving the surrounding structure. It is safe to call on any text and is
// idempotent: running it twice yields the same result.
func Secrets(s string) string {
	// PEM blocks first — they span newlines and would otherwise be partially
	// eaten by the line-oriented patterns below.
	s = privateKeyRe.ReplaceAllString(s, Placeholder)

	s = apiKeyRe.ReplaceAllString(s, Placeholder)

	s = envSecretRe.ReplaceAllStringFunc(s, func(m string) string {
		g := envSecretRe.FindStringSubmatch(m)
		if shellRef(g[3]) {
			return m
		}
		return g[1] + "=" + g[2] + Placeholder + g[4]
	})

	s = connStringRe.ReplaceAllStringFunc(s, func(m string) string {
		g := connStringRe.FindStringSubmatch(m)
		if shellRef(g[3]) {
			return m
		}
		return g[1] + "://" + g[2] + ":" + Placeholder + "@"
	})

	s = bearerRe.ReplaceAllStringFunc(s, func(m string) string {
		g := bearerRe.FindStringSubmatch(m)
		if shellRef(g[2]) {
			return m
		}
		return g[1] + Placeholder
	})

	return s
}
