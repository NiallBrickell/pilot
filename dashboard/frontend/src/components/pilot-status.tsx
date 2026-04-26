import { useEffect, useState, useMemo } from "react";
import { useNavigate } from "react-router-dom";
import type { PilotStatus, PilotAction, PilotPendingApproval } from "../lib/types";
import {
  getPilotStatus,
  installPilotHooks,
  uninstallPilotHooks,
} from "../lib/api";
import {
  usePilotSSE,
  approvePending,
  rejectPending,
} from "../lib/pilot-events";

function timeAgo(dateStr: string): string {
  if (!dateStr) return "";
  const now = new Date();
  const then = new Date(dateStr);
  const diffMs = now.getTime() - then.getTime();
  const diffMins = Math.floor(diffMs / 60000);
  if (diffMins < 1) return "just now";
  if (diffMins < 60) return `${diffMins}m ago`;
  const diffHours = Math.floor(diffMins / 60);
  if (diffHours < 24) return `${diffHours}h ago`;
  return `${Math.floor(diffHours / 24)}d ago`;
}

type ActionFilter = "all" | "passthrough" | "auto_approve" | "escalate" | "auto_respond" | "auto_respond_skipped";

const FILTER_LABELS: Record<ActionFilter, string> = {
  all: "All",
  passthrough: "Settings",
  auto_approve: "Approved",
  escalate: "Escalated",
  auto_respond: "Nudged",
  auto_respond_skipped: "Idle",
};

function cwdLabel(cwd?: string): string | null {
  if (!cwd) return null;
  const parts = cwd.split("/").filter(Boolean);
  return parts[parts.length - 1] || null;
}

function ActionIcon({ type }: { type: string }) {
  switch (type) {
    case "auto_approve":
      return <span className="text-success">&#x2713;</span>;
    case "passthrough":
      return <span className="text-success/60">&#x2713;</span>;
    case "escalate":
      return <span className="text-warning">&#x26A0;</span>;
    case "auto_respond":
      return <span className="text-info">&#x27A4;</span>;
    case "auto_respond_skipped":
      return <span className="text-muted-foreground">&#x2015;</span>;
    default:
      return <span>?</span>;
  }
}

function ApprovalSource({ action }: { action: PilotAction }) {
  if (action.action_type === "passthrough") {
    return <span className="text-[9px] px-1 py-0.5 rounded bg-muted text-muted-foreground flex-shrink-0">settings</span>;
  }
  if (action.action_type === "auto_approve" || action.action_type === "escalate") {
    return <span className="text-[9px] px-1 py-0.5 rounded bg-primary/10 text-primary flex-shrink-0">haiku</span>;
  }
  return null;
}

export function PilotStatusWidget() {
  const [status, setStatus] = useState<PilotStatus | null>(null);
  const [filter, setFilter] = useState<ActionFilter>("all");
  const [expandedAction, setExpandedAction] = useState<number | null>(null);
  const [loading, setLoading] = useState<string | null>(null);
  const [toggleError, setToggleError] = useState<string | null>(null);
  const [showWelcome, setShowWelcome] = useState(false);
  const navigate = useNavigate();

  useEffect(() => {
    const fetch = async () => {
      try {
        const s = await getPilotStatus();
        setStatus(s);
      } catch {
        // pilot not available
      }
    };
    fetch();
    const interval = setInterval(fetch, 5000);
    return () => clearInterval(interval);
  }, []);

  const sseEnabled = status?.sse_available ?? false;
  const ssePort = status?.sse_port ?? 9721;
  const { actions: sseActions, pendingApprovals } = usePilotSSE(
    ssePort,
    sseEnabled
  );

  // Merge: start with persisted actions from server (SQLite), then layer
  // SSE events on top for real-time updates. Deduplicate by timestamp.
  const allActions: PilotAction[] = useMemo(() => {
    const serverActions = status?.recent_actions ?? [];
    if (sseActions.length === 0) return serverActions;

    const seen = new Set(serverActions.map((a) => a.timestamp));
    const merged = [...serverActions];
    for (const a of sseActions) {
      if (!seen.has(a.timestamp)) {
        merged.push(a);
        seen.add(a.timestamp);
      }
    }
    // Sort newest first
    merged.sort((a, b) => b.timestamp.localeCompare(a.timestamp));
    return merged.slice(0, 200);
  }, [sseActions, status?.recent_actions]);

  const filteredActions = useMemo(() => {
    if (filter === "all") return allActions;
    return allActions.filter((a) => a.action_type === filter);
  }, [allActions, filter]);

  const pilotOn = !!status && status.available && status.hooks_installed;

  // First-run welcome: if pilot is off and the user has never enabled it,
  // show a modal pointing at the toggle.
  useEffect(() => {
    if (!status) return;
    if (pilotOn) return;
    let welcomed = false;
    try { welcomed = localStorage.getItem("pilot.welcomed") === "1"; } catch {}
    if (!welcomed) setShowWelcome(true);
  }, [status, pilotOn]);

  if (!status) {
    return (
      <div className="p-6 text-muted-foreground text-sm">
        Connecting to pilot...
      </div>
    );
  }

  const handleTogglePilot = async () => {
    const wasOn = pilotOn;
    setLoading("pilot");
    setToggleError(null);
    try {
      if (wasOn) {
        await uninstallPilotHooks();
      } else {
        await installPilotHooks();
      }
      // Poll until state actually changes (server takes a moment to start/stop)
      let changed = false;
      for (let i = 0; i < 20; i++) {
        await new Promise((r) => setTimeout(r, 500));
        const s = await getPilotStatus();
        const nowOn = s.available && s.hooks_installed;
        if (nowOn !== wasOn) {
          setStatus(s);
          changed = true;
          break;
        }
      }
      if (!changed) {
        setToggleError(
          wasOn
            ? "Couldn't stop pilot — server still running. Try `pilot stop` in a terminal."
            : "Couldn't start pilot — pilot binary not found. Run `pilot start` in a terminal once, then retry."
        );
      } else {
        try { localStorage.setItem("pilot.welcomed", "1"); } catch {}
        setShowWelcome(false);
      }
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      setToggleError(`Pilot toggle failed: ${msg}`);
    } finally {
      setLoading(null);
    }
  };

  const dismissWelcome = () => {
    try { localStorage.setItem("pilot.welcomed", "1"); } catch {}
    setShowWelcome(false);
  };

  return (
    <div className="h-full flex flex-col">
      {/* Header */}
      <div className="px-6 py-4 border-b border-border/50">
        <div className="flex items-center justify-between">
          <div className="flex items-center gap-3">
            <div
              className={`w-2.5 h-2.5 rounded-full ${
                pilotOn ? "bg-success animate-pulse" : "bg-muted-foreground"
              }`}
            />
            <h1 className="text-lg font-semibold">Pilot</h1>
            <button
              onClick={handleTogglePilot}
              disabled={loading === "pilot"}
              className={`px-3 py-1 text-xs font-medium rounded transition-colors min-w-[72px] ${
                loading === "pilot"
                  ? "bg-accent text-muted-foreground"
                  : pilotOn
                    ? "bg-success/15 text-success hover:bg-success/25"
                    : "bg-muted text-muted-foreground hover:bg-muted/80"
              }`}
            >
              {loading === "pilot" ? (
                <span className="flex items-center gap-1.5">
                  <span className="inline-block w-3 h-3 border-2 border-muted-foreground/30 border-t-muted-foreground rounded-full animate-spin" />
                  {pilotOn ? "Stopping" : "Starting"}
                </span>
              ) : pilotOn ? "On" : "Off"}
            </button>
          </div>
          <div className="flex items-center gap-4 text-sm text-muted-foreground">
            <span title="Auto-approved">
              <span className="text-success">&#x2713;</span>{" "}
              {status.stats.approvals_auto}
            </span>
            <span title="Escalated">
              <span className="text-warning">&#x26A0;</span>{" "}
              {status.stats.approvals_escalated}
            </span>
            <span title="Auto-responded">
              <span className="text-info">&#x27A4;</span>{" "}
              {status.stats.auto_responses}
            </span>
            {status.monthly_spend_cap_usd > 0 && (
              <span title="Monthly Anthropic evaluator spend">
                ${(status.monthly_usage?.estimated_cost_usd ?? 0).toFixed(2)} / $
                {status.monthly_spend_cap_usd.toFixed(0)}
              </span>
            )}
            <button
              onClick={() => navigate("/settings")}
              className="px-2 py-1 text-xs text-muted-foreground hover:text-foreground rounded hover:bg-muted/50 transition-colors"
            >
              Settings
            </button>
          </div>
        </div>
      </div>

      {/* Toggle error */}
      {toggleError && (
        <div className="px-6 py-2 border-b border-border/50 bg-destructive/10">
          <div className="flex items-start justify-between gap-3 text-xs text-destructive">
            <span>{toggleError}</span>
            <button
              onClick={() => setToggleError(null)}
              className="text-destructive/70 hover:text-destructive flex-shrink-0"
            >
              &times;
            </button>
          </div>
        </div>
      )}

      {/* First-run welcome modal */}
      {showWelcome && (
        <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60">
          <div className="bg-background border border-border rounded-lg shadow-lg p-6 max-w-sm mx-4 space-y-4">
            <h2 className="text-lg font-semibold">Welcome to Pilot</h2>
            <p className="text-sm text-muted-foreground">
              Pilot auto-approves safe Claude Code and Codex tool calls, escalates risky ones,
              and nudges agents when they stop too early. Enable it to install hooks
              and start the server.
            </p>
            <div className="flex gap-2 justify-end">
              <button
                onClick={dismissWelcome}
                className="px-3 py-1.5 text-sm text-muted-foreground hover:text-foreground rounded hover:bg-muted/50 transition-colors"
              >
                Not now
              </button>
              <button
                onClick={handleTogglePilot}
                disabled={loading === "pilot"}
                className="px-4 py-1.5 text-sm font-medium bg-primary text-primary-foreground rounded hover:bg-primary/90 transition-colors disabled:opacity-50"
              >
                {loading === "pilot" ? "Enabling..." : "Enable pilot"}
              </button>
            </div>
          </div>
        </div>
      )}

      {/* Pending approvals */}
      {pendingApprovals.size > 0 && (
        <div className="px-6 py-3 space-y-2 border-b border-border/50">
          {Array.from(pendingApprovals.values()).map((pending) => (
            <GracePeriodCard
              key={pending.id}
              pending={pending}
              ssePort={ssePort}
            />
          ))}
        </div>
      )}

      {/* Filter tabs */}
      {allActions.length > 0 && (
        <div className="px-6 pt-3 pb-1 flex items-center gap-1">
          {(Object.keys(FILTER_LABELS) as ActionFilter[]).map((f) => (
            <button
              key={f}
              onClick={() => setFilter(f)}
              className={`px-2 py-1 text-xs rounded transition-colors ${
                filter === f
                  ? "bg-accent text-foreground"
                  : "text-muted-foreground hover:text-foreground"
              }`}
            >
              {FILTER_LABELS[f]}
            </button>
          ))}
        </div>
      )}

      {/* Timeline */}
      <div className="flex-1 overflow-auto px-6 py-3">
        {filteredActions.length > 0 ? (
          <div className="space-y-1">
            {filteredActions.map((action, i) => (
              <div key={i}>
                <button
                  onClick={() => setExpandedAction(expandedAction === i ? null : i)}
                  className="w-full flex items-start gap-2 text-[11px] hover:bg-muted/30 rounded px-1 py-0.5 transition-colors"
                >
                  <ActionIcon type={action.action_type} />
                  <ApprovalSource action={action} />
                  {cwdLabel(action.cwd) && (
                    <span className="text-[9px] px-1 py-0.5 rounded bg-info/15 text-info font-medium flex-shrink-0">
                      {cwdLabel(action.cwd)}
                    </span>
                  )}
                  <span className="text-muted-foreground flex-shrink-0">
                    {timeAgo(action.timestamp)}
                  </span>
                  <span className="truncate text-left flex-1" title={action.detail}>
                    {action.detail}
                  </span>
                  {action.confidence !== null && action.confidence !== undefined && (
                    <span className="text-muted-foreground flex-shrink-0">
                      {Math.round(action.confidence * 100)}%
                    </span>
                  )}
                </button>
                {expandedAction === i && (
                  <div className="ml-5 px-2 py-1.5 text-[10px] bg-muted/20 rounded mb-1 space-y-1">
                    <div className="text-muted-foreground">
                      <span className="font-medium">Type:</span> {action.action_type}
                    </div>
                    {action.tool_name && (
                      <div className="text-muted-foreground">
                        <span className="font-medium">Tool:</span> {action.tool_name}
                      </div>
                    )}
                    {action.tool_input && (
                      <div className="text-muted-foreground">
                        <span className="font-medium">Input:</span>{" "}
                        <span className="font-mono break-all">
                          {action.tool_input.length > 300
                            ? action.tool_input.slice(0, 300) + "..."
                            : action.tool_input}
                        </span>
                      </div>
                    )}
                    <div className="text-muted-foreground">
                      <span className="font-medium">Detail:</span> {action.detail}
                    </div>
                    {action.confidence !== null && action.confidence !== undefined && (
                      <div className="text-muted-foreground">
                        <span className="font-medium">Confidence:</span>{" "}
                        {Math.round(action.confidence * 100)}%
                      </div>
                    )}
                    <div className="text-muted-foreground">
                      <span className="font-medium">Time:</span>{" "}
                      {new Date(action.timestamp).toLocaleTimeString()}
                    </div>
                  </div>
                )}
              </div>
            ))}
          </div>
        ) : (
          <div className="text-sm text-muted-foreground">
            No actions yet.
          </div>
        )}
      </div>
    </div>
  );
}

function GracePeriodCard({
  pending,
  ssePort,
}: {
  pending: PilotPendingApproval;
  ssePort: number;
}) {
  const [remaining, setRemaining] = useState(0);
  const [acting, setActing] = useState(false);

  useEffect(() => {
    const update = () => {
      const left = Math.max(
        0,
        (new Date(pending.expires_at).getTime() - Date.now()) / 1000
      );
      setRemaining(left);
    };
    update();
    const interval = setInterval(update, 100);
    return () => clearInterval(interval);
  }, [pending.expires_at]);

  const progress = Math.max(0, remaining / pending.grace_period_s);

  const handleApprove = async () => {
    setActing(true);
    await approvePending(ssePort, pending.id);
  };

  const handleReject = async () => {
    setActing(true);
    await rejectPending(ssePort, pending.id);
  };

  const isInterrogation = pending.source === "interrogate";

  return (
    <div className="bg-warning/5 border border-warning/30 rounded-md p-3 space-y-2">
      <div className="flex items-center gap-2 text-xs">
        <span className="text-warning font-medium">
          {isInterrogation ? "Off-Track?" : "Pending"}
        </span>
        <span className="text-muted-foreground font-mono">{pending.tool_name}</span>
        <div className="flex-1" />
        <span className="text-xs text-muted-foreground">
          {Math.ceil(remaining)}s
        </span>
      </div>

      <div className="h-1 bg-muted rounded-full overflow-hidden">
        <div
          className="h-full bg-warning transition-all duration-100"
          style={{ width: `${progress * 100}%` }}
        />
      </div>

      {pending.tool_input && (
        <div className="text-[10px] text-muted-foreground font-mono truncate">
          {pending.tool_input.slice(0, 120)}
          {pending.tool_input.length > 120 ? "..." : ""}
        </div>
      )}

      <div className="text-[10px] text-muted-foreground">
        {pending.reason} ({Math.round(pending.confidence * 100)}% confident)
      </div>

      <div className="flex gap-2">
        {isInterrogation ? (
          <>
            <button
              onClick={handleApprove}
              disabled={acting}
              className="px-3 py-1 text-xs bg-muted text-muted-foreground rounded hover:bg-muted/80 transition-colors disabled:opacity-50"
            >
              Continue
            </button>
            <button
              onClick={handleReject}
              disabled={acting}
              className="px-3 py-1 text-xs bg-warning/15 text-warning rounded hover:bg-warning/25 transition-colors disabled:opacity-50"
            >
              Interrupt
            </button>
          </>
        ) : (
          <>
            <button
              onClick={handleReject}
              disabled={acting}
              className="px-3 py-1 text-xs bg-destructive/15 text-destructive rounded hover:bg-destructive/25 transition-colors disabled:opacity-50"
            >
              Block
            </button>
            <button
              onClick={handleApprove}
              disabled={acting}
              className="px-3 py-1 text-xs bg-success/15 text-success rounded hover:bg-success/25 transition-colors disabled:opacity-50"
            >
              Allow
            </button>
          </>
        )}
      </div>
    </div>
  );
}
