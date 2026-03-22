# pilot

AI copilot for Claude Code sessions — auto-approves safe tool calls, escalates dangerous ones, and nudges Claude when it stops unnecessarily. Designed for running 20+ simultaneous sessions without babysitting.

## What it does

1. **Three-layer approval** — tool calls go through Claude Code settings → pilot rules → Haiku evaluation. Most calls resolve without an LLM call.
2. **Escalation** — dangerous calls are held for human approval (via dashboard, webhook, or future TUI). If no response within the timeout, Claude prompts normally.
3. **Idle detection** — when Claude stops unnecessarily, Haiku evaluates the transcript and auto-responds with context-aware nudges like "run the tests" or "keep going".
4. **Interrogation** — periodically checks if Claude is still on track. If it's going in circles or ignoring instructions, pilot redirects it.
5. **Webhooks** — POST events to your own HTTP endpoints for custom integrations, dashboards, or logging.

## Architecture

```
Claude Code session (any of 20+)
    │
    ├─ PreToolUse hook ──→ pilot approve
    │                         │
    │                         POST to pilot serve
    │                         ├─ Layer 1: Claude Code settings (no LLM)
    │                         ├─ Layer 2: Pilot rules (no LLM)
    │                         ├─ Layer 3: Haiku via Agent SDK
    │                         │
    │                         ├─ Approved → "allow"
    │                         ├─ Escalated → wait for human (timeout configurable)
    │                         └─ Interrogation → periodic on-track check
    │
    ├─ Stop hook ──→ pilot on-stop
    │                   └─ Haiku evaluates if Claude should keep going
    │
    ├─ SSE stream ──→ Dashboard / TUI (real-time events)
    │
    └─ Webhooks ──→ Your HTTP endpoints (action, pending_approval, etc.)
```

## Quick start

```bash
git clone https://github.com/NiallBrickell/pilot.git
cd pilot
make start
```

That's it. `pilot start` builds, creates `~/.pilot/` with a default config, installs hooks into `~/.claude/settings.json`, and starts the server. No manual setup needed.

To stop: `make stop` (or `./pilot stop`). This removes hooks and kills the server.

### Requirements

- Go 1.22+
- Node.js 18+ (for the evaluator sidecar)
- Claude Code with auth configured (`claude auth login`)

## How it works

### Hook flow

The `PreToolUse` hook fires for: `Bash`, `Write`, `Edit`, `NotebookEdit`, `WebFetch`, `WebSearch`. Read-only tools (Read, Glob, Grep, etc.) are never intercepted.

When a hook fires, `pilot approve` POSTs to `pilot serve`, which runs the three-layer hierarchy:

1. **Claude Code settings** — reads `~/.claude/settings.json` and `.claude/settings.local.json` walking up from the session's cwd. First match wins. Respects `defaultMode: "acceptEdits"`.
2. **Pilot rules** — fast pattern matching without LLM (extension point).
3. **Haiku evaluation** — calls the evaluator sidecar using Claude Agent SDK with structured JSON output.

### Idle detection

The `Stop` hook fires when Claude stops. `pilot on-stop` reads the transcript, builds a conversation summary, and asks Haiku whether Claude should keep going. If confidence exceeds the threshold, pilot auto-responds with a context-aware nudge.

### Interrogation

On the 1st, 5th, and every 25th tool call after a user message, pilot checks if Claude is still on track. If it's going in circles, doing workarounds instead of fixing root causes, or ignoring instructions, pilot denies the tool call with a redirect message.

## Running standalone

Pilot works completely standalone — the dashboard is optional.

```bash
./pilot start    # install hooks, start server
./pilot stop     # remove hooks, stop server
```

### Commands

| Command | Description |
|---------|-------------|
| `pilot start` | Install hooks, start server, enable pilot |
| `pilot stop` | Remove hooks, stop server, disable pilot |
| `pilot serve` | Start server in foreground (for debugging) |
| `pilot approve` | PreToolUse hook handler (called by Claude Code) |
| `pilot on-stop` | Stop hook handler (called by Claude Code) |
| `pilot status` | Print current state as JSON |
| `pilot wrap` | Wrap a Claude session in a monitored PTY |

## Configuration

All config lives in `~/.pilot/pilot.toml`. Created automatically on first run. Edit without recompiling — config is re-read on each request.

### General settings

| Setting | Default | Description |
|---------|---------|-------------|
| `model` | `"haiku"` | Model for evaluations |
| `confidence_threshold` | `0.8` | Min confidence for auto-responding to idle |
| `idle_timeout_ms` | `3000` | Wait before checking for idle (ms) |
| `pending_response_max_age_s` | `30` | Discard stale pending responses (s) |
| `grace_period_s` | `0` | Delay before auto-approvals take effect (0 = instant) |
| `escalation_timeout_s` | `30` | Wait for human on escalated calls (s) |
| `sse_port` | `9721` | SSE event stream port |
| `evaluator_port` | `9722` | Evaluator sidecar port |
| `max_concurrent_evals` | `4` | Max concurrent Haiku calls |
| `evaluator_timeout_ms` | `15000` | Evaluator call timeout (ms) |
| `interrogation_confidence` | `0.7` | Min confidence for interrogation redirects |

### Prompts

| Setting | Description |
|---------|-------------|
| `[prompts].approval` | System prompt for tool approval. Controls what gets auto-approved vs escalated. |
| `[prompts].auto_respond` | System prompt for idle detection. Controls when and how pilot nudges Claude. |

### Webhooks

Receive pilot events via HTTP POST. Add to `~/.pilot/pilot.toml`:

```toml
[[webhooks]]
url = "http://localhost:8080/pilot/events"
events = ["action", "pending_approval", "approval_resolved"]
secret = "your-hmac-secret"  # optional
```

| Field | Required | Description |
|-------|----------|-------------|
| `url` | Yes | HTTP endpoint to POST events to |
| `events` | No | Event types to send (empty = all). Options: `action`, `pending_approval`, `approval_resolved` |
| `secret` | No | HMAC-SHA256 signing key. If set, requests include `X-Pilot-Signature` header |

**Webhook payload:**

```json
{
  "id": "a1b2c3d4",
  "type": "action",
  "data": "{\"timestamp\":\"...\",\"action_type\":\"auto_approve\",\"detail\":\"Bash: git status\",\"confidence\":1.0,\"tool_name\":\"Bash\",\"cwd\":\"/path/to/project\"}"
}
```

**Event types:**

- `action` — a tool call was approved, escalated, or an idle response was sent/skipped
- `pending_approval` — an escalated call is waiting for human decision (includes countdown)
- `approval_resolved` — a pending approval was approved, rejected, or timed out

### Verifying webhook signatures

```python
import hmac, hashlib

def verify(payload: bytes, signature: str, secret: str) -> bool:
    expected = hmac.new(secret.encode(), payload, hashlib.sha256).hexdigest()
    return hmac.compare_digest(expected, signature)
```

## Dashboard

The optional Wails desktop app provides a GUI for pilot. Located in `dashboard/`.

```bash
# Development mode (hot reload)
make dashboard

# Production build
make dashboard-build
```

### Requirements

- [Wails v2](https://wails.io/docs/gettingstarted/installation)
- Node.js 18+

### Features

- Live action timeline with SSE event stream
- Pending approval cards with countdown timer and approve/reject buttons
- On/off toggle (installs/uninstalls Claude Code hooks)
- Full config editor for all `pilot.toml` settings and prompts
- Dark theme

The dashboard connects to `pilot serve` via SSE — it's purely a UI layer. All decision-making happens in the server.

## Runtime files

All runtime state is stored in `~/.pilot/` (override with `$PILOT_HOME`):

```
~/.pilot/
├── pilot.toml        # configuration (auto-created on first run)
├── state.json        # action history and stats
├── pilot.pid         # wrapper process ID
├── pilot-serve.pid   # server process ID
├── .auth-cache       # cached auth status (1 hour TTL)
└── .env              # API keys (optional)
```

## Environment variables

| Variable | Default | Description |
|----------|---------|-------------|
| `PILOT_HOME` | `~/.pilot` | Base directory for all config and state |
| `PILOT_CONFIG` | `$PILOT_HOME/pilot.toml` | Override config file path |
| `PILOT_STATE_FILE` | `$PILOT_HOME/state.json` | Override state file path |
| `PILOT_EVALUATOR_PATH` | *(next to binary)* | Override path to evaluator.mjs |
| `PILOT_EVALUATOR_PORT` | `9722` | Evaluator sidecar port |

## Integrating with your own app

Pilot exposes two integration points:

### 1. SSE event stream

Connect to `http://localhost:9721/events` for real-time events. This is what the dashboard uses.

```javascript
const es = new EventSource("http://localhost:9721/events");
es.addEventListener("action", (e) => {
  const action = JSON.parse(e.data);
  console.log(action.action_type, action.tool_name, action.detail);
});
es.addEventListener("pending_approval", (e) => {
  const pending = JSON.parse(e.data);
  // Show approve/reject UI, then POST to /approve/{id} or /reject/{id}
});
```

### 2. Webhooks

Configure `[[webhooks]]` in `pilot.toml` to receive HTTP POST callbacks. Better for server-side integrations that can't hold an SSE connection.

### API endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/events` | GET | SSE event stream |
| `/status` | GET | Current pilot state as JSON |
| `/approve/{id}` | POST | Approve a pending escalated call |
| `/reject/{id}` | POST | Reject a pending escalated call |
