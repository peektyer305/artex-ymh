// Domain types. Cross-domain dependencies are type-only.

import type { IntentState } from "./exploration";

// ---- Findings ----
export type Severity = "critical" | "high" | "medium" | "low";

// 脆弱性の対応ステータス:未処理 / 処理中 / 確認済み / 対応済み / 修正済み / 誤検知 / 無視 / 重複 / リスク受容。
export type FindingStatus =
  | "pending"
  | "in_progress"
  | "confirmed"
  | "resolved"
  | "fixed"
  | "false_positive"
  | "ignored"
  | "duplicate"
  | "risk_accepted";

// FindingAsset は脆弱性に紐づくアセット(ラベルはバックエンドでレンダリング済み)。
export interface FindingAsset {
  id: string;
  type: string;
  label: string;
}

export interface Finding {
  traffic_count?: number;
  evidence_version?: number;
  report_evidence_version?: number;
  report_stale?: boolean;
  id: string;
  finding_id?: string; // 独立 findings テーブルの行 id、ステータス更新のハンドル(タスク内の古いノードでは欠落あり)
  vulnclass: string;
  name?: string; // 脆弱性名;空なら表示は vulnclass にフォールバック
  severity: Severity;
  status: FindingStatus;
  summary: string;
  evidence: string;
  report?: string; // 詳細レポート(Markdown);詳細 API のみ返却、一覧では空
  intent_id?: string;
  param_id?: string;
  task_id?: string;
  task_description?: string;
  source_task_id?: string;
  inherited?: boolean;
  assets?: FindingAsset[];
  ts: string;
}

// FindingsPage はファインディング一覧のサーバー側ページングレスポンス。
export interface FindingsPage {
  items: Finding[];
  total: number;
  page: number;
  page_size: number;
}

export interface FindingGroup {
  task_id: string | number | null;
  task_name?: string; // 任意のタスク名;空/未指定=名称未設定
  task_description: string;
  task_status: string;
  count: number;
  critical: number;
  high: number;
  medium: number;
  low: number;
  last_found_at: string;
}

export interface FindingGroupsPage {
  items: FindingGroup[];
  total: number;
  finding_total: number;
  page: number;
  page_size: number;
}

export interface FindingDeepenResponse {
  task_id: string;
  intent_id: string;
  state: IntentState;
  queued: boolean;
}

// FindingStats はファインディング全表の集計(統計カード + 脆弱性タイプのプルダウン)、サーバー側で計算しページングの影響を受けない。
export interface FindingStats {
  total: number;
  pending: number;
  critical: number;
  high: number;
  medium: number;
  low: number;
  vulnclasses: string[];
  tasks: FindingTaskOption[];
}

// FindingTaskOption はファインディングページの「タスク別」絞り込みプルダウンの 1 項目:脆弱性のあるタスク(説明が空ならタスク削除済み、
// フロントは id を表示)とその脆弱性件数。
export interface FindingTaskOption {
  id: string | number;
  name?: string; // 任意のタスク名;空/未指定=名称未設定
  description: string;
  count: number;
}

// FindingQuery はファインディング一覧のページング/絞り込み/並び替えパラメータ。
export interface FindingQuery {
  page: number;
  pageSize: number;
  severity?: "all" | Severity;
  status?: "all" | FindingStatus;
  vulnclass?: string;
  task?: string; // タスク id;"all"/空 = タスクで絞り込まない
  query?: string;
  sort?: "severity" | "time";
  // アセットツリーのノード key;ノードを選択 = そのサブツリー全体を選択。空 = アセットで絞り込まない。
  assetScope?: string;
}

// ---- Findings by asset (アセットビュー) ----
export type FindingAssetKind = "company" | "root_domain" | "subdomain" | "ip" | "service" | "app" | "endpoint" | "none";

// FindingAssetNode はアセットツリーのノード。key は a:<id>(アセット)、c:<id>(企業)、
// r:<domain>(DB にアセット行のないルートドメイン)、__none__(アセット未関連)の形。
export interface FindingAssetNode {
  key: string;
  parent?: string;
  kind: FindingAssetKind;
  label: string;
  asset_id?: number;
  company_id?: number;
  self: number; // このアセット直下のファインディング数
  total: number; // 子孫を含み、ファインディング単位で重複排除
  critical: number;
  high: number;
  medium: number;
  low: number;
  last_found_at: string;
}

export interface FindingAssetTree {
  nodes: FindingAssetNode[];
  finding_total: number;
  truncated: boolean;
  dropped_kinds?: string[];
}

// FINDING_UNASSIGNED_ASSET はバックエンドの db.FindingUnassignedAsset に対応。
export const FINDING_UNASSIGNED_ASSET = "__none__";
