package pilot

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type PilotStatus struct {
	Available          bool          `json:"available"`
	SessionActive      bool          `json:"session_active"`
	SessionStart       *string       `json:"session_start"`
	Stats              PilotStats    `json:"stats"`
	RecentActions      []PilotAction `json:"recent_actions"`
	HasPendingResponse bool          `json:"has_pending_response"`
	HooksInstalled     bool          `json:"hooks_installed"`
	WrapperRunning     bool          `json:"wrapper_running"`
	SSEAvailable       bool          `json:"sse_available"`
	SSEPort            int           `json:"sse_port"`
}

type PilotStats struct {
	ApprovalsAuto        uint64 `json:"approvals_auto"`
	ApprovalsEscalated   uint64 `json:"approvals_escalated"`
	AutoResponses        uint64 `json:"auto_responses"`
	AutoResponsesSkipped uint64 `json:"auto_responses_skipped"`
}

type PilotAction struct {
	Timestamp  string   `json:"timestamp"`
	ActionType string   `json:"action_type"`
	Detail     string   `json:"detail"`
	Confidence *float64 `json:"confidence"`
}

func pilotStatePath() string {
	return filepath.Join(pilotDir(), "state.json")
}

func pilotBinaryExists() bool {
	_, err := findPilotBinary()
	return err == nil
}

func findPilotBinary() (string, error) {
	// Check PATH first
	if p, err := exec.LookPath("pilot"); err == nil {
		return p, nil
	}
	// Check ~/.pilot/pilot
	p := filepath.Join(pilotDir(), "pilot")
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("pilot binary not found")
}

func GetPilotStatus() PilotStatus {
	available := pilotBinaryExists()

	path := pilotStatePath()
	data, err := os.ReadFile(path)
	if err != nil {
		return PilotStatus{
			Available:     available,
			RecentActions: []PilotAction{},
		}
	}

	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		return PilotStatus{
			Available:     available,
			RecentActions: []PilotAction{},
		}
	}

	statsVal, _ := state["stats"].(map[string]any)

	var actions []PilotAction
	if arr, ok := state["recent_actions"].([]any); ok {
		start := 0
		if len(arr) > 20 {
			start = len(arr) - 20
		}
		for i := len(arr) - 1; i >= start; i-- {
			a, ok := arr[i].(map[string]any)
			if !ok {
				continue
			}
			action := PilotAction{
				Timestamp:  strVal(a, "timestamp"),
				ActionType: strVal(a, "action_type"),
				Detail:     strVal(a, "detail"),
			}
			if c, ok := a["confidence"].(float64); ok {
				action.Confidence = &c
			}
			actions = append(actions, action)
		}
	}
	if actions == nil {
		actions = []PilotAction{}
	}

	var sessionStart *string
	if s, ok := state["session_start"].(string); ok {
		sessionStart = &s
	}

	sessionActive, _ := state["session_active"].(bool)
	_, hasPending := state["pending_response"].(map[string]any)

	hookStatus := CheckHooksInstalled()
	wrapperRunning := IsWrapperRunning() || IsServeRunning()
	ssePort := readSSEPort()
	sseAvailable := checkSSEAvailable(ssePort)

	return PilotStatus{
		Available:     available,
		SessionActive: sessionActive,
		SessionStart:  sessionStart,
		Stats: PilotStats{
			ApprovalsAuto:        uint64Val(statsVal, "approvals_auto"),
			ApprovalsEscalated:   uint64Val(statsVal, "approvals_escalated"),
			AutoResponses:        uint64Val(statsVal, "auto_responses"),
			AutoResponsesSkipped: uint64Val(statsVal, "auto_responses_skipped"),
		},
		RecentActions:      actions,
		HasPendingResponse: hasPending,
		HooksInstalled:     hookStatus.Installed,
		WrapperRunning:     wrapperRunning,
		SSEAvailable:       sseAvailable,
		SSEPort:            ssePort,
	}
}

func strVal(m map[string]any, key string) string {
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

func uint64Val(m map[string]any, key string) uint64 {
	if m == nil {
		return 0
	}
	if v, ok := m[key].(float64); ok {
		return uint64(v)
	}
	return 0
}

func checkSSEAvailable(port int) bool {
	client := &http.Client{Timeout: 200 * time.Millisecond}
	resp, err := client.Get(fmt.Sprintf("http://localhost:%d/status", port))
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
