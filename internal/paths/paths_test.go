package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReplacePromptsSection_PreservesUserGeneral(t *testing.T) {
	user := `# pilot configuration
[general]
  sse_port = 9999
  grace_period_s = 5.0

[prompts]
  approval = "old approval"
  auto_respond = "old auto_respond"
`
	embedded := `[general]
sse_port = 9721

[prompts]
approval = """
new approval prompt
"""
auto_respond = """
new auto_respond
"""
`
	out, err := replacePromptsSection(user, embedded)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "sse_port = 9999") {
		t.Fatalf("user general settings lost:\n%s", out)
	}
	if !strings.Contains(out, "grace_period_s = 5.0") {
		t.Fatalf("user general settings lost:\n%s", out)
	}
	if strings.Contains(out, "old approval") || strings.Contains(out, "old auto_respond") {
		t.Fatalf("old prompts still present:\n%s", out)
	}
	if !strings.Contains(out, "new approval prompt") || !strings.Contains(out, "new auto_respond") {
		t.Fatalf("new prompts missing:\n%s", out)
	}
}

func TestReplacePromptsSection_PreservesSectionsAfterPrompts(t *testing.T) {
	user := `[general]
sse_port = 9999

[prompts]
approval = "old"
auto_respond = "old"

[[webhooks]]
url = "http://localhost:8080/hook"
events = ["action"]
`
	embedded := `[prompts]
approval = "new"
auto_respond = "new"
`
	out, err := replacePromptsSection(user, embedded)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `url = "http://localhost:8080/hook"`) {
		t.Fatalf("webhook lost:\n%s", out)
	}
	if !strings.Contains(out, `events = ["action"]`) {
		t.Fatalf("webhook events lost:\n%s", out)
	}
}

func TestReplacePromptsSection_IgnoresBracketsInTripleQuotedStrings(t *testing.T) {
	user := `[general]
sse_port = 9999

[prompts]
approval = """
This is a prompt.
[general] this looks like a section but isn't
[webhooks] neither is this
"""
auto_respond = "old"
`
	embedded := `[prompts]
approval = "new"
auto_respond = "new"
`
	out, err := replacePromptsSection(user, embedded)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "sse_port = 9999") {
		t.Fatalf("user general settings lost:\n%s", out)
	}
	if strings.Contains(out, "this looks like a section") {
		t.Fatalf("old prompt not replaced:\n%s", out)
	}
}

func TestReplacePromptsSection_AppendsWhenMissing(t *testing.T) {
	user := `[general]
sse_port = 9999
`
	embedded := `[prompts]
approval = "new"
auto_respond = "new"
`
	out, err := replacePromptsSection(user, embedded)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "sse_port = 9999") {
		t.Fatalf("user general settings lost:\n%s", out)
	}
	if !strings.Contains(out, `approval = "new"`) {
		t.Fatalf("embedded prompts not appended:\n%s", out)
	}
}

func TestUpgradeDefaults_BootstrapThenUpgrade(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PILOT_HOME", dir)

	oldEmbedded := `[general]
sse_port = 9721

[prompts]
approval = """
old approval
"""
auto_respond = """
old auto_respond
"""
`
	// User installs with the old defaults.
	if err := EnsureSetup(oldEmbedded); err != nil {
		t.Fatal(err)
	}
	// User edits general settings (but not prompts).
	userCustomised := strings.Replace(oldEmbedded, "sse_port = 9721", "sse_port = 9999", 1)
	if err := os.WriteFile(ConfigFile(), []byte(userCustomised), 0644); err != nil {
		t.Fatal(err)
	}
	// Bootstrap the baseline as if this user pre-dates the upgrade feature.
	_ = os.Remove(PromptBaselineFile())

	// First upgrade call — bootstraps baseline.
	res, err := UpgradeDefaults(oldEmbedded)
	if err != nil {
		t.Fatal(err)
	}
	if res.Upgraded || res.Reason != "bootstrapped" {
		t.Fatalf("expected bootstrap, got %+v", res)
	}

	// New default ships. Prompts change; general too.
	newEmbedded := `[general]
sse_port = 9721
max_concurrent_evals = 8

[prompts]
approval = """
new approval
"""
auto_respond = """
new auto_respond
"""
`
	res, err = UpgradeDefaults(newEmbedded)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Upgraded {
		t.Fatalf("expected upgrade, got %+v", res)
	}

	// Verify: prompts updated, user's sse_port preserved.
	out, err := os.ReadFile(ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "sse_port = 9999") {
		t.Fatalf("user's sse_port overwritten:\n%s", s)
	}
	if !strings.Contains(s, "new approval") {
		t.Fatalf("prompts not upgraded:\n%s", s)
	}
	if strings.Contains(s, "old approval") {
		t.Fatalf("old prompts still present:\n%s", s)
	}

	// Backup exists.
	matches, _ := filepath.Glob(filepath.Join(dir, "pilot.toml.pre-upgrade-*.bak"))
	if len(matches) == 0 {
		t.Fatalf("no backup file written")
	}
}

func TestUpgradeDefaults_SkipsWhenUserEditedPrompts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PILOT_HOME", dir)

	embedded := `[prompts]
approval = "v1"
auto_respond = "v1"
`
	if err := EnsureSetup(embedded); err != nil {
		t.Fatal(err)
	}
	// User edits the prompts.
	customised := `[prompts]
approval = "MY CUSTOM PROMPT"
auto_respond = "v1"
`
	if err := os.WriteFile(ConfigFile(), []byte(customised), 0644); err != nil {
		t.Fatal(err)
	}

	// Embedded ships a new version.
	newEmbedded := `[prompts]
approval = "v2"
auto_respond = "v2"
`
	res, err := UpgradeDefaults(newEmbedded)
	if err != nil {
		t.Fatal(err)
	}
	if res.Upgraded {
		t.Fatalf("should not upgrade when user customised prompts, got %+v", res)
	}
	if res.Reason != "user_customised" {
		t.Fatalf("expected user_customised reason, got %q", res.Reason)
	}

	// File still has the user's custom prompt.
	out, _ := os.ReadFile(ConfigFile())
	if !strings.Contains(string(out), "MY CUSTOM PROMPT") {
		t.Fatalf("user's custom prompt overwritten:\n%s", string(out))
	}
}
