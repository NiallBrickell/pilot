package hooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

type Status struct {
	Installed          bool   `json:"installed"`
	ClaudeInstalled    bool   `json:"claude_installed"`
	CodexInstalled     bool   `json:"codex_installed"`
	ClaudeSettingsPath string `json:"claude_settings_path"`
	CodexHooksPath     string `json:"codex_hooks_path"`
	CodexConfigPath    string `json:"codex_config_path"`
}

func CheckInstalled() Status {
	st := Status{
		ClaudeSettingsPath: ClaudeSettingsPath(),
		CodexHooksPath:     CodexHooksPath(),
		CodexConfigPath:    CodexConfigPath(),
	}

	if data, err := os.ReadFile(st.ClaudeSettingsPath); err == nil {
		content := string(data)
		st.ClaudeInstalled = strings.Contains(content, "pilot approve") &&
			strings.Contains(content, "pilot interrogate") &&
			strings.Contains(content, "pilot on-stop")
	}

	if data, err := os.ReadFile(st.CodexHooksPath); err == nil {
		content := string(data)
		st.CodexInstalled = strings.Contains(content, "pilot codex-approve") &&
			strings.Contains(content, "pilot codex-interrogate") &&
			strings.Contains(content, "pilot codex-on-stop")
	}

	st.Installed = st.ClaudeInstalled || st.CodexInstalled
	return st
}

func InstallAll(pilotBin string) error {
	if err := InstallClaude(pilotBin); err != nil {
		return err
	}
	return InstallCodex(pilotBin)
}

func UninstallAll() error {
	if err := UninstallClaude(); err != nil {
		return err
	}
	return UninstallCodex()
}

func ClaudeSettingsPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "settings.json")
}

func CodexHooksPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "hooks.json")
}

func CodexConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".codex", "config.toml")
}

func InstallClaude(pilotBin string) error {
	path := ClaudeSettingsPath()

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
			"matcher": "^(Bash|Write|Edit|NotebookEdit|WebFetch|WebSearch|Read|Grep|Glob|Agent)$",
			"hooks": []any{
				map[string]any{"type": "command", "command": pilotBin + " approve"},
			},
		},
		map[string]any{
			"matcher": ".*",
			"hooks": []any{
				map[string]any{"type": "command", "command": pilotBin + " interrogate"},
			},
		},
	)
	hooks["Stop"] = mergeHookEntries(hooks["Stop"],
		map[string]any{
			"hooks": []any{
				map[string]any{"type": "command", "command": pilotBin + " on-stop"},
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

func UninstallClaude() error {
	path := ClaudeSettingsPath()
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

	for _, key := range []string{"PreToolUse", "Stop"} {
		removePilotHookEntries(hooks, key)
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

func InstallCodex(pilotBin string) error {
	if err := ensureCodexFeatures(CodexConfigPath()); err != nil {
		return err
	}

	path := CodexHooksPath()
	settings := make(map[string]any)
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, &settings)
	}

	hooks, _ := settings["hooks"].(map[string]any)
	if hooks == nil {
		hooks = make(map[string]any)
	}

	// Codex PreToolUse can only block. Keep it for trajectory checks only;
	// approval evaluation belongs in PermissionRequest so routine Bash calls
	// don't hit the LLM before Codex has decided an approval is needed.
	hooks["PreToolUse"] = mergeHookEntries(hooks["PreToolUse"],
		map[string]any{
			"matcher": ".*",
			"hooks": []any{
				map[string]any{
					"type":          "command",
					"command":       pilotBin + " codex-interrogate",
					"timeout":       90,
					"statusMessage": "Pilot checking trajectory",
				},
			},
		},
	)
	hooks["PermissionRequest"] = mergeHookEntries(hooks["PermissionRequest"],
		map[string]any{
			"matcher": "^(Bash|apply_patch|Edit|Write|mcp__.*)$",
			"hooks": []any{
				map[string]any{
					"type":          "command",
					"command":       pilotBin + " codex-approve",
					"timeout":       90,
					"statusMessage": "Pilot reviewing approval",
				},
			},
		},
	)
	hooks["Stop"] = mergeHookEntries(hooks["Stop"],
		map[string]any{
			"hooks": []any{
				map[string]any{
					"type":          "command",
					"command":       pilotBin + " codex-on-stop",
					"timeout":       30,
					"statusMessage": "Pilot checking whether to continue",
				},
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

func UninstallCodex() error {
	path := CodexHooksPath()
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

	for _, key := range []string{"PreToolUse", "PermissionRequest", "Stop"} {
		removePilotHookEntries(hooks, key)
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
			if isPilotHookEntry(string(entryJSON)) {
				continue
			}
			result = append(result, entry)
		}
	}
	for _, entry := range pilotEntries {
		result = append(result, entry)
	}
	return result
}

func removePilotHookEntries(hooks map[string]any, key string) {
	arr, ok := hooks[key].([]any)
	if !ok {
		return
	}
	var filtered []any
	for _, entry := range arr {
		entryJSON, _ := json.Marshal(entry)
		if !isPilotHookEntry(string(entryJSON)) {
			filtered = append(filtered, entry)
		}
	}
	if len(filtered) == 0 {
		delete(hooks, key)
	} else {
		hooks[key] = filtered
	}
}

func isPilotHookEntry(entryJSON string) bool {
	return strings.Contains(entryJSON, "pilot approve") ||
		strings.Contains(entryJSON, "pilot interrogate") ||
		strings.Contains(entryJSON, "pilot on-stop") ||
		strings.Contains(entryJSON, "pilot codex-approve") ||
		strings.Contains(entryJSON, "pilot codex-interrogate") ||
		strings.Contains(entryJSON, "pilot codex-on-stop")
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
