package paths

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
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

func ConfigFile() string         { return filepath.Join(PilotDir(), "pilot.toml") }
func StateFile() string          { return filepath.Join(PilotDir(), "state.json") }
func PidFile() string            { return filepath.Join(PilotDir(), "pilot.pid") }
func ServePidFile() string       { return filepath.Join(PilotDir(), "pilot-serve.pid") }
func AuthCache() string          { return filepath.Join(PilotDir(), ".auth-cache") }
func EnvFile() string            { return filepath.Join(PilotDir(), ".env") }
func BinPathFile() string        { return filepath.Join(PilotDir(), "pilot-bin") }
func PromptBaselineFile() string { return filepath.Join(PilotDir(), ".prompt_baseline") }

// BinDir is the stable directory for the installed pilot binary (used by
// install.sh and `pilot upgrade`), independent of any source checkout.
func BinDir() string { return filepath.Join(PilotDir(), "bin") }

// InstalledBinPath is the canonical location of the installed pilot binary.
func InstalledBinPath() string { return filepath.Join(BinDir(), "pilot") }

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
// On fresh install, also writes the prompt baseline so future default changes
// can flow through via UpgradeDefaults.
func EnsureSetup(embeddedConfig string) error {
	if err := EnsureDir(); err != nil {
		return err
	}
	cfgPath := ConfigFile()
	if _, err := os.Stat(cfgPath); os.IsNotExist(err) {
		if err := os.WriteFile(cfgPath, []byte(embeddedConfig), 0644); err != nil {
			return err
		}
		if hash, err := promptHashFromTOML(embeddedConfig); err == nil {
			_ = os.WriteFile(PromptBaselineFile(), []byte(hash), 0644)
		}
	}
	return nil
}

// UpgradeResult describes what UpgradeDefaults did.
type UpgradeResult struct {
	// Upgraded is true when the user's pilot.toml was replaced with the embedded default.
	Upgraded bool
	// BackupPath is the path to the pre-upgrade backup, if Upgraded is true.
	BackupPath string
	// Reason describes why no upgrade happened (empty when Upgraded is true).
	// Values: "up_to_date", "user_customised", "bootstrapped", "no_config", "parse_error".
	Reason string
}

// UpgradeDefaults re-writes ~/.pilot/pilot.toml with the embedded default IFF
// the user hasn't written their own prompts. shippedHashes lists the prompt
// hash of every default pilot has released; prompts matching one of them are an
// old default rather than a customisation. Called at serve startup — not on the
// hot approval path.
//
// Behaviour:
//   - No config file yet → no-op (EnsureSetup handles initial write).
//   - Current prompts match embedded → already on latest, refresh baseline if drifted.
//   - Current prompts match baseline OR any shipped default → user hasn't edited,
//     upgrade. Backs up the old file first.
//   - No baseline recorded and prompts match nothing we shipped → bootstrap:
//     record the current hash so future default changes flow through.
//   - Otherwise → user has customised, leave alone.
//
// The shippedHashes check is what keeps a baseline that has drifted from the
// config file (a dashboard save, a hand-copied config, an install predating the
// baseline file) from latching the user into "customised" forever, which
// silently pins their evaluator to whatever prompt they had at the time.
func UpgradeDefaults(embeddedConfig string, shippedHashes []string) (UpgradeResult, error) {
	cfgPath := ConfigFile()
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return UpgradeResult{Reason: "no_config"}, nil
		}
		return UpgradeResult{}, err
	}

	embeddedHash, err := promptHashFromTOML(embeddedConfig)
	if err != nil {
		return UpgradeResult{Reason: "parse_error"}, nil
	}
	userHash, err := promptHashFromTOML(string(data))
	if err != nil {
		return UpgradeResult{Reason: "parse_error"}, nil
	}

	baselinePath := PromptBaselineFile()
	baselineBytes, err := os.ReadFile(baselinePath)
	if err != nil && !os.IsNotExist(err) {
		return UpgradeResult{}, err
	}
	baseline := string(trimWhitespace(baselineBytes))
	haveBaseline := err == nil

	if userHash == embeddedHash {
		if baseline != embeddedHash {
			_ = os.WriteFile(baselinePath, []byte(embeddedHash), 0644)
		}
		return UpgradeResult{Reason: "up_to_date"}, nil
	}

	if baseline == userHash || isShippedPrompt(userHash, shippedHashes) {
		return applyEmbeddedPrompts(data, embeddedConfig, embeddedHash, "pre-upgrade")
	}

	if !haveBaseline {
		// Prompts we've never shipped and no baseline to compare against — can't
		// tell an old default from an edit, so record it and upgrade next time.
		if writeErr := os.WriteFile(baselinePath, []byte(userHash), 0644); writeErr != nil {
			return UpgradeResult{}, writeErr
		}
		return UpgradeResult{Reason: "bootstrapped"}, nil
	}

	return UpgradeResult{Reason: "user_customised"}, nil
}

// isShippedPrompt reports whether hash matches a default pilot has released.
func isShippedPrompt(hash string, shippedHashes []string) bool {
	return slices.Contains(shippedHashes, hash)
}

// PromptsState describes the relationship between user prompts and embedded default.
type PromptsState string

const (
	PromptsUpToDate     PromptsState = "up_to_date"    // user == embedded
	PromptsBehind       PromptsState = "behind"        // user == baseline, user != embedded
	PromptsCustomised   PromptsState = "customised"    // user != baseline, user != embedded
	PromptsBootstrapped PromptsState = "bootstrapped"  // no baseline recorded yet
	PromptsNoConfig     PromptsState = "no_config"     // pilot.toml missing
	PromptsParseError   PromptsState = "parse_error"   // couldn't parse user or embedded TOML
)

// PromptsStatus is the comparison between user prompts and embedded defaults.
type PromptsStatus struct {
	State        PromptsState `json:"state"`
	UserHash     string       `json:"user_hash"`
	EmbeddedHash string       `json:"embedded_hash"`
	BaselineHash string       `json:"baseline_hash"`
}

// PromptsStatusOf reports whether the user's prompts match the embedded default,
// differ but are upgrade-eligible (behind), or have been customised.
func PromptsStatusOf(embeddedConfig string, shippedHashes []string) (PromptsStatus, error) {
	data, err := os.ReadFile(ConfigFile())
	if err != nil {
		if os.IsNotExist(err) {
			return PromptsStatus{State: PromptsNoConfig}, nil
		}
		return PromptsStatus{}, err
	}

	embeddedHash, err := promptHashFromTOML(embeddedConfig)
	if err != nil {
		return PromptsStatus{State: PromptsParseError}, nil
	}
	userHash, err := promptHashFromTOML(string(data))
	if err != nil {
		return PromptsStatus{State: PromptsParseError, EmbeddedHash: embeddedHash}, nil
	}

	baselineBytes, _ := os.ReadFile(PromptBaselineFile())
	baseline := string(trimWhitespace(baselineBytes))

	status := PromptsStatus{
		UserHash:     userHash,
		EmbeddedHash: embeddedHash,
		BaselineHash: baseline,
	}
	switch {
	case userHash == embeddedHash:
		status.State = PromptsUpToDate
	case baseline == userHash || isShippedPrompt(userHash, shippedHashes):
		status.State = PromptsBehind
	case baseline == "":
		status.State = PromptsBootstrapped
	default:
		status.State = PromptsCustomised
	}
	return status, nil
}

// ResetPromptsToDefault force-replaces the [prompts] section with the embedded
// default regardless of whether the user has customised it. Backs up the old
// file first and refreshes the baseline. This is an explicit user action from
// the dashboard — UpgradeDefaults handles the automatic path.
func ResetPromptsToDefault(embeddedConfig string) (UpgradeResult, error) {
	data, err := os.ReadFile(ConfigFile())
	if err != nil {
		if os.IsNotExist(err) {
			return UpgradeResult{Reason: "no_config"}, nil
		}
		return UpgradeResult{}, err
	}
	embeddedHash, err := promptHashFromTOML(embeddedConfig)
	if err != nil {
		return UpgradeResult{Reason: "parse_error"}, fmt.Errorf("parse embedded: %w", err)
	}
	return applyEmbeddedPrompts(data, embeddedConfig, embeddedHash, "pre-reset")
}

// applyEmbeddedPrompts does the surgical merge + backup + baseline write shared
// by UpgradeDefaults (auto path) and ResetPromptsToDefault (user-initiated).
// backupLabel goes into the backup filename so operators can tell them apart.
func applyEmbeddedPrompts(userData []byte, embeddedConfig, embeddedHash, backupLabel string) (UpgradeResult, error) {
	cfgPath := ConfigFile()
	baselinePath := PromptBaselineFile()

	merged, err := replacePromptsSection(string(userData), embeddedConfig)
	if err != nil {
		return UpgradeResult{}, fmt.Errorf("merge prompts: %w", err)
	}

	backup := cfgPath + "." + backupLabel + "-" + time.Now().Format("20060102-150405") + ".bak"
	if err := os.WriteFile(backup, userData, 0644); err != nil {
		return UpgradeResult{}, fmt.Errorf("write backup: %w", err)
	}
	if err := os.WriteFile(cfgPath, []byte(merged), 0644); err != nil {
		return UpgradeResult{}, fmt.Errorf("write config: %w", err)
	}
	if err := os.WriteFile(baselinePath, []byte(embeddedHash), 0644); err != nil {
		return UpgradeResult{}, fmt.Errorf("write baseline: %w", err)
	}
	return UpgradeResult{Upgraded: true, BackupPath: backup}, nil
}

// replacePromptsSection returns userConfig with its [prompts] section replaced
// by the [prompts] section from embeddedConfig. All other content (general
// settings, webhooks, comments, formatting) is preserved verbatim. If the user
// file has no [prompts] section, the embedded one is appended.
func replacePromptsSection(userConfig, embeddedConfig string) (string, error) {
	embStart, embEnd, ok := extractSection(embeddedConfig, "prompts")
	if !ok {
		return "", fmt.Errorf("embedded config missing [prompts] section")
	}
	embBlock := embeddedConfig[embStart:embEnd]
	if !strings.HasSuffix(embBlock, "\n") {
		embBlock += "\n"
	}

	userStart, userEnd, userOk := extractSection(userConfig, "prompts")
	if !userOk {
		sep := ""
		if len(userConfig) > 0 && !strings.HasSuffix(userConfig, "\n") {
			sep = "\n"
		}
		return userConfig + sep + embBlock, nil
	}
	return userConfig[:userStart] + embBlock + userConfig[userEnd:], nil
}

// extractSection finds the byte range of a top-level [name] section in a TOML
// string. The range starts at the header line and ends at the start of the
// next top-level section (or end of file). Triple-quoted strings are honoured
// so `[bracketed]` text inside a multiline prompt isn't mistaken for a header.
// Subsections like [name.sub] are treated as part of the parent section.
func extractSection(s, name string) (start, end int, ok bool) {
	header := "[" + name + "]"
	subPrefix := "[" + name + "."

	inTriple := false
	lineOffset := 0
	inSection := false

	for _, line := range strings.SplitAfter(s, "\n") {
		// Toggle triple-quoted state for each """ on this line.
		for rest := line; ; {
			idx := strings.Index(rest, `"""`)
			if idx < 0 {
				break
			}
			inTriple = !inTriple
			rest = rest[idx+3:]
		}

		if !inTriple {
			trimmed := strings.TrimSpace(line)
			if !inSection {
				if trimmed == header {
					start = lineOffset
					inSection = true
				}
			} else if strings.HasPrefix(trimmed, "[") && trimmed != header && !strings.HasPrefix(trimmed, subPrefix) {
				end = lineOffset
				return start, end, true
			}
		}
		lineOffset += len(line)
	}
	if inSection {
		return start, len(s), true
	}
	return 0, 0, false
}

// PromptHashFromTOML returns the hash identifying a config's [prompts] section,
// the identity the upgrade mechanism and prompt_history.txt are keyed on.
func PromptHashFromTOML(s string) (string, error) { return promptHashFromTOML(s) }

func promptHashFromTOML(s string) (string, error) {
	var t struct {
		Prompts map[string]string `toml:"prompts"`
	}
	if _, err := toml.Decode(s, &t); err != nil {
		return "", err
	}
	h := sha256.New()
	for _, k := range []string{"approval", "auto_respond"} {
		h.Write([]byte(k))
		h.Write([]byte{0})
		h.Write([]byte(t.Prompts[k]))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func trimWhitespace(b []byte) []byte {
	start, end := 0, len(b)
	for start < end && isSpace(b[start]) {
		start++
	}
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
