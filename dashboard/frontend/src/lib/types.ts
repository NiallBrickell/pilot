export interface PilotStatus {
  available: boolean;
  session_active: boolean;
  session_start: string | null;
  stats: PilotStats;
  recent_actions: PilotAction[];
  has_pending_response: boolean;
  hooks_installed: boolean;
  wrapper_running: boolean;
  sse_available: boolean;
  sse_port: number;
  monthly_usage: MonthlyUsage;
  monthly_spend_cap_usd: number;
}

export interface MonthlyUsage {
  period: string;
  input_tokens: number;
  output_tokens: number;
  estimated_cost_usd: number;
}

export interface PilotStats {
  approvals_auto: number;
  approvals_escalated: number;
  auto_responses: number;
  auto_responses_skipped: number;
}

export interface PilotAction {
  timestamp: string;
  action_type: string;
  detail: string;
  confidence: number | null;
  tool_name?: string;
  tool_input?: string;
  cwd?: string;
  session_id?: string;
}

export interface PilotConfig {
  general: {
    model: string;
    confidence_threshold: number;
    idle_timeout_ms: number;
    pending_response_max_age_s: number;
    grace_period_s: number;
    escalation_timeout_s: number;
    sse_port: number;
    max_concurrent_evals: number;
    evaluator_timeout_ms: number;
    monthly_spend_cap_usd: number;
    input_cost_per_mtok_usd: number;
    output_cost_per_mtok_usd: number;
    interrogation_confidence: number;
    codex_stop_hook_replies: boolean;
  };
  prompts: {
    approval: string;
    auto_respond: string;
  };
}

export interface PilotPendingApproval {
  id: string;
  tool_name: string;
  tool_input: string;
  reason: string;
  source?: string;
  confidence: number;
  expires_at: string;
  grace_period_s: number;
}

export interface PilotLogEntry {
  timestamp: string;
  level: string;
  source: string;
  message: string;
}

export interface PilotApprovalResolved {
  id: string;
  outcome: "approved" | "rejected";
  resolved_by: "human" | "timeout";
}

export type PromptsStateValue =
  | "up_to_date"
  | "behind"
  | "customised"
  | "bootstrapped"
  | "no_config"
  | "parse_error";

export interface PromptsStatus {
  state: PromptsStateValue;
  user_hash: string;
  embedded_hash: string;
  baseline_hash: string;
}

export interface ResetPromptsResult {
  upgraded: boolean;
  backup_path: string;
  reason: string;
  status: PromptsStatus;
}
