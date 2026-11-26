// Domain types. Cross-domain dependencies are type-only.

// ---- Exploration graph (per task) ----
export type ExploreKind = "task" | "begin" | "goal" | "intent" | "fact" | "finding" | "hint" | "digest";

export type GoalState = "open" | "met" | "abandoned";

export type IntentState = "open" | "running" | "paused" | "done" | "blocked" | "exhausted" | "stopped";

export type FindingState = "confirmed" | "dismissed";

export type HintState = "active" | "consumed";

export type ExploreRel = "spawns" | "derived_from" | "yields" | "proves" | "covers";

export interface TaskNode {
  id: string;
  type: ExploreKind;
  payload?: string;
  priority: number; // 0..10
  state: string; // GoalState | IntentState | FindingState | HintState
  origin: string;
  ts: string;
  source_task_id?: string;
  inherited?: boolean;
  delete_reason?: string; // 意図の論理削除(state='deleted')時の削除理由
}

// ゴール管理カード用のゴール(バックエンドが payload を text/vulnclass に分解済み)。
export interface TaskGoal {
  id: string;
  text: string;
  vulnclass?: string;
  state: string; // GoalState
  origin?: string;
  ts: string;
}

// 制約管理カード用の操作制約(allow=許可 / deny=禁止)。
export type ConstraintKind = "allow" | "deny";

export interface TaskConstraint {
  id: string;
  kind: ConstraintKind;
  text: string;
  origin?: string;
  ts?: string;
}
