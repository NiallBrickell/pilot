package cmd

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/NiallBrickell/pilot/internal/paths"
	"github.com/spf13/cobra"
)

const (
	dashboardRepo        = "NiallBrickell/pilot"
	dashboardVersionFile = "pilot-dashboard.version"
)

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "dashboard",
		Short: "Launch the Pilot dashboard GUI",
		RunE:  runDashboard,
	})
}

func runDashboard(cmd *cobra.Command, args []string) error {
	paths.RecordBinaryPath()

	// Make sure pilot is fully set up before opening the GUI so the dashboard
	// opens to a working state. runStart is idempotent: it (re)installs hooks
	// and (re)starts the serve process. If it fails (e.g. missing API key) we
	// abort so the user sees the real error instead of a silently-broken GUI.
	if err := runStart(cmd, args); err != nil {
		return err
	}

	appPath := findDashboardApp()
	latest, latestErr := latestDashboardTag()
	installed := installedDashboardVersion()

	needDownload := appPath == "" || (latestErr == nil && installed != latest)

	if needDownload {
		if latestErr != nil {
			return fmt.Errorf("dashboard not installed and couldn't check latest release: %w\n\nDownload manually from https://github.com/%s/releases", latestErr, dashboardRepo)
		}
		if appPath == "" {
			fmt.Printf("Downloading dashboard %s...\n", latest)
		} else {
			shown := installed
			if shown == "" {
				shown = "unknown"
			}
			fmt.Printf("Updating dashboard (%s -> %s)...\n", shown, latest)
		}
		var err error
		appPath, err = downloadDashboard(latest)
		if err != nil {
			return fmt.Errorf("failed to download dashboard: %w\n\nDownload manually from https://github.com/%s/releases", err, dashboardRepo)
		}
		if err := writeInstalledDashboardVersion(latest); err != nil {
			fmt.Fprintf(os.Stderr, "warning: couldn't record dashboard version: %v\n", err)
		}
	} else if latestErr != nil {
		fmt.Fprintln(os.Stderr, "warning: couldn't check for dashboard updates, using local copy")
	}

	fmt.Println("Launching dashboard...")
	return launchDashboard(appPath)
}

func latestDashboardTag() (string, error) {
	client := &http.Client{
		Timeout: 5 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequest("HEAD", fmt.Sprintf("https://github.com/%s/releases/latest", dashboardRepo), nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	loc := resp.Header.Get("Location")
	if loc == "" {
		return "", fmt.Errorf("no redirect from releases/latest (HTTP %d — has a release been published?)", resp.StatusCode)
	}
	idx := strings.LastIndex(loc, "/")
	if idx == -1 || idx == len(loc)-1 {
		return "", fmt.Errorf("malformed release URL: %s", loc)
	}
	return loc[idx+1:], nil
}

func installedDashboardVersion() string {
	data, _ := os.ReadFile(filepath.Join(paths.PilotDir(), dashboardVersionFile))
	return strings.TrimSpace(string(data))
}

func writeInstalledDashboardVersion(tag string) error {
	return os.WriteFile(filepath.Join(paths.PilotDir(), dashboardVersionFile), []byte(tag), 0644)
}

func findDashboardApp() string {
	pilotDir := paths.PilotDir()

	if runtime.GOOS == "darwin" {
		// Check for .app bundle
		appPath := filepath.Join(pilotDir, "pilot-dashboard.app")
		if info, err := os.Stat(appPath); err == nil && info.IsDir() {
			return appPath
		}
	}

	// Check for plain binary
	binPath := filepath.Join(pilotDir, "pilot-dashboard")
	if _, err := os.Stat(binPath); err == nil {
		return binPath
	}

	return ""
}

func downloadDashboard(tag string) (string, error) {
	paths.EnsureDir()

	var assetName string
	switch runtime.GOOS {
	case "darwin":
		assetName = "pilot-dashboard-macos.zip"
	case "linux":
		assetName = "pilot-dashboard-linux-amd64.tar.gz"
	default:
		return "", fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}

	url := fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", dashboardRepo, tag, assetName)

	resp, err := http.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return "", fmt.Errorf("download failed: HTTP %d — is there a release published?", resp.StatusCode)
	}

	// Save archive to temp file
	tmpFile, err := os.CreateTemp("", "pilot-dashboard-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmpFile.Name())

	if _, err := io.Copy(tmpFile, resp.Body); err != nil {
		tmpFile.Close()
		return "", err
	}
	tmpFile.Close()

	pilotDir := paths.PilotDir()

	// Extract
	switch runtime.GOOS {
	case "darwin":
		// Wipe any previous install so stale files from older bundles don't linger.
		appPath := filepath.Join(pilotDir, "pilot-dashboard.app")
		if err := os.RemoveAll(appPath); err != nil {
			return "", fmt.Errorf("failed to remove existing dashboard: %w", err)
		}
		cmd := exec.Command("unzip", "-o", "-q", tmpFile.Name(), "-d", pilotDir)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("unzip failed: %s: %w", string(out), err)
		}
		return appPath, nil

	case "linux":
		cmd := exec.Command("tar", "xzf", tmpFile.Name(), "-C", pilotDir)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("extract failed: %s: %w", string(out), err)
		}
		return filepath.Join(pilotDir, "pilot-dashboard"), nil
	}

	return "", fmt.Errorf("unsupported platform: %s", runtime.GOOS)
}

func launchDashboard(appPath string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", appPath).Start()
	default:
		cmd := exec.Command(appPath)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		return cmd.Start()
	}
}
