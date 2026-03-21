# pilot

AI copilot for Claude Code sessions — auto-approves safe tool calls, escalates dangerous ones, and nudges Claude when it stops unnecessarily. Designed for running 20+ simultaneous sessions.

## What it does

1. **Three-layer approval hierarchy** — tool calls go through Claude Code settings → pilot rules → Haiku evaluation. Most calls are resolved without an LLM call.
2. **Escalation with timeout** — dangerous calls are held for human approval (via dashboard or future TUI). If no response within the timeout, Claude prompts normally.
3. **Idle detection** — when Claude stops unnecessarily, Haiku evaluates the transcript and auto-responds with context-aware nudges.
4. **Claude Agent SDK** — evaluations run via a long-running Node.js sidecar using `@anthropic-ai/claude-agent-sdk` with structured JSON output. No process spawning per evaluation.

## Architecture

```
Claude Code session (any of 20+)
    │
    ├─ PreToolUse hook ──→ pilot approve (thin Go client)
    │                         ├─ POST to pilot serve → three-layer evaluation:
    │                         │   1. Claude Code settings (walk cwd upward, first match wins)
    │                         │   2. Pilot rules (fast pattern matching, no LLM)
    │                         │   3. Haiku via Agent SDK sidecar (structured JSON output)
    │                         ├─ If approved → return "allow"
    │                         └─ If escalated → wait for human decision (configurable timeout)
    │
    └─ Stop hook ──→ pilot on-stop
                        ├─ reads transcript + last_assistant_message
                        └─ POST to serve → Haiku evaluates if Claude should keep going
```

## Install

```bash
# Build and install
make install

# Configure Claude Code hooks
pilot install
```

Or step by step:

```bash
go build -o pilot .
cp pilot /usr/local/bin/pilot
cp evaluator.mjs /usr/local/bin/evaluator.mjs
npm install --production
mkdir -p ~/.pilot
cp pilot.example.toml ~/.pilot/pilot.toml
pilot install  # prints hook config to add to ~/.claude/settings.json
```

### Requirements

- Go 1.22+
- Node.js 18+ (for the evaluator sidecar)
- Claude Code auth configured (`claude auth login`)
- `ANTHROPIC_API_KEY` or `CLAUDE_CODE_OAUTH_TOKEN` in `~/.pilot/.env`

## How the hooks work

The PreToolUse hook only fires for tools that Claude would normally prompt about: `Bash`, `Write`, `Edit`, `NotebookEdit`, `WebFetch`, `WebSearch`. Read-only tools (Read, Glob, Grep, etc.) are never intercepted.

When a hook fires, `pilot approve` is a thin Go binary that POSTs to `pilot serve`. The server runs the three-layer hierarchy:

1. **Claude Code settings** — reads `~/.claude/settings.json` and `.claude/settings.local.json` files walking up from the session's cwd. Each file is evaluated independently; first match wins. A local deny can't be overridden by a parent allow. Respects `defaultMode: "acceptEdits"`.
2. **Pilot rules** — fast pattern matching (extension point, currently empty).
3. **Haiku evaluation** — via the Node.js evaluator sidecar using Claude Agent SDK with `json_schema` structured output. Semaphore-limited to 4 concurrent calls.

## Running

```bash
# Start the server (required for approvals and idle detection)
pilot serve

# Or run in dev mode
make dev
```

The server starts on port 9721 (SSE events) and spawns the evaluator sidecar on port 9722.

## Configuration

All config lives in `~/.pilot/pilot.toml` — edit without recompiling.

| Setting | Default | Description |
|---------|---------|-------------|
| `model` | `haiku` | Model for evaluations |
| `confidence_threshold` | `0.8` | Min confidence for auto-responding to idle |
| `grace_period_s` | `0` | Delay before auto-approvals take effect (0 = instant) |
| `escalation_timeout_s` | `30` | How long to wait for human on escalated calls |
| `sse_port` | `9721` | SSE event stream port |
| `[prompts].approval` | *(see file)* | System prompt for tool approval evaluation |
| `[prompts].auto_respond` | *(see file)* | System prompt for idle detection |

## Runtime files

All runtime state is stored in `~/.pilot/` (override with `$PILOT_HOME`):

```
~/.pilot/
├── pilot.toml        # configuration
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
