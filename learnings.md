# Pilot Learnings

## Evaluation approach: direct Anthropic API

We went through four iterations:

| Approach | Latency | Issues |
|----------|---------|--------|
| `claude -p` per call | ~4-6s | Process spawn per tool call. 20 sessions = hundreds of processes = kernel panic |
| `claude -p` via server with semaphore | ~4-6s | Still spawning processes, just rate-limited. Hangs if `claude -p` hangs (no timeout) |
| Agent SDK sidecar (Node.js) | ~2-3s | Long-running Node process, structured output, no process spawning. Extra runtime dependency. |
| Direct Anthropic API (current) | ~1.5-2s | Go HTTP calls to api.anthropic.com. No Node.js dependency. Structured output via `output_config`. |

### Why direct API wins

- **No extra runtime** — pure Go, no Node.js dependency. One binary.
- **Structured output** — `output_config: { format: { type: "json_schema", schema: {...} } }` gives validated JSON directly.
- **Concurrency** — separate semaphores in Go (4 approval + 2 idle). Approvals can never be blocked by idle evaluations.
- **Simpler deployment** — no sidecar process to supervise, no port management, no crash recovery loop.

## Three-layer approval hierarchy

PreToolUse hooks fire for ALL tool calls, including ones the user has already auto-approved. We went through several iterations:

| Approach | Issue |
|----------|-------|
| Flat permissions.go reimplementing Claude's rules | Missed `defaultMode: "acceptEdits"`, didn't walk settings hierarchy correctly |
| Merged all settings files then evaluated | Local deny could be overridden by parent allow |
| Walk-up with first-match-wins (current) | Correct. Each settings file evaluated independently in order |

### Claude Code settings hierarchy

Settings files from most local to most global. Each file checked independently — first match wins:

1. `.claude/settings.local.json` in cwd
2. `.claude/settings.local.json` walking up to root
3. `~/.claude/settings.json` (global)

Within each file: deny > ask > allow. `defaultMode: "acceptEdits"` auto-approves Write/Edit/NotebookEdit.

### Hook matchers

Each hook invocation = one OS process spawn + one HTTP roundtrip to serve. Two PreToolUse hooks (approve + interrogate) means 2 processes per matched tool call.

- **approve**: `"^(Bash|Write|Edit|NotebookEdit|WebFetch|WebSearch|Read|Grep|Glob|Agent)$"` — Read/Grep/Glob must be included because pilot auto-approves them (Layer 2), saving the user from Claude's default permission prompts.
- **interrogate**: `".*"` — fires on every tool call. Server-side cadence logic (1st, 5th, every 25th per user turn) short-circuits most calls immediately. Keeping the broad matcher preserves full visibility into off-track behaviour.

## The MBP incident

Running `wails dev` from a background shell process (`&`) caused the GUI app to spin trying to attach to a display context. Combined with pilot hooks firing on every tool call (spawning `claude -p` per call across 20 sessions), this pegged all 24 cores and nearly kernel panicked an M4 MBP.

Lessons:
- Never launch GUI apps from background shell processes
- Limit concurrent LLM evaluations with a semaphore
- Use a narrower hook matcher to reduce process spawns
- `pilot-disable` should `pkill` lingering processes, not just remove hooks

## Approval prompt tuning

The prompt went through iterations:

1. **Conservative** — explicit safe/deny lists. Over-escalated normal dev work (db creds in commands, .env file edits).
2. **Relaxed** — approve by default, only deny genuinely dangerous operations (data loss, exfil, security). No babysitting. This matches how experienced devs actually work.

Key insight: the prompt should describe *categories of danger*, not explicit command lists. "Destructive database operations" is better than "DROP, TRUNCATE, DELETE" because it lets the model reason about novel cases.

## Stop hook auto-respond: block, don't inject

Original approach: write `pending_response` to state.json, have a PTY wrapper poll and inject it into stdin. Required a dedicated terminal window per session. Completely impractical for 20+ sessions.

Fix: Claude Code's Stop hook supports `{"decision": "block", "reason": "..."}`. Claude sees the reason and continues. No PTY wrapper, no stdin injection, no terminal management. The hook itself is the injection mechanism.

## Separate semaphores for approvals vs idle

With 20 sessions, stop hooks fire frequently. If idle evaluations (2-15s each) saturate a shared semaphore, approval evaluations queue behind them. Result: `pilot approve` times out, Claude shows "hook error", and the tool call goes through unevaluated.

Fix: separate semaphores. 3 slots for approvals, 2 for idle. Approvals can never be blocked by idle evaluations. Idle can be slower without impacting the approval path.

## Tool input is JSON, not plain text

Claude Code sends tool input as JSON: `{"command":"git status","description":"..."}` for Bash, `{"file_path":"/foo.go",...}` for Edit/Write. The permission matching code was comparing `Bash({"command":"git status",...})` against patterns like `Bash(git status:*)` — never matching.

Fix: extract the relevant field from JSON input before building the signature. `command` for Bash, `file_path` for Edit/Write, `domain` from URL for WebFetch. Tests added.

## Interrogation: recent messages define the task

In long sessions, the user's task evolves. The interrogation system initially weighted the original request too heavily, flagging legitimate work as "off track" because it didn't match the first message.

Fix: the prompt now says "MOST RECENT messages are the current task". Only flag genuinely off-track behaviour, not task evolution.

## Webhook integration pattern

External apps (like acme-development dashboard) integrate via webhooks, not direct code imports. Pilot POSTs events to configured HTTP endpoints. The external app receives them and feeds to its own UI.

This is cleaner than the original approach of embedding pilot code in the dashboard. The dashboard just needs a webhook receiver endpoint and a way to approve/reject via `POST /approve/{id}` and `POST /reject/{id}` on pilot serve.
