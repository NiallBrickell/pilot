# Pilot Learnings

## Evaluation approach: Agent SDK sidecar

We went through three iterations:

| Approach | Latency | Issues |
|----------|---------|--------|
| `claude -p` per call | ~4-6s | Process spawn per tool call. 20 sessions = hundreds of processes = kernel panic |
| `claude -p` via server with semaphore | ~4-6s | Still spawning processes, just rate-limited. Hangs if `claude -p` hangs (no timeout) |
| Agent SDK sidecar (current) | ~2-3s | Long-running Node process, structured output, no process spawning |

### Why the Agent SDK wins

- **No process spawning** — the evaluator is a long-running HTTP server. Each evaluation is an HTTP call, not a fork+exec.
- **Structured output** — `outputFormat: { type: "json_schema", schema: {...} }` gives us `msg.structured_output` with validated JSON. No parsing markdown code fences.
- **Auth** — uses `CLAUDE_CODE_OAUTH_TOKEN` from `.env` (from `claude setup-token`). Setup token is scoped for Claude Code usage.
- **Concurrency** — built-in semaphore (max 4 concurrent). Queue the rest instead of spawning unbounded processes.

### Agent SDK structured output gotcha

`maxTurns: 1` is not enough — the schema enforcement uses an extra turn internally. Use `maxTurns: 3` to be safe. The structured output comes back on `msg.structured_output`, NOT `msg.result` (which returns the raw text with markdown fences).

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

### Hook matcher matters

`"matcher": ".*"` fires on EVERY tool call including Read, Glob, Grep — spawning a process each time. Changed to `"^(Bash|Write|Edit|NotebookEdit|WebFetch|WebSearch)$"` to only fire on tools that might actually need approval.

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
