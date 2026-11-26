// Domain types. Cross-domain dependencies are type-only.

// ---- Agent triggers (P3 スケジューリング、カスタム agent のみ) ----
export interface AgentTrigger {
  id: number;
  agent_key: string;
  enabled: boolean;
  interval_sec: number; // 定期実行：N 秒ごと(0=定期なし)
  on_finding: boolean; // 任意のタスクでファインディングを発見したときに発火
  on_goal_met: boolean; // 任意のタスクでゴール達成したときに発火
  on_task_timeout: boolean; // 任意のタスクがタイムアウトしたときに発火
  on_tool_call: boolean; // 選択したツールが呼ばれた(実行完了)ときに発火
  on_task_create: boolean; // 任意のタスクが作成されたときに発火
  interval_message: string; // 各トリガー条件ごとの個別ユーザーメッセージ
  finding_message: string;
  goal_message: string;
  task_timeout_message: string;
  tool_call_message: string;
  task_create_message: string;
  tool_names: string[]; // on_tool_call で選択したツール key(1 つ以上)
  last_fire?: string;
}

// ---- Agents ----
export interface Agent {
  id: string;
  key: string; // 組み込みは goals/planner/mainagent/worker；カスタムはユーザー定義の key
  name: string;
  description?: string;
  role: string;
  builtin: boolean;
  enabled: boolean;
  llm_profile_id?: number | null; // バインドした LLM 設定；null/absent = タスク/セッション/グローバルに従う
  max_turns?: number; // 0 = 無制限
  run_seconds?: number; // worker 1 回の実行の実時間上限(秒)；0 = 無制限
  web_search?: boolean; // ウェブ検索を有効にするか(システム全体のスイッチでゲート)
  interactive_shell?: boolean; // 対話型シェルを有効にするか(永続 PTY セッションのツール群)
  // P3 トリガー後の処理方針(カスタム agent のみ意味を持つ)
  trigger_run_mode?: "serial" | "parallel"; // 直列で待機 / トリガーごとに 1 セッションを並列実行
  trigger_merge_mode?: "by_task" | "all" | "none"; // serial のみ：同一タスクでマージ / 全てマージ / マージしない
  trigger_max_parallel?: number; // parallel のみ：agent ごとの並列上限；0=無制限
  // バインド数(一覧 API のみ返却)：参照可能な MCP / 参照可能な Skill / バインドツール
  mcp_count?: number;
  skill_count?: number;
  tool_count?: number;
}

export interface PromptVar {
  name: string;
  description: string;
  example: string;
  source: "exploration" | "runtime" | "distilled";
}

export interface PromptVersion {
  version: number;
  ts: string;
  note: string;
  template_text: string;
}

export interface AgentDetail {
  agent: Agent;
  prompt: string;
  variables: PromptVar[];
  versions: PromptVersion[];
  visibility: { mcp: number[]; skill: string[] };
  // バインド可能な LLM 設定の候補(「デフォルトモデル」プルダウン用)；現在のバインドは agent.llm_profile_id を参照
  llm_profiles?: { id: number; name: string; model: string; is_default: boolean }[];
  wrapup_prompt?: string; // 保存済みの締めくくりプロンプト(空=組み込みデフォルトを使用)
  wrapup_default?: string; // 組み込みデフォルトの締めくくりプロンプト(プレースホルダ/デフォルトに戻す用)
  wrapup_max_turns?: number; // 保存済みの締めくくりターン数(0=組み込みデフォルトを使用)
  wrapup_max_turns_default?: number; // 組み込みデフォルトの締めくくりターン数("0=デフォルトN" のヒント用)
  // タスクレベルのタイムアウト時の締めくくりプロンプト(worker/planner のみ、task_timeout_wrapup_supported=true のときだけこの区画を表示)
  task_timeout_wrapup_supported?: boolean;
  task_timeout_wrapup_prompt?: string;
  task_timeout_wrapup_default?: string;
  task_timeout_wrapup_max_turns?: number;
  task_timeout_wrapup_max_turns_default?: number;
}
