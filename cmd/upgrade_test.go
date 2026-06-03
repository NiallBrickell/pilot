package cmd

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAssetName(t *testing.T) {
	cases := []struct {
		goos, goarch, want string
		wantErr            bool
	}{
		{"darwin", "arm64", "pilot-darwin-arm64", false},
		{"darwin", "amd64", "pilot-darwin-amd64", false},
		{"linux", "amd64", "pilot-linux-amd64", false},
		{"linux", "arm64", "pilot-linux-arm64", false},
		{"windows", "amd64", "", true},
		{"darwin", "386", "", true},
	}
	for _, c := range cases {
		got, err := assetName(c.goos, c.goarch)
		if c.wantErr {
			if err == nil {
				t.Errorf("assetName(%q,%q): expected error, got %q", c.goos, c.goarch, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("assetName(%q,%q): unexpected error %v", c.goos, c.goarch, err)
		}
		if got != c.want {
			t.Errorf("assetName(%q,%q) = %q, want %q", c.goos, c.goarch, got, c.want)
		}
	}
}

func TestLatestRelease(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		w.Write([]byte(`{
			"tag_name": "v9.9.9",
			"assets": [
				{"name": "pilot-linux-amd64", "browser_download_url": "https://example.test/linux"},
				{"name": "pilot-darwin-arm64", "browser_download_url": "https://example.test/darwin-arm64"}
			]
		}`))
	}))
	defer srv.Close()

	// latestRelease hardcodes api.github.com, so exercise the parsing via a
	// transport that redirects to the test server.
	client := &http.Client{
		Timeout:   5 * time.Second,
		Transport: rewriteTransport{srv.URL},
	}

	tag, url, err := latestRelease(client, "erdoai/pilot", "pilot-darwin-arm64")
	if err != nil {
		t.Fatalf("latestRelease: %v", err)
	}
	if tag != "v9.9.9" {
		t.Errorf("tag = %q, want v9.9.9", tag)
	}
	if url != "https://example.test/darwin-arm64" {
		t.Errorf("url = %q, want darwin-arm64 url", url)
	}
	if gotUA == "" {
		t.Error("expected a User-Agent header to be sent (api.github.com rejects requests without one)")
	}

	if _, _, err := latestRelease(client, "erdoai/pilot", "pilot-no-such-asset"); err == nil {
		t.Error("expected error for missing asset")
	}
}

func TestLatestReleaseHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		w.Write([]byte("rate limited"))
	}))
	defer srv.Close()
	client := &http.Client{Transport: rewriteTransport{srv.URL}}
	_, _, err := latestRelease(client, "erdoai/pilot", "pilot-darwin-arm64")
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Errorf("expected 403 error, got %v", err)
	}
}

// rewriteTransport redirects every request to the test server host while
// preserving headers, so we can test latestRelease without real network.
type rewriteTransport struct{ base string }

func (rt rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	target, _ := http.NewRequest(req.Method, rt.base+req.URL.Path, req.Body)
	target.Header = req.Header
	return http.DefaultTransport.RoundTrip(target)
}
