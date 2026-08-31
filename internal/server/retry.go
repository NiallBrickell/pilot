package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"sync"
	"time"
)

// Retry decisions handed from the PermissionDenied hook to the hooks that see
// the model's retry of the same call.
const (
	// RetryAllow: Pilot (or a human via the dashboard) approved the call. The
	// PreToolUse hook answers "allow", which Claude Code honours ahead of the
	// auto-mode classifier, so the retry runs without another classifier pass.
	RetryAllow = "allow"
	// RetryAsk: Pilot could not settle the call (evaluator unavailable, spend
	// cap, or no human answered before the escalation timeout). The PreToolUse
	// hook answers "ask", which forces Claude Code's own permission prompt for
	// the retry; the PermissionRequest hook then declines to decide so that
	// prompt reaches the user instead of being evaluated a second time.
	RetryAsk = "ask"
)

// Stages at which a retry record is claimed.
const (
	RetryStagePreToolUse        = "pre_tool_use"
	RetryStagePermissionRequest = "permission_request"
)

// retryTTL bounds how long a classifier-denied call stays pre-decided. The
// model normally retries within seconds; anything older is stale enough that
// a fresh evaluation is safer than replaying an old verdict.
const retryTTL = 10 * time.Minute

// retryMaxRecords caps memory across a fleet of sessions.
const retryMaxRecords = 1000

// RetryRecord is one classifier-denied tool call Pilot has already decided.
type RetryRecord struct {
	Key       string    `json:"key"`
	SessionID string    `json:"session_id"`
	ToolName  string    `json:"tool_name"`
	ToolInput string    `json:"tool_input"`
	Decision  string    `json:"decision"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
	// prompted is set once PreToolUse has answered "ask" for this record so the
	// PermissionRequest hook knows a prompt is in flight for it.
	prompted bool
	// nudged is set once the Stop hook has told the model to retry, so a session
	// that ignores the nudge is not nudged forever.
	nudged bool
}

// RetryStore keeps classifier-denial decisions until the model retries the
// call. Records are one-shot: a claim that resolves the call removes it.
type RetryStore struct {
	mu    sync.Mutex
	now   func() time.Time
	items map[string]*RetryRecord
}

func NewRetryStore() *RetryStore {
	return &RetryStore{now: time.Now, items: make(map[string]*RetryRecord)}
}

// RetryKey identifies a tool call within a session. Bash's free-text
// "description" is dropped because the model rewrites it on retry; every other
// field must match exactly so a changed command is evaluated afresh.
func RetryKey(sessionID, toolName, toolInput string) string {
	normalized := toolInput
	var parsed map[string]any
	if json.Unmarshal([]byte(toolInput), &parsed) == nil && parsed != nil {
		delete(parsed, "description")
		if b, err := json.Marshal(parsed); err == nil { // map keys marshal sorted
			normalized = string(b)
		}
	}
	sum := sha256.Sum256([]byte(sessionID + "\x00" + toolName + "\x00" + normalized))
	return hex.EncodeToString(sum[:])
}

// Register stores (or replaces) the decision for a call. It returns the key.
func (rs *RetryStore) Register(sessionID, toolName, toolInput, decision, reason string) string {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.sweepLocked()

	key := RetryKey(sessionID, toolName, toolInput)
	rs.items[key] = &RetryRecord{
		Key:       key,
		SessionID: sessionID,
		ToolName:  toolName,
		ToolInput: toolInput,
		Decision:  decision,
		Reason:    reason,
		CreatedAt: rs.now(),
	}
	if len(rs.items) > retryMaxRecords {
		rs.evictOldestLocked()
	}
	return key
}

// Claim returns the decision for a call at the given stage, or nil when Pilot
// has nothing pending for it.
//
// PreToolUse consumes an "allow" (the call runs now) but only marks an "ask"
// as prompted, because the PermissionRequest hook fires next for that same
// call and must find the record. PermissionRequest consumes whatever remains.
func (rs *RetryStore) Claim(sessionID, toolName, toolInput, stage string) *RetryRecord {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.sweepLocked()

	key := RetryKey(sessionID, toolName, toolInput)
	rec, ok := rs.items[key]
	if !ok {
		return nil
	}
	switch stage {
	case RetryStagePreToolUse:
		if rec.Decision == RetryAsk {
			rec.prompted = true
		} else {
			delete(rs.items, key)
		}
	default:
		delete(rs.items, key)
	}
	copy := *rec
	return &copy
}

// Nudge returns one record for the session that the model has not retried yet
// and that has not already produced a nudge, marking it nudged. Records with a
// prompt in flight are skipped: the user, not the model, holds those.
func (rs *RetryStore) Nudge(sessionID string) *RetryRecord {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.sweepLocked()

	var candidates []*RetryRecord
	for _, rec := range rs.items {
		if rec.SessionID == sessionID && !rec.nudged && !rec.prompted {
			candidates = append(candidates, rec)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].CreatedAt.Before(candidates[j].CreatedAt) })
	rec := candidates[0]
	rec.nudged = true
	copy := *rec
	return &copy
}

// Len reports live records; used by tests and status output.
func (rs *RetryStore) Len() int {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.sweepLocked()
	return len(rs.items)
}

func (rs *RetryStore) sweepLocked() {
	cutoff := rs.now().Add(-retryTTL)
	for key, rec := range rs.items {
		if rec.CreatedAt.Before(cutoff) {
			delete(rs.items, key)
		}
	}
}

func (rs *RetryStore) evictOldestLocked() {
	var oldestKey string
	var oldest time.Time
	for key, rec := range rs.items {
		if oldestKey == "" || rec.CreatedAt.Before(oldest) {
			oldestKey, oldest = key, rec.CreatedAt
		}
	}
	if oldestKey != "" {
		delete(rs.items, oldestKey)
	}
}
