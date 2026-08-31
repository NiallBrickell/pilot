package pilot

import (
	"os"
	"path/filepath"
	"strings"
)

func pilotDir() string {
	if p := os.Getenv("PILOT_HOME"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".pilot")
}

// ServerToken mirrors the CLI's config.ServerToken: the bearer pilot serve
// requires on POST routes when PILOT_SERVER_TOKEN is set in the environment
// or in ~/.pilot/.env. The dashboard's approve/reject buttons POST to serve
// and must carry it, or a token-protected server silently rejects them.
func ServerToken() string {
	if t := os.Getenv("PILOT_SERVER_TOKEN"); t != "" {
		return t
	}
	data, err := os.ReadFile(filepath.Join(pilotDir(), ".env"))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != "PILOT_SERVER_TOKEN" {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		return v
	}
	return ""
}
