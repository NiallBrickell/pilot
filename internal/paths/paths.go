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
func BinPathFile() string  { return filepath.Join(PilotDir(), "pilot-bin") }

// RecordBinaryPath writes the resolved path of the running pilot binary to
// ~/.pilot/pilot-bin and creates a ~/.pilot/pilot symlink so external tools
// (like the dashboard) can find it without relying on PATH or cwd.
func RecordBinaryPath() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	_ = EnsureDir()
	_ = os.WriteFile(BinPathFile(), []byte(exe), 0644)

	link := filepath.Join(PilotDir(), "pilot")
	if existing, err := os.Readlink(link); err == nil && existing == exe {
		return
	}
	_ = os.Remove(link)
	_ = os.Symlink(exe, link)
}

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
