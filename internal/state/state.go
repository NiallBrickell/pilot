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
	DurationMs *float64   `json:"duration_ms,omitempty"`
	Source     string     `json:"source,omitempty"`
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
			duration_ms REAL,
			source TEXT,
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

		db.Exec(`CREATE TABLE IF NOT EXISTS logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			timestamp TEXT NOT NULL,
			level TEXT NOT NULL,
			source TEXT NOT NULL,
			message TEXT NOT NULL
		)`)

		db.Exec(`CREATE INDEX IF NOT EXISTS idx_logs_timestamp ON logs (timestamp DESC)`)

		// Ensure stat rows exist
		for _, key := range []string{"approvals_auto", "approvals_escalated", "auto_responses", "auto_responses_skipped"} {
			db.Exec(`INSERT OR IGNORE INTO stats (key, value) VALUES (?, 0)`, key)
		}

		// Migrations: add columns that may not exist in older databases.
		db.Exec(`ALTER TABLE actions ADD COLUMN duration_ms REAL`)
		db.Exec(`ALTER TABLE actions ADD COLUMN source TEXT`)

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
		`INSERT INTO actions (timestamp, action_type, detail, confidence, duration_ms, source, tool_name, tool_input, cwd, session_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		action.Timestamp.Format(time.RFC3339Nano),
		string(action.ActionType),
		action.Detail,
		confidence,
		action.DurationMs,
		action.Source,
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

	// Recent actions (last 200, from the last hour only)
	cutoff := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339Nano)
	actionRows, err := db.Query(
		`SELECT timestamp, action_type, detail, confidence, duration_ms, source, tool_name, tool_input, cwd, session_id
		 FROM actions WHERE timestamp > ? ORDER BY id DESC LIMIT 200`, cutoff)
	if err == nil {
		defer actionRows.Close()
		for actionRows.Next() {
			var ts, actionType, detail string
			var confidence, durationMs *float64
			var source, toolName, toolInput, cwd, sessionID *string
			actionRows.Scan(&ts, &actionType, &detail, &confidence, &durationMs, &source, &toolName, &toolInput, &cwd, &sessionID)

			t, _ := time.Parse(time.RFC3339Nano, ts)
			action := PilotAction{
				Timestamp:  t,
				ActionType: ActionType(actionType),
				Detail:     detail,
				Confidence: confidence,
				DurationMs: durationMs,
			}
			if source != nil {
				action.Source = *source
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

// ProfileStats holds aggregated evaluation timing data.
type ProfileStats struct {
	Source string  `json:"source"`
	Count  int     `json:"count"`
	AvgMs  float64 `json:"avg_ms"`
	P50Ms  float64 `json:"p50_ms"`
	P95Ms  float64 `json:"p95_ms"`
	P99Ms  float64 `json:"p99_ms"`
	MinMs  float64 `json:"min_ms"`
	MaxMs  float64 `json:"max_ms"`
}

// ReadProfile returns evaluation timing stats grouped by source.
func ReadProfile(limit int) []ProfileStats {
	db := getDB()
	if limit <= 0 {
		limit = 1000
	}

	rows, err := db.Query(`
		SELECT source,
			COUNT(*) as cnt,
			AVG(duration_ms) as avg_ms,
			MIN(duration_ms) as min_ms,
			MAX(duration_ms) as max_ms
		FROM actions
		WHERE duration_ms IS NOT NULL AND source IS NOT NULL AND source != ''
		GROUP BY source
		ORDER BY cnt DESC
	`)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var stats []ProfileStats
	for rows.Next() {
		var s ProfileStats
		rows.Scan(&s.Source, &s.Count, &s.AvgMs, &s.MinMs, &s.MaxMs)
		stats = append(stats, s)
	}

	// Compute percentiles per source from raw data
	for i, s := range stats {
		pRows, err := db.Query(`
			SELECT duration_ms FROM actions
			WHERE duration_ms IS NOT NULL AND source = ?
			ORDER BY duration_ms ASC
			LIMIT ?
		`, s.Source, limit)
		if err != nil {
			continue
		}
		var durations []float64
		for pRows.Next() {
			var d float64
			pRows.Scan(&d)
			durations = append(durations, d)
		}
		pRows.Close()

		if len(durations) > 0 {
			stats[i].P50Ms = percentile(durations, 50)
			stats[i].P95Ms = percentile(durations, 95)
			stats[i].P99Ms = percentile(durations, 99)
		}
	}

	return stats
}

// ReadProfileAll returns overall timing stats (not grouped by source).
func ReadProfileAll(limit int) *ProfileStats {
	db := getDB()
	if limit <= 0 {
		limit = 1000
	}

	var s ProfileStats
	s.Source = "all"
	err := db.QueryRow(`
		SELECT COUNT(*), COALESCE(AVG(duration_ms), 0), COALESCE(MIN(duration_ms), 0), COALESCE(MAX(duration_ms), 0)
		FROM actions WHERE duration_ms IS NOT NULL
	`).Scan(&s.Count, &s.AvgMs, &s.MinMs, &s.MaxMs)
	if err != nil || s.Count == 0 {
		return nil
	}

	pRows, err := db.Query(`
		SELECT duration_ms FROM actions
		WHERE duration_ms IS NOT NULL
		ORDER BY duration_ms ASC
		LIMIT ?
	`, limit)
	if err != nil {
		return &s
	}
	defer pRows.Close()

	var durations []float64
	for pRows.Next() {
		var d float64
		pRows.Scan(&d)
		durations = append(durations, d)
	}
	if len(durations) > 0 {
		s.P50Ms = percentile(durations, 50)
		s.P95Ms = percentile(durations, 95)
		s.P99Ms = percentile(durations, 99)
	}

	return &s
}

func percentile(sorted []float64, p int) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := float64(p) / 100.0 * float64(len(sorted)-1)
	lower := int(idx)
	if lower >= len(sorted)-1 {
		return sorted[len(sorted)-1]
	}
	frac := idx - float64(lower)
	return sorted[lower]*(1-frac) + sorted[lower+1]*frac
}

// LogEntry represents a debug log line.
type LogEntry struct {
	Timestamp string `json:"timestamp"`
	Level     string `json:"level"`
	Source    string `json:"source"`
	Message   string `json:"message"`
}

// WriteLog records a debug log entry.
func WriteLog(level, source, message string) {
	db := getDB()
	db.Exec(`INSERT INTO logs (timestamp, level, source, message) VALUES (?, ?, ?, ?)`,
		time.Now().UTC().Format(time.RFC3339Nano), level, source, message)

	// Keep last 500 logs
	db.Exec(`DELETE FROM logs WHERE id NOT IN (SELECT id FROM logs ORDER BY id DESC LIMIT 500)`)
}

// ReadLogs returns recent log entries.
func ReadLogs(limit int) []LogEntry {
	db := getDB()
	if limit <= 0 {
		limit = 100
	}
	rows, err := db.Query(`SELECT timestamp, level, source, message FROM logs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()

	var entries []LogEntry
	for rows.Next() {
		var e LogEntry
		rows.Scan(&e.Timestamp, &e.Level, &e.Source, &e.Message)
		entries = append(entries, e)
	}
	return entries
}
