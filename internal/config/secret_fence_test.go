package config_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// This is a regression fence, not the redactor. The redactor (internal/redact)
// scrubs aggressively at capture time and is value-agnostic — it will happily
// redact PGPASSWORD='x'. This fence instead scans the committed source tree and
// fails only on material that *looks like a live secret*: a high-entropy value
// in a credential-shaped position. That keeps intentional placeholders (u:p@,
// $TOKEN, PGPASSWORD='x') legal in fixtures while ensuring a real credential —
// like the inline Postgres password that once leaked here — can never be
// committed again unnoticed.
//
// The redaction package's own tests are the sanctioned home for realistic
// secret-shaped literals, so they are exempt.
var fenceExempt = map[string]bool{
	"internal/redact/redact.go":            true,
	"internal/redact/redact_test.go":       true,
	"internal/state/state_redact_test.go":  true,
	"internal/config/secret_fence_test.go": true,
}

var fencePatterns = []struct {
	name string
	re   *regexp.Regexp
}{
	{"inline connection-string password", regexp.MustCompile(`(?i)\b(?:postgres(?:ql)?|mysql|redis|mongodb|amqp)://[^:/@\s]+:([A-Za-z0-9/+._~-]{12,})@`)},
	{"credential env assignment", regexp.MustCompile(`(?i)[A-Za-z0-9_]*(?:PASSWORD|TOKEN|SECRET|KEY)=['"]?([A-Za-z0-9/+._-]{16,})`)},
	{"bearer token", regexp.MustCompile(`(?i)Bearer\s+([A-Za-z0-9._~+/-]{16,})`)},
	{"openai key", regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`)},
	{"github token", regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}`)},
	{"slack token", regexp.MustCompile(`xox[baprs]-[A-Za-z0-9-]{10,}`)},
	{"aws access key", regexp.MustCompile(`AKIA[0-9A-Z]{12,}`)},
	{"pem private key", regexp.MustCompile(`-----BEGIN[ A-Z0-9]*PRIVATE KEY-----`)},
}

// repoRoot walks up from the test's working directory to the directory holding
// the .git folder.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not locate repo root (.git) from %s", dir)
		}
		dir = parent
	}
}

// TestNoCommittedSecrets walks every committed Go file and fails if any
// real-looking secret survives. It is the last line of defence behind the
// capture-time redactor: even a hand-edited fixture is caught here.
func TestNoCommittedSecrets(t *testing.T) {
	root := repoRoot(t)

	var findings []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			switch info.Name() {
			case ".git", "node_modules", "dist", "build":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if fenceExempt[rel] {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, p := range fencePatterns {
			for _, m := range p.re.FindAllString(string(data), -1) {
				findings = append(findings, rel+": "+p.name+": "+m)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}

	if len(findings) > 0 {
		t.Fatalf("committed secret material found (redact before committing):\n  %s",
			strings.Join(findings, "\n  "))
	}
}
