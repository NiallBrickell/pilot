package paths

import (
	"os"
	"path/filepath"
)

// PilotDir returns the base directory for pilot config and state.
// Uses $PILOT_HOME if set, otherwise ~/.pilot.
func PilotDir() string {
	if p := os.Getenv("PILOT_HOME"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".pilot")
}

func ConfigFile() string   { return filepath.Join(PilotDir(), "pilot.toml") }
func StateFile() string    { return filepath.Join(PilotDir(), "state.json") }
func PidFile() string      { return filepath.Join(PilotDir(), "pilot.pid") }
func ServePidFile() string { return filepath.Join(PilotDir(), "pilot-serve.pid") }
func AuthCache() string    { return filepath.Join(PilotDir(), ".auth-cache") }
func EnvFile() string      { return filepath.Join(PilotDir(), ".env") }

// EnsureDir creates the pilot directory if it doesn't exist.
func EnsureDir() error {
	return os.MkdirAll(PilotDir(), 0755)
}

// EnsureSetup creates ~/.pilot/ and writes a default pilot.toml if one doesn't exist.
// embeddedConfig is the default config content to write.
func EnsureSetup(embeddedConfig string) error {
	if err := EnsureDir(); err != nil {
		return err
	}
	cfgPath := ConfigFile()
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		return os.WriteFile(cfgPath, []byte(embeddedConfig), 0644)
	}
	return nil
}
