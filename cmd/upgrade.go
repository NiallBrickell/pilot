package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"time"

	"github.com/erdoai/pilot/internal/paths"
	"github.com/spf13/cobra"
)

// releaseRepo is the GitHub repo that publishes pilot release binaries.
const releaseRepo = "erdoai/pilot"

func init() {
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Download the latest release binary from GitHub and restart pilot",
		RunE:  runUpgrade,
	}
	cmd.Flags().Bool("force", false, "reinstall even if already on the latest version")
	rootCmd.AddCommand(cmd)
}

// assetName maps a GOOS/GOARCH pair to the release asset filename built by
// .github/workflows/release.yml.
func assetName(goos, goarch string) (string, error) {
	switch goos {
	case "darwin", "linux":
	default:
		return "", fmt.Errorf("unsupported OS %q (pilot releases cover darwin and linux)", goos)
	}
	switch goarch {
	case "amd64", "arm64":
	default:
		return "", fmt.Errorf("unsupported architecture %q (pilot releases cover amd64 and arm64)", goarch)
	}
	return fmt.Sprintf("pilot-%s-%s", goos, goarch), nil
}

type ghRelease struct {
	TagName string `json:"tag_name"`
	Assets  []struct {
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
	} `json:"assets"`
}

// latestRelease fetches the newest release and returns its tag plus the
// download URL for the given asset. api.github.com rejects requests without a
// User-Agent, so one is always set.
func latestRelease(client *http.Client, repo, asset string) (tag, url string, err error) {
	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/"+repo+"/releases/latest", nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("User-Agent", "pilot-upgrade")
	req.Header.Set("Accept", "application/vnd.github+json")

	resp, err := client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("GitHub API returned %d: %s", resp.StatusCode, string(body))
	}

	var rel ghRelease
	if err := json.Unmarshal(body, &rel); err != nil {
		return "", "", fmt.Errorf("parse release JSON: %w", err)
	}
	for _, a := range rel.Assets {
		if a.Name == asset {
			return rel.TagName, a.BrowserDownloadURL, nil
		}
	}
	return rel.TagName, "", fmt.Errorf("release %s has no asset %q", rel.TagName, asset)
}

func runUpgrade(cmd *cobra.Command, args []string) error {
	force, _ := cmd.Flags().GetBool("force")

	asset, err := assetName(runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}

	client := &http.Client{Timeout: 60 * time.Second}
	fmt.Printf("Checking %s for the latest release…\n", releaseRepo)
	tag, url, err := latestRelease(client, releaseRepo, asset)
	if err != nil {
		return err
	}

	if tag == Version && !force {
		fmt.Printf("Already on the latest version (%s). Use --force to reinstall.\n", Version)
		return nil
	}

	if err := os.MkdirAll(paths.BinDir(), 0755); err != nil {
		return fmt.Errorf("create %s: %w", paths.BinDir(), err)
	}

	// Download to a temp file in the same dir so the final swap is an atomic
	// rename (same filesystem). Never overwrite the target in place: macOS
	// SIGKILLs execs from a vnode whose running binary's code pages were
	// rewritten underneath it.
	tmp, err := os.CreateTemp(paths.BinDir(), "pilot-download-*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once renamed away

	fmt.Printf("Downloading %s (%s)…\n", asset, tag)
	if err := download(client, url, tmp); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpPath, 0755); err != nil {
		return fmt.Errorf("chmod: %w", err)
	}

	// Smoke-test the downloaded binary before trusting it. Use --help, which
	// every release supports and exits 0, to catch a corrupt or wrong-arch
	// download without depending on flags newer releases may add.
	if out, err := exec.Command(tmpPath, "--help").CombinedOutput(); err != nil {
		return fmt.Errorf("downloaded binary failed to run (%w): %s", err, string(out))
	}

	dest := paths.InstalledBinPath()
	if err := os.Rename(tmpPath, dest); err != nil {
		return fmt.Errorf("install to %s: %w", dest, err)
	}
	fmt.Printf("Installed %s to %s\n", tag, dest)

	// Re-exec the freshly installed binary's `start`, which repoints hooks at
	// this stable path and restarts serve.
	start := exec.Command(dest, "start")
	start.Stdout = os.Stdout
	start.Stderr = os.Stderr
	if err := start.Run(); err != nil {
		return fmt.Errorf("installed pilot, but `start` failed: %w", err)
	}
	return nil
}

func download(client *http.Client, url string, w io.Writer) error {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "pilot-upgrade")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download returned %d", resp.StatusCode)
	}
	if _, err := io.Copy(w, resp.Body); err != nil {
		return fmt.Errorf("write download: %w", err)
	}
	return nil
}
