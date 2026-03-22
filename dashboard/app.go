package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"pilot-dashboard/internal/pilot"
)

type App struct {
	ctx  context.Context
	port int
}

func NewApp() *App {
	return &App{port: 9721}
}

func (a *App) Startup(ctx context.Context) {
	a.ctx = ctx
	setDockIcon(icon)

	cfg, err := pilot.ReadPilotConfig()
	if err == nil && cfg.General.SSEPort != 0 {
		a.port = cfg.General.SSEPort
	}
}

func (a *App) Shutdown(ctx context.Context) {}

func (a *App) baseURL() string {
	return fmt.Sprintf("http://localhost:%d", a.port)
}

func (a *App) isServerRunning() bool {
	client := &http.Client{Timeout: 300 * time.Millisecond}
	resp, err := client.Get(a.baseURL() + "/status")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// GetPilotStatus returns current state. Never blocks, never auto-starts.
func (a *App) GetPilotStatus() pilot.PilotStatus {
	status := pilot.PilotStatus{
		RecentActions: []pilot.PilotAction{},
		SSEPort:       a.port,
	}

	status.HooksInstalled = pilot.CheckHooksInstalled().Installed

	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(a.baseURL() + "/status")
	if err != nil {
		// Server not running — that's fine, just report the state
		return status
	}
	defer resp.Body.Close()

	status.Available = true
	status.SSEAvailable = true

	var state map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return status
	}

	status.SessionActive, _ = state["session_active"].(bool)
	if s, ok := state["session_start"].(string); ok {
		status.SessionStart = &s
	}
	_, status.HasPendingResponse = state["pending_response"].(map[string]any)

	if statsVal, ok := state["stats"].(map[string]any); ok {
		status.Stats = pilot.PilotStats{
			ApprovalsAuto:        uint64Val(statsVal, "approvals_auto"),
			ApprovalsEscalated:   uint64Val(statsVal, "approvals_escalated"),
			AutoResponses:        uint64Val(statsVal, "auto_responses"),
			AutoResponsesSkipped: uint64Val(statsVal, "auto_responses_skipped"),
		}
	}

	if arr, ok := state["recent_actions"].([]any); ok {
		start := 0
		if len(arr) > 20 {
			start = len(arr) - 20
		}
		for i := len(arr) - 1; i >= start; i-- {
			item, ok := arr[i].(map[string]any)
			if !ok {
				continue
			}
			action := pilot.PilotAction{
				Timestamp:  strVal(item, "timestamp"),
				ActionType: strVal(item, "action_type"),
				Detail:     strVal(item, "detail"),
			}
			if c, ok := item["confidence"].(float64); ok {
				action.Confidence = &c
			}
			status.RecentActions = append(status.RecentActions, action)
		}
	}
	if status.RecentActions == nil {
		status.RecentActions = []pilot.PilotAction{}
	}

	return status
}

// InstallPilotHooks installs hooks and starts server.
func (a *App) InstallPilotHooks() error {
	if err := pilot.InstallHooks(); err != nil {
		return err
	}
	if !a.isServerRunning() {
		return pilot.StartServe()
	}
	return nil
}

// UninstallPilotHooks removes hooks and stops server.
func (a *App) UninstallPilotHooks() error {
	_ = pilot.StopServe()
	_ = pilot.KillLingering()
	return pilot.UninstallHooks()
}

func (a *App) GetPilotConfig() (pilot.PilotConfig, error) {
	return pilot.ReadPilotConfig()
}

func (a *App) SavePilotConfig(cfg pilot.PilotConfig) error {
	return pilot.WritePilotConfig(cfg)
}

type LogEntry struct {
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Source    string `json:"source"`
	Message   string `json:"message"`
}

func (a *App) GetPilotLogs() []LogEntry {
	client := &http.Client{Timeout: 500 * time.Millisecond}
	resp, err := client.Get(a.baseURL() + "/logs")
	if err != nil {
		return []LogEntry{}
	}
	defer resp.Body.Close()

	var logs []LogEntry
	json.NewDecoder(resp.Body).Decode(&logs)
	if logs == nil {
		return []LogEntry{}
	}
	return logs
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
