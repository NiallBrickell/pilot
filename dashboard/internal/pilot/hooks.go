package pilot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

type HookStatus struct {
	Installed    bool   `json:"installed"`
	SettingsPath string `json:"settings_path"`
}

func claudeSettingsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "settings.json")
}

func pilotBinaryPath() string {
	bin, err := findPilotBinary()
	if err != nil {
		return "pilot"
	}
	return bin
}

func CheckHooksInstalled() HookStatus {
	path := claudeSettingsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return HookStatus{Installed: false, SettingsPath: path}
	}

	bin := pilotBinaryPath()
	installed := strings.Contains(string(data), bin+" approve")

	return HookStatus{Installed: installed, SettingsPath: path}
}

func InstallHooks() error {
	path := claudeSettingsPath()
	bin := pilotBinaryPath()

	settings := make(map[string]any)
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &settings)
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = make(map[string]any)
	}

	hooks["PreToolUse"] = []any{
		map[string]any{
			"matcher": "^(Bash|Write|Edit|NotebookEdit|WebFetch|WebSearch)$",
			"hooks": []any{
				map[string]any{
					"type":    "command",
					"command": bin + " approve",
				},
			},
		},
	}

	hooks["Stop"] = []any{
		map[string]any{
			"hooks": []any{
				map[string]any{
					"type":    "command",
					"command": bin + " on-stop",
				},
			},
		},
	}

	settings["hooks"] = hooks

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	return os.WriteFile(path, data, 0644)
}

func UninstallHooks() error {
	path := claudeSettingsPath()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	settings := make(map[string]any)
	if err := json.Unmarshal(data, &settings); err != nil {
		return err
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		return nil
	}

	bin := pilotBinaryPath()

	if pre, ok := hooks["PreToolUse"].([]any); ok {
		var filtered []any
		for _, entry := range pre {
			entryJSON, _ := json.Marshal(entry)
			if !strings.Contains(string(entryJSON), bin) {
				filtered = append(filtered, entry)
			}
		}
		if len(filtered) == 0 {
			delete(hooks, "PreToolUse")
		} else {
			hooks["PreToolUse"] = filtered
		}
	}

	if stop, ok := hooks["Stop"].([]any); ok {
		var filtered []any
		for _, entry := range stop {
			entryJSON, _ := json.Marshal(entry)
			if !strings.Contains(string(entryJSON), bin) {
				filtered = append(filtered, entry)
			}
		}
		if len(filtered) == 0 {
			delete(hooks, "Stop")
		} else {
			hooks["Stop"] = filtered
		}
	}

	if len(hooks) == 0 {
		delete(settings, "hooks")
	} else {
		settings["hooks"] = hooks
	}

	out, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(path, out, 0644)
}
