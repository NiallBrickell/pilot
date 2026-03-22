package cmd

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/NiallBrickell/pilot/internal/paths"
	"github.com/spf13/cobra"
)

const dashboardRepo = "NiallBrickell/pilot"

func init() {
	rootCmd.AddCommand(&cobra.Command{
		Use:   "dashboard",
		Short: "Launch the Pilot dashboard GUI",
		RunE:  runDashboard,
	})
}

func runDashboard(cmd *cobra.Command, args []string) error {
	appPath := findDashboardApp()
	if appPath == "" {
		fmt.Println("Dashboard not found locally. Downloading from GitHub releases...")
		var err error
		appPath, err = downloadDashboard()
		if err != nil {
			return fmt.Errorf("failed to download dashboard: %w\n\nDownload manually from https://github.com/"+dashboardRepo+"/releases", err)
		}
		fmt.Printf("Downloaded to %s\n", appPath)
	}

	fmt.Println("Launching dashboard...")
	return launchDashboard(appPath)
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

func downloadDashboard() (string, error) {
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

	// Get latest release download URL
	url := fmt.Sprintf("https://github.com/%s/releases/latest/download/%s", dashboardRepo, assetName)

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
		// Unzip .app bundle
		cmd := exec.Command("unzip", "-o", "-q", tmpFile.Name(), "-d", pilotDir)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("unzip failed: %s: %w", string(out), err)
		}
		return filepath.Join(pilotDir, "pilot-dashboard.app"), nil

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
