import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import type { PilotConfig, PilotLogEntry, PromptsStatus } from "../lib/types";
import {
  getPilotConfig,
  savePilotConfig,
  getPilotLogs,
  getPromptsStatus,
  resetPrompts,
} from "../lib/api";

export function PilotConfigPage() {
  const navigate = useNavigate();
  const [config, setConfig] = useState<PilotConfig | null>(null);
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [logs, setLogs] = useState<PilotLogEntry[]>([]);
  const [showLogs, setShowLogs] = useState(false);
  const [promptsStatus, setPromptsStatus] = useState<PromptsStatus | null>(null);
  const [resetting, setResetting] = useState(false);
  const [resetNotice, setResetNotice] = useState<string | null>(null);
  const [confirmingReset, setConfirmingReset] = useState(false);

  useEffect(() => {
    getPilotConfig()
      .then(setConfig)
      .catch((err) => setError(`Failed to load config: ${err}`));
    getPromptsStatus()
      .then(setPromptsStatus)
      .catch(() => {
        // serve not reachable — banner just stays hidden.
      });
  }, []);

  const handleResetPrompts = async () => {
    setResetting(true);
    setResetNotice(null);
    setError(null);
    setConfirmingReset(false);
    try {
      const result = await resetPrompts();
      setPromptsStatus(result.status);
      const fresh = await getPilotConfig();
      setConfig(fresh);
      setResetNotice(
        result.backup_path
          ? `Prompts updated. Backup at ${result.backup_path}`
          : "Prompts updated.",
      );
      setTimeout(() => setResetNotice(null), 5000);
    } catch (err) {
      setError(`Failed to reset prompts: ${err}`);
    } finally {
      setResetting(false);
    }
  };

  const handleSave = async () => {
    if (!config) return;
    setSaving(true);
    setError(null);
    setSaved(false);
    try {
      await savePilotConfig(config);
      setSaved(true);
      setTimeout(() => setSaved(false), 2000);
      getPromptsStatus().then(setPromptsStatus).catch(() => {});
    } catch (err) {
      setError(`Failed to save: ${err}`);
    } finally {
      setSaving(false);
    }
  };

  if (!config) {
    return (
      <div className="p-6">
        {error ? (
          <p className="text-destructive text-sm">{error}</p>
        ) : (
          <p className="text-muted-foreground text-sm">Loading config...</p>
        )}
      </div>
    );
  }

  return (
    <div className="h-full overflow-auto">
      <div className="p-6 max-w-3xl">
        <div className="flex items-center justify-between mb-6">
          <div>
            <h1 className="text-2xl font-semibold text-foreground">
              Pilot Settings
            </h1>
            <p className="text-xs text-muted-foreground">
              Configure pilot behavior, prompts, and thresholds
            </p>
          </div>
          <div className="flex items-center gap-2">
            <button
              onClick={() => navigate("/")}
              className="px-3 py-1.5 text-xs text-muted-foreground hover:text-foreground transition-colors"
            >
              Back
            </button>
            <button
              onClick={handleSave}
              disabled={saving}
              className="px-4 py-1.5 text-xs bg-primary text-primary-foreground rounded-md hover:opacity-90 transition-opacity disabled:opacity-50"
            >
              {saving ? "Saving..." : saved ? "Saved!" : "Save"}
            </button>
          </div>
        </div>

        {error && (
          <div className="mb-4 p-2 bg-destructive/10 border border-destructive/30 rounded text-xs text-destructive">
            {error}
          </div>
        )}

        <section className="mb-8">
          <h2 className="text-sm font-semibold text-foreground mb-3">
            General
          </h2>
          <div className="space-y-3">
            <Field label="Model" hint="Model for evaluations">
              <input
                type="text"
                value={config.general.model}
                onChange={(e) =>
                  setConfig({
                    ...config,
                    general: { ...config.general, model: e.target.value },
                  })
                }
                className="input-field"
              />
            </Field>

            <Field
              label="Confidence Threshold"
              hint="Minimum confidence for auto-responding (0.0 - 1.0)"
            >
              <div className="flex items-center gap-3">
                <input
                  type="range"
                  min="0"
                  max="1"
                  step="0.05"
                  value={config.general.confidence_threshold}
                  onChange={(e) =>
                    setConfig({
                      ...config,
                      general: {
                        ...config.general,
                        confidence_threshold: parseFloat(e.target.value),
                      },
                    })
                  }
                  className="flex-1"
                />
                <span className="text-xs font-mono w-10 text-right">
                  {config.general.confidence_threshold.toFixed(2)}
                </span>
              </div>
            </Field>

            <Field
              label="Grace Period (seconds)"
              hint="Delay before auto-approvals take effect. 0 = instant."
            >
              <div className="flex items-center gap-3">
                <input
                  type="range"
                  min="0"
                  max="30"
                  step="1"
                  value={config.general.grace_period_s}
                  onChange={(e) =>
                    setConfig({
                      ...config,
                      general: {
                        ...config.general,
                        grace_period_s: parseInt(e.target.value),
                      },
                    })
                  }
                  className="flex-1"
                />
                <span className="text-xs font-mono w-10 text-right">
                  {config.general.grace_period_s}s
                </span>
              </div>
            </Field>

            <Field
              label="Escalation Timeout (seconds)"
              hint="How long to wait for approval on escalated calls."
            >
              <div className="flex items-center gap-3">
                <input
                  type="range"
                  min="0"
                  max="60"
                  step="5"
                  value={config.general.escalation_timeout_s}
                  onChange={(e) =>
                    setConfig({
                      ...config,
                      general: {
                        ...config.general,
                        escalation_timeout_s: parseInt(e.target.value),
                      },
                    })
                  }
                  className="flex-1"
                />
                <span className="text-xs font-mono w-10 text-right">
                  {config.general.escalation_timeout_s}s
                </span>
              </div>
            </Field>

            <Field
              label="Idle Timeout (ms)"
              hint="Wait time before checking for idle pauses"
            >
              <input
                type="number"
                value={config.general.idle_timeout_ms}
                onChange={(e) =>
                  setConfig({
                    ...config,
                    general: {
                      ...config.general,
                      idle_timeout_ms: parseInt(e.target.value) || 0,
                    },
                  })
                }
                className="input-field"
              />
            </Field>

            <Field
              label="Pending Response Max Age (s)"
              hint="Discard pending auto-responses older than this"
            >
              <input
                type="number"
                value={config.general.pending_response_max_age_s}
                onChange={(e) =>
                  setConfig({
                    ...config,
                    general: {
                      ...config.general,
                      pending_response_max_age_s: parseInt(e.target.value) || 0,
                    },
                  })
                }
                className="input-field"
              />
            </Field>

            <Field label="SSE Port" hint="Port for the live event stream server">
              <input
                type="number"
                value={config.general.sse_port}
                onChange={(e) =>
                  setConfig({
                    ...config,
                    general: {
                      ...config.general,
                      sse_port: parseInt(e.target.value) || 9721,
                    },
                  })
                }
                className="input-field"
              />
            </Field>
          </div>
        </section>

        <section className="mb-8">
          <div className="flex items-center gap-2 mb-3">
            <h2 className="text-sm font-semibold text-foreground">Prompts</h2>
            {promptsStatus?.state === "up_to_date" && (
              <span className="text-[10px] text-muted-foreground flex items-center gap-1">
                <span className="w-1.5 h-1.5 rounded-full bg-success" />
                Up to date with default
              </span>
            )}
          </div>

          {promptsStatus?.state === "behind" && (
            <div className="mb-4 p-3 rounded-md border border-info/40 bg-info/10 flex items-center justify-between gap-3">
              <div className="text-xs text-foreground">
                <div className="font-medium">New default prompts available.</div>
                <div className="text-muted-foreground mt-0.5">
                  You haven't edited the prompts, so it's safe to upgrade. Your
                  current file will be backed up first.
                </div>
              </div>
              <button
                onClick={handleResetPrompts}
                disabled={resetting}
                className="px-3 py-1.5 text-xs bg-primary text-primary-foreground rounded-md hover:opacity-90 transition-opacity disabled:opacity-50 whitespace-nowrap"
              >
                {resetting ? "Upgrading..." : "Upgrade"}
              </button>
            </div>
          )}

          {promptsStatus?.state === "customised" && (
            <div className="mb-4 p-3 rounded-md border border-warning/40 bg-warning/10 flex items-center justify-between gap-3">
              <div className="text-xs text-foreground">
                <div className="font-medium">
                  You've customised your prompts.
                </div>
                <div className="text-muted-foreground mt-0.5">
                  Revert to the current defaults? Your edits will be backed up.
                </div>
              </div>
              {confirmingReset ? (
                <div className="flex items-center gap-2 whitespace-nowrap">
                  <button
                    onClick={handleResetPrompts}
                    disabled={resetting}
                    className="px-3 py-1.5 text-xs bg-warning/20 border border-warning/50 text-foreground rounded-md hover:bg-warning/30 transition-colors disabled:opacity-50"
                  >
                    {resetting ? "Reverting..." : "Yes, revert"}
                  </button>
                  <button
                    onClick={() => setConfirmingReset(false)}
                    disabled={resetting}
                    className="px-3 py-1.5 text-xs text-muted-foreground hover:text-foreground transition-colors disabled:opacity-50"
                  >
                    Cancel
                  </button>
                </div>
              ) : (
                <button
                  onClick={() => setConfirmingReset(true)}
                  disabled={resetting}
                  className="px-3 py-1.5 text-xs border border-border text-foreground rounded-md hover:bg-muted/50 transition-colors disabled:opacity-50 whitespace-nowrap"
                >
                  Revert to default
                </button>
              )}
            </div>
          )}

          {resetNotice && (
            <div className="mb-4 p-2 rounded-md border border-border bg-muted/30 text-[11px] text-muted-foreground">
              {resetNotice}
            </div>
          )}

          <Field
            label="Approval Prompt"
            hint="System prompt for tool call approval."
          >
            <textarea
              value={config.prompts.approval}
              onChange={(e) =>
                setConfig({
                  ...config,
                  prompts: { ...config.prompts, approval: e.target.value },
                })
              }
              rows={16}
              className="input-field font-mono text-[11px] leading-relaxed"
            />
          </Field>

          <Field
            label="Auto-Respond Prompt"
            hint="System prompt for idle detection."
          >
            <textarea
              value={config.prompts.auto_respond}
              onChange={(e) =>
                setConfig({
                  ...config,
                  prompts: { ...config.prompts, auto_respond: e.target.value },
                })
              }
              rows={16}
              className="input-field font-mono text-[11px] leading-relaxed"
            />
          </Field>
        </section>

        {/* Debug Logs */}
        <section className="mb-8">
          <div className="flex items-center gap-3 mb-3">
            <h2 className="text-sm font-semibold text-foreground">
              Debug Logs
            </h2>
            <button
              onClick={async () => {
                setShowLogs(!showLogs);
                if (!showLogs) {
                  const l = await getPilotLogs();
                  setLogs(l);
                }
              }}
              className="px-2 py-0.5 text-[10px] text-muted-foreground hover:text-foreground rounded hover:bg-muted/50 transition-colors"
            >
              {showLogs ? "Hide" : "Show"}
            </button>
            {showLogs && (
              <button
                onClick={async () => {
                  const l = await getPilotLogs();
                  setLogs(l);
                }}
                className="px-2 py-0.5 text-[10px] text-muted-foreground hover:text-foreground rounded hover:bg-muted/50 transition-colors"
              >
                Refresh
              </button>
            )}
          </div>
          {showLogs && (
            <div className="bg-background border border-border rounded-md max-h-96 overflow-auto">
              {logs.length === 0 ? (
                <div className="p-3 text-xs text-muted-foreground">No logs yet.</div>
              ) : (
                <table className="w-full text-[10px] font-mono">
                  <tbody>
                    {logs.map((log, i) => (
                      <tr key={i} className="border-b border-border/20 hover:bg-muted/20">
                        <td className="px-2 py-1 text-muted-foreground whitespace-nowrap">
                          {new Date(log.timestamp).toLocaleTimeString()}
                        </td>
                        <td className={`px-1 py-1 whitespace-nowrap ${
                          log.level === "error" ? "text-destructive" :
                          log.level === "warn" ? "text-warning" :
                          log.level === "info" ? "text-info" :
                          "text-muted-foreground"
                        }`}>
                          {log.level}
                        </td>
                        <td className="px-1 py-1 text-muted-foreground whitespace-nowrap">
                          {log.source}
                        </td>
                        <td className="px-2 py-1 break-all">
                          {log.message}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              )}
            </div>
          )}
        </section>
      </div>
    </div>
  );
}

function Field({
  label,
  hint,
  children,
}: {
  label: string;
  hint?: string;
  children: React.ReactNode;
}) {
  return (
    <div className="space-y-1">
      <label className="text-xs font-medium text-foreground">{label}</label>
      {hint && (
        <p className="text-[10px] text-muted-foreground">{hint}</p>
      )}
      {children}
    </div>
  );
}
