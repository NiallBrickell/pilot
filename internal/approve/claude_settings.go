// Layer 1: Claude Code settings interpreter.
// Reads the user's actual Claude Code settings files and evaluates whether
// a tool call would be auto-approved by Claude's own permission system.
package approve

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// claudeSettings mirrors Claude Code's settings.json / settings.local.json
type claudeSettings struct {
	Permissions struct {
		Allow       []string `json:"allow"`
		Deny        []string `json:"deny"`
		Ask         []string `json:"ask"`
		DefaultMode string   `json:"defaultMode"`
	} `json:"permissions"`
}

// Built-in tools Claude Code always auto-approves regardless of settings.
// Note: Read, Grep, Glob are NOT here — pilot evaluates them (e.g. out-of-cwd reads).
var builtinAutoApproved = map[string]bool{
	"LSP": true, "TodoRead": true, "TaskGet": true, "TaskList": true,
	"TaskCreate": true, "TaskUpdate": true,
}

// CheckClaudeSettings evaluates the tool call against Claude Code's settings
// hierarchy. Walks from cwd upward, checking each settings file in order.
// First match wins — a local deny can't be overridden by a parent allow.
// Returns "allow", "deny", or "" (no match in any settings file).
func CheckClaudeSettings(toolName, toolInput, cwd string) string {
	if builtinAutoApproved[toolName] {
		return "allow"
	}

	if cwd == "" {
		cwd, _ = os.Getwd()
	}

	// Collect settings files from most local to most global.
	// At each directory, check settings.local.json first (highest priority),
	// then settings.json (project-level, committed).
	var files []claudeSettings

	for dir := cwd; dir != ""; {
		for _, name := range []string{"settings.local.json", "settings.json"} {
			s := loadSettingsFile(filepath.Join(dir, ".claude", name))
			if len(s.Permissions.Allow) > 0 || len(s.Permissions.Deny) > 0 ||
				len(s.Permissions.Ask) > 0 || s.Permissions.DefaultMode != "" {
				files = append(files, s)
			}
		}

		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	// Global settings last (lowest priority)
	home, err := os.UserHomeDir()
	if err == nil {
		global := loadSettingsFile(filepath.Join(home, ".claude", "settings.json"))
		files = append(files, global)
	}

	// Evaluate each file in order — first match wins
	toolSig := buildSignature(toolName, toolInput)
	for _, s := range files {
		result := evaluateAgainstSettings(s, toolName, toolSig)
		if result != "" {
			return result
		}
	}

	return ""
}

// evaluateAgainstSettings checks one settings file. Returns "allow", "deny", or "" (no match).
func evaluateAgainstSettings(s claudeSettings, toolName, toolSig string) string {
	// acceptEdits auto-approves file edits
	if s.Permissions.DefaultMode == "acceptEdits" &&
		(toolName == "Write" || toolName == "Edit" || toolName == "NotebookEdit") {
		return "allow"
	}

	// Deny rules checked first within this file
	for _, pattern := range s.Permissions.Deny {
		if matchesPattern(pattern, toolName, toolSig) {
			return "deny"
		}
	}

	// Ask rules — explicit "prompt the user"
	for _, pattern := range s.Permissions.Ask {
		if matchesPattern(pattern, toolName, toolSig) {
			return "deny"
		}
	}

	// Allow rules
	for _, pattern := range s.Permissions.Allow {
		if matchesPattern(pattern, toolName, toolSig) {
			return "allow"
		}
	}

	// No match in this file — fall through to next
	return ""
}

func loadSettingsFile(path string) claudeSettings {
	var s claudeSettings
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	_ = json.Unmarshal(data, &s)
	return s
}

func buildSignature(toolName, toolInput string) string {
	if toolInput == "" {
		return toolName
	}

	// Claude Code sends tool input as JSON. Extract the relevant field
	// to match against permission patterns like "Bash(git status:*)".
	extracted := extractKeyField(toolName, toolInput)
	return toolName + "(" + strings.TrimSpace(extracted) + ")"
}

// extractKeyField pulls the permission-relevant field from JSON tool input.
// For Bash: "command" field. For Edit/Write: "file_path" field.
// Falls back to raw input if not JSON or field not found.
func extractKeyField(toolName, toolInput string) string {
	if len(toolInput) == 0 || toolInput[0] != '{' {
		return toolInput // Not JSON, use as-is
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(toolInput), &parsed); err != nil {
		return toolInput
	}

	switch toolName {
	case "Bash":
		if cmd, ok := parsed["command"].(string); ok {
			return cmd
		}
	case "Edit", "Write", "NotebookEdit", "Read":
		if fp, ok := parsed["file_path"].(string); ok {
			return fp
		}
	case "Grep":
		if p, ok := parsed["path"].(string); ok {
			return p
		}
	case "Glob":
		if p, ok := parsed["path"].(string); ok {
			return p
		}
	case "Agent":
		if desc, ok := parsed["description"].(string); ok {
			return desc
		}
	case "WebFetch":
		if url, ok := parsed["url"].(string); ok {
			return "domain:" + extractDomain(url)
		}
	}

	return toolInput
}

func extractDomain(url string) string {
	// Simple domain extraction: strip protocol, take host
	u := strings.TrimPrefix(strings.TrimPrefix(url, "https://"), "http://")
	if i := strings.IndexByte(u, '/'); i >= 0 {
		u = u[:i]
	}
	return u
}

func matchesPattern(pattern, toolName, toolSig string) bool {
	if !strings.Contains(pattern, "(") {
		return pattern == toolName
	}

	patternTool, patternContent, ok := strings.Cut(pattern, "(")
	if !ok || patternTool != toolName {
		return false
	}
	patternContent = strings.TrimSuffix(patternContent, ")")

	if patternContent == "*" {
		return true
	}

	if prefix, ok := strings.CutSuffix(patternContent, ":*"); ok {
		sigContent := extractSigContent(toolSig, toolName)
		return strings.HasPrefix(sigContent, prefix)
	}

	sigContent := extractSigContent(toolSig, toolName)
	return sigContent == patternContent
}

func extractSigContent(toolSig, toolName string) string {
	prefix := toolName + "("
	if strings.HasPrefix(toolSig, prefix) && strings.HasSuffix(toolSig, ")") {
		return toolSig[len(prefix) : len(toolSig)-1]
	}
	return toolSig
}
