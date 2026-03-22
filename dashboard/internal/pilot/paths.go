package pilot

import (
	"os"
	"path/filepath"
)

func pilotDir() string {
	if p := os.Getenv("PILOT_HOME"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".pilot")
}
