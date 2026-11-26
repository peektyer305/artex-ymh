// Domain types. Cross-domain dependencies are type-only.

import type { Activity } from "./activity";

// ---- Intercept Rules ----
export type InterceptAction = "allow" | "deny" | "ask";

export type InterceptMatchTarget = "tool_name" | "tool_input";

export type InterceptMatchType = "string" | "regex";

export interface InterceptRule {
  id: number;
  name: string;
  enabled: boolean;
  priority: number;
  match_target: InterceptMatchTarget;
  match_type: InterceptMatchType;
  pattern: string;
  action: InterceptAction;
  message: string;
  timeout_enabled: boolean;
  timeout_seconds: number;
  timeout_action: "deny" | "allow";
  created_at: string;
  updated_at: string;
}

export interface InterceptPending {
  decision_source?: "rule" | "model" | "unknown" | "";
  id: number;
  rule_id?: number;
  conversation_id?: number;
  task_id?: string;
  agent_name: string;
  tool_name: string;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  tool_input: Record<string, any>;
  status: "pending" | "allowed" | "denied" | "timeout";
  reason: string; // ルールの message またはモデル判定の理由(モデル判定は [模型] プレフィックス付き)
  decided_at?: string;
  created_at: string;
}

// JudgeConfig: モデルによるフォールバック承認(どのインターセプトルールにも該当しない場合のみモデルが判断)のグローバル設定。
export interface JudgeConfig {
  enabled: boolean;
  profile_id: number; // 0 = アクティブ/デフォルト設定に従う
  prompt: string; // 判定プロンプト;GET で未設定ならバックエンドが組み込みテンプレート全文を返す
  timeout_seconds: number; // モデル呼び出しのタイムアウト
  fail_action: "allow" | "ask" | "deny"; // モデルのエラー/タイムアウト/解析不能時のフォールバック
  ask_timeout_seconds: number; // モデルが ask と判定し人手に回した後の承認待ちタイムアウト
  ask_timeout_action: "allow" | "deny"; // 承認タイムアウト後のデフォルト動作
}

// AutoAllowState: 一括許可(guard auto-allow)の現在の状態。enabled=false のとき
// expires_at/remaining_seconds はいずれも 0;期限切れ後はバックエンドの遅延チェックで自動オフ。
export interface AutoAllowState {
  enabled: boolean;
  expires_at: number; // unix 秒
  remaining_seconds: number;
}

// PendingScopeRow: エンジンが認可境界の外を繰り返し探索したネットワークセグメント/ホストで、人手の判断待ち。
// approve = タスク scope に書き込み(以後 guard が許可);dismiss = 無視。status が確定済みのとき
// decided_at に値あり;並行して決定すると 409 を返すため、呼び出し側は一覧を更新すればよい。
export interface PendingScopeRow {
  id: number;
  task_id: number;
  kind: string; // cidr | ip など
  value: string;
  status: "pending" | "approved" | "dismissed";
  hits: number;
  first_seen: string;
  last_seen: string;
  decided_at?: string;
}

export interface InterceptApprovalFilter {
  status?: InterceptPending["status"];
  decision_source?: "rule" | "model" | "unknown";
}

// InterceptApprovalRow enriches InterceptPending with conversation/task and rule context.
export interface InterceptApprovalRow extends InterceptPending {
  conv_title: string; // "" if no linked conversation
  conv_agent_key: string; // "" if no linked conversation
  rule_name: string; // "" if rule was deleted
}

// The exact JSON sent to the review model, retained for all model verdicts.
export interface InterceptReviewInput {
  version: number;
  background?: {
    // worker_summary is retained only for immutable v2/v3 snapshots.
    source: "user_message" | "worker_summary";
    text: string;
    truncated?: boolean;
  };
  // Version 1 snapshots are immutable and remain readable in historical audits.
  task?: {
    task_id: number;
    description: string;
    goal: string;
    constraints: { id: number; kind: string; text: string; origin: string; created_at: number }[];
    truncated?: boolean;
  };
  working_directory?: string;
  worker_intent?: string;
  turn_input?: string;
  background_truncated?: boolean;
  // Legacy v1/v2 snapshots only; v3 never sends execution history.
  history?: {
    tool_use_id: string;
    tool: string;
    arguments_preview: string;
    result: string;
    status: "succeeded" | "failed";
    truncated?: boolean;
  }[];
  history_truncated?: boolean;
  correlation?: "exact" | "ambiguous" | "unavailable";
  tool_name: string;
  arguments: Record<string, unknown>;
}

// Immutable review snapshot plus separately recorded execution outcome.
export interface InterceptAudit {
  model_input?: InterceptReviewInput;
  model_input_digest?: string;
  run_id?: string;
  tool_use_id?: string;
  correlation: "exact" | "ambiguous" | "unavailable";
  input_digest: string;
  user_message: string;
  user_truncated?: boolean;
  context:
    | { kind: string; tool?: string; tool_use_id?: string; text: string; is_error?: boolean; truncated?: boolean }[]
    | null;
  context_truncated?: boolean;
  captured_at: string;
  model_fallback?: boolean;
  initial_action: "allow" | "ask" | "deny";
  initial_reason: string;
  effective_action?: "allow" | "deny";
  decision_reason?: string;
  rule_name?: string;
  config_digest?: string;
  profile_id?: number;
  execution_status: "not_started" | "not_executed" | "awaiting_result" | "succeeded" | "failed" | "unknown";
  output?: string;
  output_truncated?: boolean;
  execution_ended_at?: string;
}

export interface InterceptDetail extends InterceptApprovalRow {
  audit: InterceptAudit | null;
}

// Original execution selected from an approval, never submitted to the reviewer.
export interface InterceptExecution {
  conversation_id: number | null;
  task_id: string | null;
  session: string;
  seq: number;
  items: Activity[];
}
