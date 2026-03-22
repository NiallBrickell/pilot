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

func CheckHooksInstalled() HookStatus {
	path := claudeSettingsPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return HookStatus{Installed: false, SettingsPath: path}
	}
	// Check for any pilot hook regardless of binary path
	installed := strings.Contains(string(data), "pilot approve")
	return HookStatus{Installed: installed, SettingsPath: path}
}

func InstallHooks() error {
	path := claudeSettingsPath()
	bin, err := FindPilotBinary()
	if err != nil {
		return err
	}

	settings := make(map[string]any)
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &settings)
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = make(map[string]any)
	}

	pilotPreToolUse := map[string]any{
		"matcher": "^(Bash|Write|Edit|NotebookEdit|WebFetch|WebSearch)$",
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": bin + " approve",
			},
		},
	}

	pilotStop := map[string]any{
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": bin + " on-stop",
			},
		},
	}

	// Keep existing non-pilot entries, replace/add pilot entry
	hooks["PreToolUse"] = mergeHookEntries(hooks["PreToolUse"], pilotPreToolUse)
	hooks["Stop"] = mergeHookEntries(hooks["Stop"], pilotStop)

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

	// Remove any entry containing "pilot approve" or "pilot on-stop"
	removePilotEntries(hooks, "PreToolUse")
	removePilotEntries(hooks, "Stop")

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

func mergeHookEntries(existing any, pilotEntry map[string]any) []any {
	var result []any
	if arr, ok := existing.([]any); ok {
		for _, entry := range arr {
			entryJSON, _ := json.Marshal(entry)
			if strings.Contains(string(entryJSON), "pilot approve") || strings.Contains(string(entryJSON), "pilot on-stop") {
				continue // Remove old pilot entries
			}
			result = append(result, entry)
		}
	}
	result = append(result, pilotEntry)
	return result
}

func removePilotEntries(hooks map[string]any, key string) {
	arr, ok := hooks[key].([]any)
	if !ok {
		return
	}
	var filtered []any
	for _, entry := range arr {
		entryJSON, _ := json.Marshal(entry)
		if !strings.Contains(string(entryJSON), "pilot approve") && !strings.Contains(string(entryJSON), "pilot on-stop") {
			filtered = append(filtered, entry)
		}
	}
	if len(filtered) == 0 {
		delete(hooks, key)
	} else {
		hooks[key] = filtered
	}
}
