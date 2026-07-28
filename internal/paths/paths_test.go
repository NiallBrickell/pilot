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

	// First upgrade call — records the baseline. Nothing to upgrade yet: the
	// prompts already are this build's default, so that hash becomes the baseline.
	res, err := UpgradeDefaults(oldEmbedded, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Upgraded || res.Reason != "up_to_date" {
		t.Fatalf("expected up_to_date, got %+v", res)
	}
	if b, _ := os.ReadFile(PromptBaselineFile()); len(b) == 0 {
		t.Fatal("baseline not recorded")
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
	res, err = UpgradeDefaults(newEmbedded, nil)
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

func TestPromptsStatusOf_States(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PILOT_HOME", dir)

	embedded := `[general]
sse_port = 9721

[prompts]
approval = "v2"
auto_respond = "v2"
`

	// No config yet → no_config.
	if s, _ := PromptsStatusOf(embedded, nil); s.State != PromptsNoConfig {
		t.Fatalf("expected no_config, got %q", s.State)
	}

	// Write old default, no baseline → bootstrapped.
	oldUserConfig := `[general]
sse_port = 9721

[prompts]
approval = "v1"
auto_respond = "v1"
`
	if err := os.WriteFile(ConfigFile(), []byte(oldUserConfig), 0644); err != nil {
		t.Fatal(err)
	}
	if s, _ := PromptsStatusOf(embedded, nil); s.State != PromptsBootstrapped {
		t.Fatalf("expected bootstrapped, got %q", s.State)
	}

	// Set baseline = user's v1 hash → behind (embedded is v2, user is on v1).
	v1Hash, _ := promptHashFromTOML(oldUserConfig)
	if err := os.WriteFile(PromptBaselineFile(), []byte(v1Hash), 0644); err != nil {
		t.Fatal(err)
	}
	if s, _ := PromptsStatusOf(embedded, nil); s.State != PromptsBehind {
		t.Fatalf("expected behind, got %q", s.State)
	}

	// User edits prompts → customised.
	editedConfig := strings.Replace(oldUserConfig, `approval = "v1"`, `approval = "CUSTOM"`, 1)
	if err := os.WriteFile(ConfigFile(), []byte(editedConfig), 0644); err != nil {
		t.Fatal(err)
	}
	if s, _ := PromptsStatusOf(embedded, nil); s.State != PromptsCustomised {
		t.Fatalf("expected customised, got %q", s.State)
	}

	// User prompts match embedded → up_to_date.
	if err := os.WriteFile(ConfigFile(), []byte(embedded), 0644); err != nil {
		t.Fatal(err)
	}
	if s, _ := PromptsStatusOf(embedded, nil); s.State != PromptsUpToDate {
		t.Fatalf("expected up_to_date, got %q", s.State)
	}
}

func TestResetPromptsToDefault_OverridesCustomisation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PILOT_HOME", dir)

	customised := `[general]
sse_port = 9999

[prompts]
approval = "I HAVE EDITED THIS"
auto_respond = "ALSO THIS"
`
	if err := os.WriteFile(ConfigFile(), []byte(customised), 0644); err != nil {
		t.Fatal(err)
	}
	embedded := `[prompts]
approval = "default approval"
auto_respond = "default auto_respond"
`
	res, err := ResetPromptsToDefault(embedded)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Upgraded {
		t.Fatalf("expected reset to apply, got %+v", res)
	}

	out, _ := os.ReadFile(ConfigFile())
	s := string(out)
	if !strings.Contains(s, "sse_port = 9999") {
		t.Fatalf("user's general settings lost:\n%s", s)
	}
	if strings.Contains(s, "I HAVE EDITED THIS") {
		t.Fatalf("custom prompt not overridden:\n%s", s)
	}
	if !strings.Contains(s, "default approval") {
		t.Fatalf("default prompt not applied:\n%s", s)
	}

	// Status should now report up_to_date and baseline should match embedded.
	status, _ := PromptsStatusOf(embedded, nil)
	if status.State != PromptsUpToDate {
		t.Fatalf("expected up_to_date after reset, got %q", status.State)
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
	res, err := UpgradeDefaults(newEmbedded, nil)
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

// TestUpgradeDefaults_HealsDriftedBaseline covers the failure this whole
// mechanism exists to survive: the config file moved to a newer default while
// .prompt_baseline stayed behind (a dashboard save, a hand-copied config, an
// install predating the baseline file). The user never edited a prompt, but
// baseline != userHash, so they read as "customised" and every later default is
// silently withheld — which is how an install ends up pinned to a months-old
// prompt no matter how many times the default is fixed.
func TestUpgradeDefaults_HealsDriftedBaseline(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PILOT_HOME", dir)

	v1 := `[prompts]
approval = "v1"
auto_respond = "v1"
`
	v2 := `[prompts]
approval = "v2"
auto_respond = "v2"
`
	v3 := `[general]
sse_port = 9999

[prompts]
approval = "v3"
auto_respond = "v3"
`
	v1Hash, _ := promptHashFromTOML(v1)
	v2Hash, _ := promptHashFromTOML(v2)

	// On disk: the v2 default. Baseline: still v1.
	if err := os.WriteFile(ConfigFile(), []byte(v2), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(PromptBaselineFile(), []byte(v1Hash), 0644); err != nil {
		t.Fatal(err)
	}

	// Without the shipped history there's nothing to distinguish this from a
	// real edit, so it stays put — the old, broken behaviour.
	res, err := UpgradeDefaults(v3, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Reason != "user_customised" {
		t.Fatalf("expected user_customised without history, got %+v", res)
	}

	// Knowing v2 is one of ours, the drift is recognised and v3 is applied.
	shipped := []string{v1Hash, v2Hash}
	if s, _ := PromptsStatusOf(v3, shipped); s.State != PromptsBehind {
		t.Fatalf("expected behind, got %q", s.State)
	}
	res, err = UpgradeDefaults(v3, shipped)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Upgraded {
		t.Fatalf("expected upgrade, got %+v", res)
	}

	out, _ := os.ReadFile(ConfigFile())
	if !strings.Contains(string(out), `approval = "v3"`) {
		t.Fatalf("prompts not upgraded:\n%s", string(out))
	}

	// Baseline is realigned, so the next default flows through normally.
	baseline, _ := os.ReadFile(PromptBaselineFile())
	v3Hash, _ := promptHashFromTOML(v3)
	if string(baseline) != v3Hash {
		t.Fatalf("baseline not realigned: got %q want %q", string(baseline), v3Hash)
	}
}

// TestUpgradeDefaults_UpgradesUnbaselinedShippedDefault covers an install that
// has no baseline at all but is sitting on a default we recognise: it should
// upgrade immediately rather than burning a run on bootstrapping.
func TestUpgradeDefaults_UpgradesUnbaselinedShippedDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PILOT_HOME", dir)

	v1 := `[prompts]
approval = "v1"
auto_respond = "v1"
`
	v2 := `[prompts]
approval = "v2"
auto_respond = "v2"
`
	if err := os.WriteFile(ConfigFile(), []byte(v1), 0644); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(PromptBaselineFile())

	v1Hash, _ := promptHashFromTOML(v1)
	res, err := UpgradeDefaults(v2, []string{v1Hash})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Upgraded {
		t.Fatalf("expected upgrade, got %+v", res)
	}
}
