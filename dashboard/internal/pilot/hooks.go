package pilot

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type HookStatus struct {
	Installed    bool   `json:"installed"`
	SettingsPath string `json:"settings_path"`
}

func claudeSettingsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "settings.json")
}

func codexHooksPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "hooks.json")
}

func codexConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "config.toml")
}

// CheckHooksInstalled is deliberately event-aware. A legacy Pilot approval in
// Claude's PreToolUse event is not a healthy install: it runs before Claude Auto
// Mode and was the source of the prompts this dashboard is meant to prevent.
// An install without the PermissionDenied fallback is stale too: auto mode's
// classifier denies without prompting, so PermissionRequest alone never sees
// those calls.
func CheckHooksInstalled() HookStatus {
	claudePath := claudeSettingsPath()
	codexPath := codexHooksPath()
	installed := hookEventContains(claudePath, "PermissionRequest", "pilot approve") &&
		hookEventContains(claudePath, "PermissionDenied", "pilot on-denied") &&
		hookEventContains(claudePath, "PreToolUse", "pilot pre-approve") &&
		hookEventContains(codexPath, "PermissionRequest", "pilot codex-approve") &&
		codexConfigHealthy(codexConfigPath())
	return HookStatus{Installed: installed, SettingsPath: claudePath + " / " + codexPath}
}

// HooksNeedRepair lets a newly updated dashboard repair stale Claude and Codex
// hooks/config written by older dashboard/CLI builds, even when the CLI that
// launched it is stale. A completely disabled install stays disabled.
func HooksNeedRepair() bool {
	if !fileContains(claudeSettingsPath(), "pilot ") && !fileContains(codexHooksPath(), "pilot ") {
		return false
	}
	return !CheckHooksInstalled().Installed
}

func hookEventContains(path, event, commandMarker string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var settings map[string]any
	if json.Unmarshal(data, &settings) != nil {
		return false
	}
	hooks, _ := settings["hooks"].(map[string]any)
	entries, _ := hooks[event].([]any)
	for _, entry := range entries {
		encoded, _ := json.Marshal(entry)
		if strings.Contains(string(encoded), commandMarker) {
			return true
		}
	}
	return false
}

func fileContains(path, marker string) bool {
	data, err := os.ReadFile(path)
	return err == nil && strings.Contains(string(data), marker)
}

func codexConfigHealthy(path string) bool {
	var cfg struct {
		ApprovalsReviewer string `toml:"approvals_reviewer"`
		Features          struct {
			Hooks                   bool `toml:"hooks"`
			ExecPermissionApprovals bool `toml:"exec_permission_approvals"`
			RequestPermissionsTool  bool `toml:"request_permissions_tool"`
			DeprecatedCodexHooks    bool `toml:"codex_hooks"`
		} `toml:"features"`
		Hooks struct {
			State map[string]map[string]any `toml:"state"`
		} `toml:"hooks"`
	}
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return false
	}
	if cfg.ApprovalsReviewer != "auto_review" || !cfg.Features.Hooks ||
		!cfg.Features.ExecPermissionApprovals || !cfg.Features.RequestPermissionsTool ||
		cfg.Features.DeprecatedCodexHooks {
		return false
	}
	for key, state := range cfg.Hooks.State {
		if strings.Contains(key, ":permission_request:") {
			if hash, ok := state["trusted_hash"].(string); ok && strings.HasPrefix(hash, "sha256:") {
				return true
			}
		}
	}
	return false
}

// InstallHooks delegates lifecycle ownership to the CLI. Keeping a second hook
// policy in the dashboard caused it to restore a legacy PreToolUse approval
// immediately after the CLI had migrated approval to PermissionRequest.
//
// Run upgrade first because an old CLI can launch a newly downloaded dashboard.
// The upgraded CLI's start command then performs the canonical hook migration.
func InstallHooks() error {
	bin, err := FindPilotBinary()
	if err != nil {
		return err
	}
	if out, upgradeErr := exec.Command(bin, "upgrade").CombinedOutput(); upgradeErr != nil {
		// Starting the available CLI is still useful offline. If that also fails,
		// include the update failure in the surfaced error below.
		startErr := runPilotStart(bin)
		if startErr != nil {
			return fmt.Errorf("pilot update failed (%v: %s); start failed: %w", upgradeErr, strings.TrimSpace(string(out)), startErr)
		}
		return nil
	}

	// upgrade starts a newly installed release itself, but a no-op upgrade does
	// not. Re-resolve because upgrade may have moved Pilot to ~/.pilot/bin/pilot;
	// start is idempotent and also repairs hooks if the server was already up.
	if current, findErr := FindPilotBinary(); findErr == nil {
		bin = current
	}
	return runPilotStart(bin)
}

func runPilotStart(bin string) error {
	cmd := exec.Command(bin, "start")
	cmd.Env = append(os.Environ(), "PILOT_SKIP_AUTO_UPGRADE=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s start: %w: %s", bin, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func UninstallHooks() error {
	bin, err := FindPilotBinary()
	if err != nil {
		return err
	}
	if out, err := exec.Command(bin, "stop").CombinedOutput(); err != nil {
		return fmt.Errorf("%s stop: %w: %s", bin, err, strings.TrimSpace(string(out)))
	}
	return nil
}
