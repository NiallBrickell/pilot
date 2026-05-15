package approve

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type codexConfig struct {
	Projects map[string]codexProjectConfig `toml:"projects"`
}

type codexProjectConfig struct {
	TrustLevel string `toml:"trust_level"`
}

// CheckCodexSettings evaluates the request against Codex's own local trust
// config. In trusted projects, Codex's normal permission model already treats
// routine local work as allowed, so Pilot should not spend LLM calls re-checking
// every permission request. Obvious destructive commands still block locally.
func CheckCodexSettings(toolName string, parsed map[string]any, toolInput, cwd string) string {
	if cwd == "" {
		cwd, _ = os.Getwd()
	}
	if !isCodexTrustedProject(cwd) {
		return ""
	}

	switch toolName {
	case "Bash":
		command := extractKeyField(toolName, parsed, toolInput)
		if isDangerousBashCommand(command) {
			return "deny"
		}
		return "allow"
	case "apply_patch":
		return "allow"
	case "Edit", "Write":
		if target := extractKeyField(toolName, parsed, toolInput); target != "" && !pathWithinCwd(target, cwd) {
			return ""
		}
		return "allow"
	default:
		// Trusted project: allow unknown tools (filesystem permissions,
		// request_permissions, etc.) to match Codex's own trust model.
		return "allow"
	}
}

func isCodexTrustedProject(cwd string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	var cfg codexConfig
	if _, err := toml.DecodeFile(filepath.Join(home, ".codex", "config.toml"), &cfg); err != nil {
		return false
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return false
	}
	for projectPath, project := range cfg.Projects {
		if !strings.EqualFold(project.TrustLevel, "trusted") {
			continue
		}
		absProject, err := filepath.Abs(projectPath)
		if err != nil {
			continue
		}
		if isWithinDir(absCwd, absProject) {
			return true
		}
	}
	return false
}

func pathWithinCwd(path, cwd string) bool {
	if path == "" || cwd == "" {
		return false
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return false
	}
	return isWithinDir(absPath, absCwd)
}

func isDangerousBashCommand(command string) bool {
	cmd := strings.ToLower(strings.TrimSpace(command))
	if cmd == "" {
		return false
	}

	denySubstrings := []string{
		"rm -rf /",
		"rm -rf ~",
		"git reset --hard",
		"git clean -fd",
		"git clean -xdf",
		"git push --force",
		"git push -f",
		"push --force-with-lease",
		"gh pr merge",
		"npm publish",
		"pnpm publish",
		"yarn publish",
		"terraform destroy",
		"kubectl delete",
		"fly destroy",
		"railway delete",
		"vercel rm",
	}
	for _, denied := range denySubstrings {
		if strings.Contains(cmd, denied) {
			return true
		}
	}

	fields := strings.Fields(cmd)
	for i, field := range fields {
		if field == "git" && i+1 < len(fields) && fields[i+1] == "merge" {
			return true
		}
	}
	return false
}
