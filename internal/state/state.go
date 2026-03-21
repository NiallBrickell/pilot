package state

import (
	"encoding/json"
	"os"
	"time"

	"github.com/NiallBrickell/pilot/internal/config"
)

type ActionType string

const (
	AutoApprove         ActionType = "auto_approve"
	Escalate            ActionType = "escalate"
	AutoRespond         ActionType = "auto_respond"
	AutoRespondSkipped  ActionType = "auto_respond_skipped"
)

type PilotState struct {
	SessionActive   bool             `json:"session_active"`
	SessionStart    *time.Time       `json:"session_start"`
	Stats           SessionStats     `json:"stats"`
	RecentActions   []PilotAction    `json:"recent_actions"`
	PendingResponse *PendingResponse `json:"pending_response"`
}

type PendingResponse struct {
	Message    string    `json:"message"`
	Confidence float64   `json:"confidence"`
	Timestamp  time.Time `json:"timestamp"`
}

type SessionStats struct {
	ApprovalsAuto        uint64 `json:"approvals_auto"`
	ApprovalsEscalated   uint64 `json:"approvals_escalated"`
	AutoResponses        uint64 `json:"auto_responses"`
	AutoResponsesSkipped uint64 `json:"auto_responses_skipped"`
}

type PilotAction struct {
	Timestamp  time.Time  `json:"timestamp"`
	ActionType ActionType `json:"action_type"`
	Detail     string     `json:"detail"`
	Confidence *float64   `json:"confidence"`
}

func ReadState() (PilotState, error) {
	path := config.StateFilePath()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return defaultState(), nil
		}
		return defaultState(), err
	}
	var s PilotState
	if err := json.Unmarshal(data, &s); err != nil {
		return defaultState(), err
	}
	if s.RecentActions == nil {
		s.RecentActions = []PilotAction{}
	}
	return s, nil
}

func defaultState() PilotState {
	return PilotState{
		RecentActions: []PilotAction{},
	}
}

func WriteState(s *PilotState) error {
	path := config.StateFilePath()
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

func RecordAction(action PilotAction) error {
	s, _ := ReadState()

	switch action.ActionType {
	case AutoApprove:
		s.Stats.ApprovalsAuto++
	case Escalate:
		s.Stats.ApprovalsEscalated++
	case AutoRespond:
		s.Stats.AutoResponses++
	case AutoRespondSkipped:
		s.Stats.AutoResponsesSkipped++
	}

	s.RecentActions = append(s.RecentActions, action)
	if len(s.RecentActions) > 100 {
		s.RecentActions = s.RecentActions[len(s.RecentActions)-100:]
	}

	return WriteState(&s)
}
