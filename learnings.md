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

## Runtime-first approval hierarchy

Claude `PreToolUse` hooks fire before permission rules and the Auto-mode classifier. Putting Pilot approval there caused every tool call to reach Pilot first: 43,909 direct Haiku approval calls and $125.28 in one month. Approval must run on `PermissionRequest`, after the runtime has decided it needs external help. `PreToolUse` is only for opt-in trajectory guards.

Within Pilot's residual permission-request path, we went through several settings-matching iterations:

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

### A closed deny list needs a deterministic safe default

Routing every Layer-2 miss to Haiku inverted the approval policy: a production
ledger showed 69% of calls paying for evaluation even though 99% of evaluator
outcomes were approvals. Routine, inspectable calls now approve locally; only a
complete marker for the prompt's destructive/exfiltration boundary, or input that
cannot be inspected, reaches Haiku.

Normal and degraded operation must share that exact boundary. A blanket
"non-Bash tools approve" outage fallback let structured calls such as dataset
deletion, HTTP DELETE and capture-endpoint uploads bypass markers that normal mode
correctly caught. Both paths now parse the structured payload and ask on malformed
or opaque input.

Billing failures are state, not transient noise. Anthropic's typed status/error
contract determines retries (transport errors, 408/409/429 and 5xx only), and a
`billing_error` or legacy low-balance response opens a process circuit breaker.
This prevents one exhausted balance from producing another HTTP request and retry
for every tool invocation.

Prompt caching is an optimization, never an assumption. Requests opt into the
current top-level ephemeral cache, but models may skip short prefixes. Treat the
response's cache-creation and cache-read token counters as the authority, expose
them in debug telemetry, and price reported writes/reads separately in spend
accounting.

### Hook matchers

Each hook invocation = one OS process spawn + one HTTP roundtrip to serve.

- **approve**: `PermissionRequest` with `".*"` — Claude Auto mode and native permission rules resolve routine calls before this hook runs.
- **interrogate**: `".*"` — fires on every tool call. Server-side cadence logic (1st, 5th, every 25th per user turn) short-circuits most calls immediately. Keeping the broad matcher preserves full visibility into off-track behaviour.

The dashboard must not maintain its own hook installer. A CLI migration moved
Claude approval from `PreToolUse` to `PermissionRequest`, but the duplicated
dashboard implementation restored the old hook as soon as the updated dashboard
started. Dashboard lifecycle actions now upgrade and delegate to the canonical
CLI installer; startup treats stale Claude or Codex hook/config state as a
migration to repair.

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

External apps (like a separate dashboard) integrate via webhooks, not direct code imports. Pilot POSTs events to configured HTTP endpoints. The external app receives them and feeds to its own UI.

This is cleaner than the original approach of embedding pilot code in the dashboard. The dashboard just needs a webhook receiver endpoint and a way to approve/reject via `POST /approve/{id}` and `POST /reject/{id}` on pilot serve.

## Codex hook model

Codex hooks are close to Claude Code hooks, but the approval semantics differ. `PreToolUse` is only useful as an early block/redirect guardrail; it cannot auto-approve or ask the user. Real auto-approval belongs in `PermissionRequest`, where Pilot can return an allow/deny decision. On dashboard timeout, Codex `PermissionRequest` should decline to decide so Codex falls back to its normal approval flow; `PreToolUse` should fail open unless a human explicitly rejects.

Codex transcripts are also a different JSONL envelope (`payload.type`, `payload.message`, `response_item` message content), so transcript parsing needs runtime-neutral extraction rather than assuming Claude's `message.role/content` shape.
