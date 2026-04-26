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

func codexHooksPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "hooks.json")
}

func codexConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "config.toml")
}

func CheckHooksInstalled() HookStatus {
	claudePath := claudeSettingsPath()
	codexPath := codexHooksPath()
	installed := false
	if data, err := os.ReadFile(claudePath); err == nil {
		content := string(data)
		installed = installed || (strings.Contains(content, "pilot approve") &&
			strings.Contains(content, "pilot interrogate") &&
			strings.Contains(content, "pilot on-stop"))
	}
	if data, err := os.ReadFile(codexPath); err == nil {
		content := string(data)
		installed = installed || (strings.Contains(content, "pilot codex-approve") &&
			strings.Contains(content, "pilot codex-interrogate") &&
			strings.Contains(content, "pilot codex-on-stop"))
	}
	return HookStatus{Installed: installed, SettingsPath: claudePath + " / " + codexPath}
}

func InstallHooks() error {
	bin, err := FindPilotBinary()
	if err != nil {
		return err
	}
	if err := installClaudeHooks(bin); err != nil {
		return err
	}
	return installCodexHooks(bin)
}

func installClaudeHooks(bin string) error {
	path := claudeSettingsPath()

	settings := make(map[string]any)
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &settings)
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = make(map[string]any)
	}

	pilotPreToolUse := map[string]any{
		"matcher": "^(Bash|Write|Edit|NotebookEdit|WebFetch|WebSearch|Read|Grep|Glob|Agent)$",
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": bin + " approve",
			},
		},
	}

	pilotInterrogate := map[string]any{
		"matcher": ".*",
		"hooks": []any{
			map[string]any{
				"type":    "command",
				"command": bin + " interrogate",
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

	// Keep existing non-pilot entries, replace/add pilot entries
	hooks["PreToolUse"] = mergeHookEntries(hooks["PreToolUse"], pilotPreToolUse, pilotInterrogate)
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

func installCodexHooks(bin string) error {
	if err := ensureCodexFeatures(codexConfigPath()); err != nil {
		return err
	}

	path := codexHooksPath()
	settings := make(map[string]any)
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &settings)
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = make(map[string]any)
	}

	hooks["PreToolUse"] = mergeHookEntries(hooks["PreToolUse"],
		map[string]any{
			"matcher": ".*",
			"hooks": []any{
				map[string]any{"type": "command", "command": bin + " codex-interrogate", "timeout": 90, "statusMessage": "Pilot checking trajectory"},
			},
		},
	)
	hooks["PermissionRequest"] = mergeHookEntries(hooks["PermissionRequest"],
		map[string]any{
			"matcher": "^(Bash|apply_patch|Edit|Write|mcp__.*)$",
			"hooks": []any{
				map[string]any{"type": "command", "command": bin + " codex-approve", "timeout": 90, "statusMessage": "Pilot reviewing approval"},
			},
		},
	)
	hooks["Stop"] = mergeHookEntries(hooks["Stop"],
		map[string]any{
			"hooks": []any{
				map[string]any{"type": "command", "command": bin + " codex-on-stop", "timeout": 30, "statusMessage": "Pilot checking whether to continue"},
			},
		},
	)

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
	if err := uninstallHooksAtPath(claudeSettingsPath(), []string{"PreToolUse", "Stop"}); err != nil {
		return err
	}
	return uninstallHooksAtPath(codexHooksPath(), []string{"PreToolUse", "PermissionRequest", "Stop"})
}

func uninstallHooksAtPath(path string, keys []string) error {
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

	for _, key := range keys {
		removePilotEntries(hooks, key)
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

func mergeHookEntries(existing any, pilotEntries ...map[string]any) []any {
	var result []any
	if arr, ok := existing.([]any); ok {
		for _, entry := range arr {
			entryJSON, _ := json.Marshal(entry)
			if isPilotEntry(string(entryJSON)) {
				continue // Remove old pilot entries
			}
			result = append(result, entry)
		}
	}
	for _, entry := range pilotEntries {
		result = append(result, entry)
	}
	return result
}

// isPilotEntry returns true if a serialized hook entry belongs to pilot.
func isPilotEntry(entryJSON string) bool {
	return strings.Contains(entryJSON, "pilot approve") ||
		strings.Contains(entryJSON, "pilot interrogate") ||
		strings.Contains(entryJSON, "pilot on-stop") ||
		strings.Contains(entryJSON, "pilot codex-approve") ||
		strings.Contains(entryJSON, "pilot codex-interrogate") ||
		strings.Contains(entryJSON, "pilot codex-on-stop")
}

func removePilotEntries(hooks map[string]any, key string) {
	arr, ok := hooks[key].([]any)
	if !ok {
		return
	}
	var filtered []any
	for _, entry := range arr {
		entryJSON, _ := json.Marshal(entry)
		if !isPilotEntry(string(entryJSON)) {
			filtered = append(filtered, entry)
		}
	}
	if len(filtered) == 0 {
		delete(hooks, key)
	} else {
		hooks[key] = filtered
	}
}

func ensureCodexFeatures(path string) error {
	data, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	required := []string{
		"codex_hooks",
		"exec_permission_approvals",
		"request_permissions_tool",
	}
	seen := make(map[string]bool, len(required))
	content := string(data)
	lines := strings.Split(content, "\n")
	inFeatures := false
	featuresSeen := false
	var out []string

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			if inFeatures {
				out = appendMissingCodexFeatures(out, required, seen)
			}
			inFeatures = trimmed == "[features]"
			if inFeatures {
				featuresSeen = true
			}
		}
		if inFeatures {
			if key, ok := codexFeatureAssignmentKey(trimmed); ok && isRequiredCodexFeature(key, required) {
				out = append(out, key+" = true")
				seen[key] = true
				continue
			}
		}
		if i == len(lines)-1 && trimmed == "" {
			continue
		}
		out = append(out, line)
	}

	if inFeatures {
		out = appendMissingCodexFeatures(out, required, seen)
	}
	if !featuresSeen {
		if len(out) > 0 && strings.TrimSpace(out[len(out)-1]) != "" {
			out = append(out, "")
		}
		out = append(out, "[features]")
		for _, key := range required {
			out = append(out, key+" = true")
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.Join(out, "\n")+"\n"), 0600)
}

func appendMissingCodexFeatures(out []string, required []string, seen map[string]bool) []string {
	for _, key := range required {
		if !seen[key] {
			out = append(out, key+" = true")
			seen[key] = true
		}
	}
	return out
}

func codexFeatureAssignmentKey(trimmed string) (string, bool) {
	idx := strings.Index(trimmed, "=")
	if idx == -1 {
		return "", false
	}
	return strings.TrimSpace(trimmed[:idx]), true
}

func isRequiredCodexFeature(key string, required []string) bool {
	for _, want := range required {
		if key == want {
			return true
		}
	}
	return false
}
