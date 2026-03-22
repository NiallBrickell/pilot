package state

import (
	"database/sql"
	"path/filepath"
	"sync"
	"time"

	"github.com/NiallBrickell/pilot/internal/paths"
	_ "modernc.org/sqlite"
)

type ActionType string

const (
	AutoApprove        ActionType = "auto_approve"
	Escalate           ActionType = "escalate"
	AutoRespond        ActionType = "auto_respond"
	AutoRespondSkipped ActionType = "auto_respond_skipped"
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
	ToolName   string     `json:"tool_name,omitempty"`
	ToolInput  string     `json:"tool_input,omitempty"`
	Cwd        string     `json:"cwd,omitempty"`
	SessionID  string     `json:"session_id,omitempty"`
}

var (
	db     *sql.DB
	dbOnce sync.Once
)

func dbPath() string {
	return filepath.Join(paths.PilotDir(), "pilot.db")
}

func getDB() *sql.DB {
	dbOnce.Do(func() {
		paths.EnsureDir()
		var err error
		db, err = sql.Open("sqlite", dbPath()+"?_journal_mode=WAL&_busy_timeout=5000")
		if err != nil {
			panic("failed to open pilot.db: " + err.Error())
		}
		db.SetMaxOpenConns(1) // SQLite is single-writer

		// Create tables
		db.Exec(`CREATE TABLE IF NOT EXISTS actions (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp TEXT NOT NULL,
			action_type TEXT NOT NULL,
			detail TEXT NOT NULL,
			confidence REAL,
			tool_name TEXT,
			tool_input TEXT,
			cwd TEXT,
			session_id TEXT
		)`)

		db.Exec(`CREATE TABLE IF NOT EXISTS stats (
			key TEXT PRIMARY KEY,
			value INTEGER NOT NULL DEFAULT 0
		)`)

		db.Exec(`CREATE TABLE IF NOT EXISTS session (
			key TEXT PRIMARY KEY,
			value TEXT
		)`)

		// Ensure stat rows exist
		for _, key := range []string{"approvals_auto", "approvals_escalated", "auto_responses", "auto_responses_skipped"} {
			db.Exec(`INSERT OR IGNORE INTO stats (key, value) VALUES (?, 0)`, key)
		}

		// Index for recent queries
		db.Exec(`CREATE INDEX IF NOT EXISTS idx_actions_timestamp ON actions (timestamp DESC)`)
	})
	return db
}

func RecordAction(action PilotAction) error {
	db := getDB()

	var confidence *float64
	if action.Confidence != nil {
		confidence = action.Confidence
	}

	_, err := db.Exec(
		`INSERT INTO actions (timestamp, action_type, detail, confidence, tool_name, tool_input, cwd, session_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		action.Timestamp.Format(time.RFC3339Nano),
		string(action.ActionType),
		action.Detail,
		confidence,
		action.ToolName,
		action.ToolInput,
		action.Cwd,
		action.SessionID,
	)
	if err != nil {
		return err
	}

	// Update stats
	var statKey string
	switch action.ActionType {
	case AutoApprove:
		statKey = "approvals_auto"
	case Escalate:
		statKey = "approvals_escalated"
	case AutoRespond:
		statKey = "auto_responses"
	case AutoRespondSkipped:
		statKey = "auto_responses_skipped"
	}
	if statKey != "" {
		db.Exec(`UPDATE stats SET value = value + 1 WHERE key = ?`, statKey)
	}

	return nil
}

func ReadState() (PilotState, error) {
	db := getDB()
	s := PilotState{
		RecentActions: []PilotAction{},
	}

	// Stats
	rows, err := db.Query(`SELECT key, value FROM stats`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var key string
			var value uint64
			rows.Scan(&key, &value)
			switch key {
			case "approvals_auto":
				s.Stats.ApprovalsAuto = value
			case "approvals_escalated":
				s.Stats.ApprovalsEscalated = value
			case "auto_responses":
				s.Stats.AutoResponses = value
			case "auto_responses_skipped":
				s.Stats.AutoResponsesSkipped = value
			}
		}
	}

	// Recent actions (last 200)
	actionRows, err := db.Query(
		`SELECT timestamp, action_type, detail, confidence, tool_name, tool_input, cwd, session_id
		 FROM actions ORDER BY id DESC LIMIT 200`)
	if err == nil {
		defer actionRows.Close()
		for actionRows.Next() {
			var ts, actionType, detail string
			var confidence *float64
			var toolName, toolInput, cwd, sessionID *string
			actionRows.Scan(&ts, &actionType, &detail, &confidence, &toolName, &toolInput, &cwd, &sessionID)

			t, _ := time.Parse(time.RFC3339Nano, ts)
			action := PilotAction{
				Timestamp:  t,
				ActionType: ActionType(actionType),
				Detail:     detail,
				Confidence: confidence,
			}
			if toolName != nil {
				action.ToolName = *toolName
			}
			if toolInput != nil {
				action.ToolInput = *toolInput
			}
			if cwd != nil {
				action.Cwd = *cwd
			}
			if sessionID != nil {
				action.SessionID = *sessionID
			}
			s.RecentActions = append(s.RecentActions, action)
		}
	}

	// Session state
	var activeStr, startStr sql.NullString
	db.QueryRow(`SELECT value FROM session WHERE key = 'active'`).Scan(&activeStr)
	db.QueryRow(`SELECT value FROM session WHERE key = 'start'`).Scan(&startStr)
	s.SessionActive = activeStr.Valid && activeStr.String == "true"
	if startStr.Valid {
		t, err := time.Parse(time.RFC3339Nano, startStr.String)
		if err == nil {
			s.SessionStart = &t
		}
	}

	return s, nil
}

func WriteState(s *PilotState) error {
	db := getDB()

	active := "false"
	if s.SessionActive {
		active = "true"
	}
	db.Exec(`INSERT OR REPLACE INTO session (key, value) VALUES ('active', ?)`, active)

	if s.SessionStart != nil {
		db.Exec(`INSERT OR REPLACE INTO session (key, value) VALUES ('start', ?)`, s.SessionStart.Format(time.RFC3339Nano))
	} else {
		db.Exec(`DELETE FROM session WHERE key = 'start'`)
	}

	if s.PendingResponse != nil {
		db.Exec(`INSERT OR REPLACE INTO session (key, value) VALUES ('pending_message', ?)`, s.PendingResponse.Message)
		db.Exec(`INSERT OR REPLACE INTO session (key, value) VALUES ('pending_confidence', ?)`, s.PendingResponse.Confidence)
	} else {
		db.Exec(`DELETE FROM session WHERE key IN ('pending_message', 'pending_confidence')`)
	}

	return nil
}
