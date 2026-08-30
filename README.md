# pilot

AI copilot for Claude Code and Codex sessions — auto-approves safe tool calls, escalates dangerous ones, and nudges agents when they stop unnecessarily. Designed for running 20+ simultaneous sessions without babysitting.

## What it does

1. **Runtime-first approval** — Claude Auto mode and Codex's own permission flow decide routine calls first. Only real permission requests reach Pilot's settings → deterministic danger boundary → Haiku fallback.
2. **Escalation** — dangerous calls are held for human approval via dashboard, webhook, or future TUI. If no response arrives before timeout, Claude prompts normally and Codex PermissionRequest hooks decline to decide.
3. **Idle detection** — when an agent stops unnecessarily, Haiku evaluates the transcript and auto-responds with context-aware nudges like "run the tests" or "keep going".
4. **Interrogation** (opt-in) — periodically checks if the agent is still on track. If it's going in circles or ignoring instructions, pilot redirects it. Off by default; enable with `interrogation_enabled = true`.
5. **Webhooks** — POST events to your own HTTP endpoints for custom integrations, dashboards, or logging.

## How this compares to Claude Code's auto mode

Claude Code's auto mode (now the default) runs its own LLM classifier and takes
natural-language configuration — an `autoMode.environment` description plus
`allow` / `soft_deny` / `hard_deny` **prose** rules — on top of the usual
`permissions.allow` / `ask` / `deny` rules and `PreToolUse` hooks. For a **single
session** that covers most of what Pilot's approval layer does, so if you run one
or two sessions and mostly want fewer prompts, configure auto mode first.

Pilot and auto mode compose rather than purely compete: Pilot's Layer 1 honors
your `permissions.allow` / `ask` / `deny` rules — the same ones auto mode evaluates
before its classifier — so a tightened permission set means fewer Haiku calls.
(The prose `autoMode` block is read by Claude Code's classifier, not by Pilot's
evaluator; keep must-never-run boundaries in `permissions.deny`, which both honor.)

Pilot earns its place on the things auto mode doesn't do at all:

- **Keep-going nudges.** Auto mode decides whether a *tool call* is safe; it does
  nothing when an agent stops half-finished. Pilot's Stop hook evaluates the
  transcript and nudges it onward — the core of running sessions unattended.
- **Out-of-band human escalation.** Auto mode's `ask` prompts in *that* terminal.
  Across 20 sessions you can't watch 20 terminals; Pilot funnels every pending
  approval into one dashboard/webhook queue with a timeout fallback.
- **Codex, not just Claude Code.** Auto mode is Claude Code only. Pilot covers
  Codex sessions with the same policy.
- **A decision layer you own.** Auto mode's classifier prompt is fixed; you only
  feed it context. Pilot's `[prompts].approval` is yours to edit, version, and
  regression-test (`make replay`).
- **Fleet observability.** SSE stream, webhooks, spend cap, and timing profiles
  across every session — auto mode is per-instance.

Pilot is for driving a fleet, not for being a better approver in one window.

## Architecture

```
Claude Code / Codex session (any of 20+)
    │
    ├─ Native auto/permission decision
    │       └─ PermissionRequest only ──→ pilot approve / pilot codex-approve
    │                         │
    │                         POST to pilot serve
    │                         ├─ Layer 1: runtime settings where available (no LLM)
    │                         ├─ Layer 2: routine call / danger-marker boundary (no LLM)
    │                         ├─ Layer 3: Haiku for residual danger or ambiguity
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

Install the latest release binary (no Go toolchain or checkout needed):

```bash
curl -fsSL https://raw.githubusercontent.com/NiallBrickell/pilot/main/install.sh | sh
```

This downloads the right binary for your OS/arch into `~/.pilot/bin`, installs hooks, and starts the server. Set `ANTHROPIC_API_KEY` first (or write it to `~/.pilot/.env`) — the installer tells you how if it's missing. Add `~/.pilot/bin` to your `PATH` to call `pilot` directly.

`pilot start` checks the latest release before installing hooks, so normal starts
stay current automatically. To update immediately (or reinstall explicitly):

```bash
pilot upgrade        # downloads the latest release and restarts (no-op if already current)
```

Or build from source for development:

```bash
git clone https://github.com/NiallBrickell/pilot.git
cd pilot
make start
```

Either way, `pilot start` checks for a newer release, creates `~/.pilot/` with a default config, installs hooks into `~/.claude/settings.json` and `~/.codex/hooks.json`, enables Codex hooks in `~/.codex/config.toml`, and starts the server. No manual setup needed. `make start` deliberately runs the just-built development binary without the release handoff.

To stop: `make stop` (or `./pilot stop`). This removes hooks and kills the server.

### Requirements

- Go 1.22+
- An Anthropic API key (set `ANTHROPIC_API_KEY` in env or `~/.pilot/.env`)
- Claude Code with auth configured (`claude auth login`) and/or Codex CLI

## How it works

### Hook flow

For Claude Code, Pilot installs approval handling on `PermissionRequest`, not `PreToolUse`. Claude's permission rules and Auto-mode classifier therefore decide ordinary calls first. Pilot runs only when Claude Code is about to request an external permission decision. Optional trajectory checks still use `PreToolUse` because they are guards rather than approvals.

For Codex, Pilot installs `PreToolUse` trajectory-check hooks plus `PermissionRequest` approval hooks for `Bash`, `apply_patch`/`Edit`/`Write`, and MCP tools. It also enables Codex's `exec_permission_approvals` and `request_permissions_tool` feature flags so sandbox/network escalation prompts can flow through `PermissionRequest`. Codex `PreToolUse` can only block, so auto-approval happens in `PermissionRequest`. The trajectory-check hook is off by default; set `interrogation_enabled = true` to enable it.

When a permission-request hook fires, `pilot approve` or `pilot codex-approve` POSTs to `pilot serve`, which runs the fallback approval hierarchy:

1. **Runtime settings** — for Claude Code, reads `~/.claude/settings.json` and `.claude/settings.local.json` walking up from the session's cwd. For Codex, reads `~/.codex/config.toml` and treats trusted projects as locally approved for routine Bash/edit/write permission requests while still blocking obvious destructive commands.
2. **Pilot rules** — deterministically approve inspectable routine calls. Calls matching the approval prompt's complete danger/ambiguity markers continue; malformed or opaque structured input never fast-approves.
3. **Haiku evaluation** — judges only that residual set through the Anthropic API with structured JSON output.

### Evaluator spend and degraded mode

The deterministic boundary is the primary cost control: ordinary shell commands and structured tools do not pay for a model to repeat an approval. The same boundary is used if Anthropic is unavailable, so routine calls keep moving while danger markers and uninspectable input ask the user.

Pilot retries only transient transport errors, HTTP 408/409/429, and 5xx responses. Authentication, validation, permission and billing responses are terminal. A `402 billing_error` (plus the legacy low-credit response) opens a process-wide circuit breaker, so subsequent residual calls do not keep spending HTTP requests on an account that cannot serve them.

Residual requests enable Anthropic's ephemeral prompt cache for the fixed system prompt. Cache eligibility is model- and prefix-length-dependent, so Pilot does not assume it took effect: it parses `cache_creation_input_tokens` and `cache_read_input_tokens`, includes their actual write/read rates in local spend accounting, and emits the counters in debug usage logs. Zero counters mean Anthropic skipped caching for that response.

If Codex still shows its own approval prompt for a command that Pilot should handle, first check `./pilot status` or `curl http://localhost:9721/status`. Pilot's Codex hook handlers fail open when `pilot serve` is unreachable, so a normal Codex prompt usually means the server is down, the active Codex session was started before hooks/features were enabled, or no decision was returned before Codex asked you.

### Escalation carries the reason

When a call is denied — say `git merge`, which is on the approval prompt's closed
deny-list — Pilot doesn't just block it. It **escalates to you with the evaluator's
plain-language explanation** of what it matched and why it's your call, so you
approve or reject with the security rationale in front of you rather than a bare
yes/no.

That reason travels everywhere the escalation surfaces:

- the **dashboard** pending-approval card, alongside the approve/reject buttons and countdown;
- the **`action`** and **`pending_approval`** events on the SSE stream and your webhooks (the `detail` / `reason` field);
- the hook response returned to the session.

The approval prompt requires the evaluator to name the deny-list entry it matched
before denying (see [Changing the approval prompt](#changing-the-approval-prompt)),
so a `git merge` escalation reads like *"git merge is on the deny list — merging is
a human decision,"* not just "denied." The rationale reaches you at the moment you
decide.

### Idle detection

The `Stop` hook fires when the agent stops. `pilot on-stop` or `pilot codex-on-stop` reads the transcript, builds a structured conversation summary, and asks Haiku whether the agent should keep going.

If confidence exceeds the threshold, pilot returns `{"decision": "block", "reason": "keep going — run the tests"}`. Claude Code and Codex both treat this Stop-hook block as a continuation prompt.

### Interrogation

When enabled, on the 1st, 5th, and every 25th tool call after a user message, pilot checks if the agent is still on track. If it's going in circles, doing workarounds instead of fixing root causes, or ignoring instructions, pilot denies the tool call with a redirect message.

These PreToolUse trajectory checks are **off by default** — they add an interruption per matched call and a second hook process per tool. Set `interrogation_enabled = true` in `~/.pilot/pilot.toml` to turn them on without affecting approval handling.

## Running standalone

Pilot works completely standalone — the dashboard is optional.

```bash
./pilot start    # install hooks, start server
./pilot stop     # remove hooks, stop server
```

### Commands

| Command | Description |
|---------|-------------|
| `pilot start` | Update if needed, install hooks, start server, enable pilot |
| `pilot stop` | Remove hooks, stop server, disable pilot |
| `pilot upgrade` | Download the latest release binary from GitHub and restart |
| `pilot dashboard` | Download (if needed) and launch the desktop GUI |
| `pilot serve` | Start server in foreground (for debugging) |
| `pilot approve` | Claude Code PermissionRequest hook handler |
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
| `input_cost_per_mtok_usd` | `1.0` | Base input token price used for local spend estimates; reported cache writes/reads are priced at 1.25x/0.1x this value |
| `output_cost_per_mtok_usd` | `5.0` | Output token price used for local spend estimates |
| `interrogation_confidence` | `0.7` | Min confidence for interrogation redirects |
| `interrogation_enabled` | `false` | Allow PreToolUse trajectory checks to redirect stuck/off-track agents |

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

This downloads the prebuilt app from GitHub releases to `~/.pilot/` and launches it. On macOS it opens as a native `.app`. The dashboard delegates enable/disable to the CLI rather than carrying a second copy of Claude/Codex hook policy; on startup it repairs hook/config state left by older dashboard builds.

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

### Changing the approval prompt

The prompt in `internal/config/pilot.toml` defines the residual judgment after
Pilot's deterministic danger boundary. A regression in either layer can silently
interrupt work or bypass a manual gate, so two rules keep them aligned.

**Record the hash.** Append the new prompt hash to `internal/config/prompt_history.txt`
whenever you change `[prompts]`. `go test ./internal/config` fails until you do, and
prints the line to add. Installs whose prompts match a recorded hash are treated as
an old default rather than a user edit, so they keep receiving upgrades — without
this, one config write that skips the baseline pins that machine to its current
prompt forever.

**Replay before shipping.** `make replay` runs ~40 real escalated tool calls
(pulled from live `pilot.db` history) through the evaluator and reports every
verdict that lands the wrong way:

```
47/47 correct (33 settled by pilot rules, 14 by the evaluator)
```

The deterministic corpus test runs in CI and requires every routine case to settle
locally while preserving every deliberate manual gate. The live evaluator replay
costs a few cents, so `make release` runs it instead. Add a case to
`replay_corpus_test.go` whenever you fix a false positive or expand the danger
boundary, taking the command verbatim from the escalation that exposed it.

Haiku reads the deny list very literally. "`rm -rf` where the path starts with `/`"
was read as covering plain `rm a.md b.md`, and "a destination the user has no
relationship with" as covering a bare `IP:port` database host. Reasoning about the
wording is not a substitute for replaying it.

### Releasing

```bash
make release VERSION=v0.1.29
```

That runs the unit tests, replays the approval corpus, then tags and pushes.
Equivalent by hand:

```bash
git tag v0.1.16
git push origin v0.1.16
```

The Release workflow runs on `v*` tags and uploads:
- **CLI binaries** (`pilot-darwin-arm64`, `pilot-darwin-amd64`, `pilot-linux-amd64`, `pilot-linux-arm64`) — version-stamped via `-ldflags`, consumed by `install.sh` and `pilot upgrade`.
- **Dashboard assets** that `pilot dashboard` downloads from GitHub Releases.

`install.sh` always pulls `releases/latest/download/<asset>`, so the newest tag is what new installs and `pilot upgrade` receive.

## Security

Pilot inspects and records real tool calls, so a command may carry a secret
(an inline database password, an API key, a bearer token). Two layers keep those
out of durable storage and out of git:

- **Capture-time redaction.** Every write into `~/.pilot/pilot.db` — recorded
  actions and logs — passes through `internal/redact` first, which replaces
  secret material with `***REDACTED***` while preserving the surrounding command
  so it stays readable. This runs before anything touches disk.
- **Repo secret scanning.** [gitleaks](https://github.com/gitleaks/gitleaks)
  runs in CI (`.github/workflows/secret-scan.yml`) on every push and pull
  request, using `.gitleaks.toml`. A committed-secret fence test
  (`internal/config/secret_fence_test.go`) also fails `go test` if a real-looking
  secret is present in the source tree.

To scan your own commits locally, enable the pre-commit hook once:

```bash
git config core.hooksPath .githooks
```

The hook runs gitleaks on staged changes if it is installed, and is a no-op
otherwise.

## Runtime files

All runtime state is stored in `~/.pilot/` (override with `$PILOT_HOME`):

```
~/.pilot/
├── bin/pilot         # installed binary (install.sh / pilot upgrade)
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
| `PILOT_SERVER_TOKEN` | *(none)* | Optional bearer token required by every POST route and `/internal/auth-check` |

### Local-only control plane

`pilot serve` binds its HTTP server to IPv4 loopback (`127.0.0.1`) only. The
server exposes approval, hook-management, and configuration routes, so it is an
authority boundary rather than a service to publish on a host, VPC, or public
network. The loopback listener also keeps ordinary bridge-networked containers
from calling the host's privileged Pilot routes.

Do not put the server behind a public reverse proxy or run an untrusted worker
with host networking. For remote operator access, use an authenticated SSH
port-forward to `127.0.0.1:9721`; outbound webhooks remain the intended
server-to-server integration.

For a production process that shares the host with other workloads, set a
high-entropy `PILOT_SERVER_TOKEN`. When it is non-empty, every POST request must
carry the exact header `Authorization: Bearer <token>`. Pilot compares bearer
credentials in constant time and rejects a missing or invalid credential before
the request body or route handler is reached. The read-only `/status`, `/events`,
`/logs`, `/config`, and `/internal/profile` routes remain local and public.

Deployment health checks can verify both listener and credential wiring with:

```bash
curl -fsS -o /dev/null -w '%{http_code}\n' \
  -H "Authorization: Bearer $PILOT_SERVER_TOKEN" \
  http://127.0.0.1:9721/internal/auth-check
```

The authenticated response is `204`; a missing, malformed, or incorrect bearer
credential receives `401`. With no `PILOT_SERVER_TOKEN`, Pilot keeps its existing
local desktop behavior and POST requests need no authorization header.

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
| `/health` | GET | Lightweight process identity used by startup checks |
| `/events` | GET | SSE event stream |
| `/status` | GET | Current pilot state + hooks status as JSON |
| `/internal/auth-check` | GET | Authenticated deployment challenge (`204` on success) |
| `/approve/{id}` | POST | Approve a pending escalated call |
| `/reject/{id}` | POST | Reject a pending escalated call |
| `/hooks/install` | POST | Install pilot hooks into Claude Code and Codex config |
| `/hooks/uninstall` | POST | Remove pilot hooks from Claude Code and Codex config |
| `/config` | GET | Current pilot configuration as JSON |
| `/logs` | GET | Recent pilot logs |
