# pilot

AI copilot for Claude Code and Codex sessions — auto-approves safe tool calls, escalates dangerous ones, and nudges agents when they stop unnecessarily. Designed for running 20+ simultaneous sessions without babysitting.

## What it does

1. **Three-layer approval** — tool calls go through runtime settings where available → pilot rules → Haiku evaluation. Most calls resolve without an LLM call.
2. **Escalation** — dangerous calls are held for human approval via dashboard, webhook, or future TUI. If no response arrives before timeout, Claude prompts normally and Codex PermissionRequest hooks decline to decide.
3. **Idle detection** — when an agent stops unnecessarily, Haiku evaluates the transcript and auto-responds with context-aware nudges like "run the tests" or "keep going".
4. **Interrogation** — periodically checks if the agent is still on track. If it's going in circles or ignoring instructions, pilot redirects it.
5. **Webhooks** — POST events to your own HTTP endpoints for custom integrations, dashboards, or logging.

## Architecture

```
Claude Code / Codex session (any of 20+)
    │
    ├─ PreToolUse / PermissionRequest hook ──→ pilot approve / pilot codex-approve
    │                         │
    │                         POST to pilot serve
    │                         ├─ Layer 1: runtime settings where available (no LLM)
    │                         ├─ Layer 2: Pilot rules (no LLM)
    │                         ├─ Layer 3: Haiku via Anthropic API
    │                         │
    │                         ├─ Approved → "allow"
    │                         ├─ Escalated → wait for human (timeout configurable)
    │                         └─ Interrogation → periodic on-track check
    │
    ├─ Stop hook ──→ pilot on-stop / pilot codex-on-stop
    │                   └─ Haiku evaluates if the agent should keep going
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

That's it. `pilot start` builds, creates `~/.pilot/` with a default config, installs hooks into `~/.claude/settings.json` and `~/.codex/hooks.json`, enables Codex hooks in `~/.codex/config.toml`, and starts the server. No manual setup needed.

To stop: `make stop` (or `./pilot stop`). This removes hooks and kills the server.

### Requirements

- Go 1.22+
- An Anthropic API key (set `ANTHROPIC_API_KEY` in env or `~/.pilot/.env`)
- Claude Code with auth configured (`claude auth login`) and/or Codex CLI

## How it works

### Hook flow

For Claude Code, the `PreToolUse` hook fires for: `Bash`, `Write`, `Edit`, `NotebookEdit`, `WebFetch`, `WebSearch`, `Read`, `Grep`, `Glob`, and `Agent`.

For Codex, Pilot installs `PreToolUse` trajectory-check hooks plus `PermissionRequest` approval hooks for `Bash`, `apply_patch`/`Edit`/`Write`, and MCP tools. It also enables Codex's `exec_permission_approvals` and `request_permissions_tool` feature flags so sandbox/network escalation prompts can flow through `PermissionRequest`. Codex `PreToolUse` can only block, so auto-approval happens in `PermissionRequest`. Set `interrogation_enabled = false` to disable the trajectory-check hook.

When a hook fires, `pilot approve` or `pilot codex-approve` POSTs to `pilot serve`, which runs the approval hierarchy:

1. **Runtime settings** — for Claude Code, reads `~/.claude/settings.json` and `.claude/settings.local.json` walking up from the session's cwd. For Codex, reads `~/.codex/config.toml` and treats trusted projects as locally approved for routine Bash/edit/write permission requests while still blocking obvious destructive commands.
2. **Pilot rules** — fast pattern matching without LLM (extension point).
3. **Haiku evaluation** — calls the Anthropic API directly with structured JSON output.

If Codex still shows its own approval prompt for a command that Pilot should handle, first check `./pilot status` or `curl http://localhost:9721/status`. Pilot's Codex hook handlers fail open when `pilot serve` is unreachable, so a normal Codex prompt usually means the server is down, the active Codex session was started before hooks/features were enabled, or no decision was returned before Codex asked you.

### Idle detection

The `Stop` hook fires when the agent stops. `pilot on-stop` or `pilot codex-on-stop` reads the transcript, builds a structured conversation summary, and asks Haiku whether the agent should keep going.

If confidence exceeds the threshold, pilot returns `{"decision": "block", "reason": "keep going — run the tests"}`. Claude Code and Codex both treat this Stop-hook block as a continuation prompt.

### Interrogation

On the 1st, 5th, and every 25th tool call after a user message, pilot checks if the agent is still on track. If it's going in circles, doing workarounds instead of fixing root causes, or ignoring instructions, pilot denies the tool call with a redirect message.

Set `interrogation_enabled = false` in `~/.pilot/pilot.toml` to disable these PreToolUse trajectory checks without disabling approval handling.

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
| `pilot dashboard` | Download (if needed) and launch the desktop GUI |
| `pilot serve` | Start server in foreground (for debugging) |
| `pilot approve` | Claude Code PreToolUse hook handler |
| `pilot codex-approve` | Codex PermissionRequest hook handler |
| `pilot on-stop` | Claude Code Stop hook handler |
| `pilot codex-on-stop` | Codex Stop hook handler |
| `pilot codex-interrogate` | Codex PreToolUse interrogation hook handler |
| `pilot status` | Print current state as JSON |
| `pilot profile` | Show evaluation timing stats (avg, p50, p95, p99 by source) |
| `pilot wrap` | Wrap a Claude session in a monitored PTY |

## Configuration

All config lives in `~/.pilot/pilot.toml`. Created automatically on first run. Edit without recompiling — config is re-read on each request.

### General settings

| Setting | Default | Description |
|---------|---------|-------------|
| `model` | `"claude-haiku-4-5"` | Model for evaluations |
| `confidence_threshold` | `0.8` | Min confidence for auto-responding to idle |
| `idle_timeout_ms` | `3000` | Wait before checking for idle (ms) |
| `pending_response_max_age_s` | `30` | Discard stale pending responses (s) |
| `grace_period_s` | `0` | Delay before auto-approvals take effect (0 = instant) |
| `escalation_timeout_s` | `30` | Wait for human on escalated calls (s) |
| `stop_hook_replies` | `true` | Allow Stop hooks to nudge agents to continue (all runtimes) |
| `sse_port` | `9721` | SSE event stream port |
| `max_concurrent_evals` | `4+2` | Max concurrent API calls (4 approval + 2 idle, separate semaphores) |
| `evaluator_timeout_ms` | `15000` | Evaluator call timeout (ms) |
| `monthly_spend_cap_usd` | `20.0` | Monthly Anthropic evaluator spend cap. `0` disables it. |
| `input_cost_per_mtok_usd` | `1.0` | Input token price used for local spend estimates |
| `output_cost_per_mtok_usd` | `5.0` | Output token price used for local spend estimates |
| `interrogation_confidence` | `0.7` | Min confidence for interrogation redirects |
| `interrogation_enabled` | `true` | Allow PreToolUse trajectory checks to redirect stuck/off-track agents |

### Prompts

| Setting | Description |
|---------|-------------|
| `[prompts].approval` | System prompt for tool approval. Controls what gets auto-approved vs escalated. |
| `[prompts].auto_respond` | System prompt for idle detection. Controls when and how pilot nudges the agent. |

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

Optional desktop GUI for pilot. Downloads automatically on first launch — no build tools needed.

```bash
./pilot dashboard
```

This downloads the prebuilt app from GitHub releases to `~/.pilot/` and launches it. On macOS it opens as a native `.app`.

Pushes to `main` run validation CI only. They do not publish new dashboard binaries. To make dashboard changes available through `./pilot dashboard` or `make dashboard`, create and push a `v*` tag; the Release workflow builds the CLI/dashboard artifacts and publishes them to GitHub Releases.

### Features

- Live action timeline with SSE event stream
- Pending approval cards with countdown timer and approve/reject buttons
- On/off toggle (installs/uninstalls Claude Code and Codex hooks)
- Full config editor for all `pilot.toml` settings and prompts
- Dark theme

The dashboard connects to `pilot serve` via SSE — it's purely a UI layer. All decision-making happens in the server.

### Developing the dashboard

If you want to hack on the dashboard itself, you'll need [Wails v2](https://wails.io/docs/gettingstarted/installation):

```bash
make dashboard-dev      # dev mode with hot reload
make dashboard-build    # production build
```

### Releasing

Release tags drive installable artifacts:

```bash
git tag v0.1.7
git push origin v0.1.7
```

The Release workflow runs on `v*` tags and uploads the prebuilt dashboard assets that `pilot dashboard` downloads from GitHub Releases.

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
| `ANTHROPIC_API_KEY` | *(none)* | Anthropic API key (also checked in `~/.pilot/.env`) |

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
| `/status` | GET | Current pilot state + hooks status as JSON |
| `/approve/{id}` | POST | Approve a pending escalated call |
| `/reject/{id}` | POST | Reject a pending escalated call |
| `/hooks/install` | POST | Install pilot hooks into Claude Code and Codex config |
| `/hooks/uninstall` | POST | Remove pilot hooks from Claude Code and Codex config |
| `/config` | GET | Current pilot configuration as JSON |
| `/logs` | GET | Recent pilot logs |
