"use client";

import { fmtBytes } from "@/lib/format";
import { taskStatusOptions } from "@/lib/status";
import { type SortDirection } from "@/lib/sort-preference";
import type { ChatAttachment, DeleteTaskOptions, DeleteTaskResult, Task, TaskArchiveState, TaskStatus } from "@/lib/types";

// fmtBytes renders a human file size for the upload manifest.

// UPLOAD_MARKER labels the auto-appended block of uploaded-file paths inside the task
// description, so re-uploads append under the same block instead of adding a new header.
export const UPLOAD_MARKER = "【アップロードファイル（絶対パス）】";

// appendUploads folds newly-uploaded files' ABSOLUTE paths into the description as a
// Read/Bash-friendly manifest — the worker opens them by path. Keeps one marked block:
// first upload adds the header, later uploads append bullets under it.
export function appendUploads(desc: string, atts: ChatAttachment[]): string {
  const bullets = atts.map((a) => `- ${a.abs ?? a.path}（${fmtBytes(a.size)}）`).join("\n");
  if (desc.includes(UPLOAD_MARKER)) {
    return `${desc.replace(/\s*$/, "")}\n${bullets}\n`;
  }
  const head = desc.trim() ? `${desc.replace(/\s*$/, "")}\n\n` : "";
  return `${head}${UPLOAD_MARKER}ワーカーはReadやBashに保存先のパスを指定してファイルを開けます：${bullets}\n`;
}

// POLL_MS is the task-list refresh interval. Task state moves on the server (planner /
// worker), so the list has to be pulled; 10s is plenty for status / 進捗 / token 変化.
export const POLL_MS = 10_000;

export const MAX_SOURCE_TASKS = 8;

// fmtTokens renders a compact token count (1234 → 1.2k, 2_000_000 → 2M).

// fmtDuration renders a run duration in seconds as a compact human string.

// taskDuration is the task's run duration in seconds: created → now while running,
// created → completion for a finished task, else created → last activity. 0 when it
// never ran (no activity yet).
export function taskDuration(task: Task, nowSec: number): number {
  const start = task.created_unix ?? 0;
  if (!start) return 0;
  const end =
    task.status === "running"
      ? nowSec
      : task.completed_unix && task.completed_unix > 0
        ? task.completed_unix
        : (task.last_activity_unix ?? 0);
  return end > start ? end - start : 0;
}

// deleteDetails takes only the countable part of the result so callers can pass an
// aggregate accumulated over a bulk delete.
export type DeleteCounts = Omit<DeleteTaskResult, "deleted" | "cleanup_warning">;

export function deleteDetails(result: DeleteCounts): string[] {
  const details: string[] = [];
  if (result.assets_deleted > 0) details.push(`アセット削除 ${result.assets_deleted} 件`);
  if (result.assets_detached > 0) details.push(`共有アセットの関連解除 ${result.assets_detached} 件`);
  if (result.traffic_deleted > 0) details.push(`トラフィック削除 ${result.traffic_deleted} 件`);
  if (result.files_deleted) details.push("タスクファイル削除");
  if (result.findings_deleted > 0) details.push(`ファインディング削除 ${result.findings_deleted} 件`);
  if (result.llm_records_deleted > 0) details.push(`LLM リクエスト/レスポンス記録削除 ${result.llm_records_deleted} 件`);
  return details;
}

export function deleteSummary(result: DeleteTaskResult): string {
  const details = deleteDetails(result);
  return details.length > 0 ? `タスクを削除しました（${details.join("、")}）` : "タスクを削除しました";
}

// fmtDateTime renders a unix-seconds timestamp as a compact local date-time
// (MM-DD HH:mm), or "—" when unset.
export function fmtDateTime(unix?: number): string {
  if (!unix || unix <= 0) return "—";
  const d = new Date(unix * 1000);
  const p = (n: number) => String(n).padStart(2, "0");
  return `${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}`;
}

export const STATUS_OPTIONS = taskStatusOptions;

// Select は空文字列の value を受け付けないため、「未分類」は絞り込み・新規作成フォーム・一括移動で
// このセンチネル値を共通利用し、送信時にバックエンドの null へ変換する。
export const UNCATEGORIZED_VALUE = "uncategorized";

// 一時停止可能 = 終端状態でなく未一時停止。バックエンド applyTaskControlWithCause のゲートと一致
// （done/failed/timeout が終端状態）。paused のみ再開可能。行内ボタンと一括制御はこの判定を共有し、
// 片方だけ押せる／押せないという食い違いを防ぐ。
export const PAUSABLE_STATUSES = new Set<TaskStatus>(["created", "queued", "running"]);

export const ARCHIVABLE_STATUSES = new Set<TaskStatus>(["paused", "done", "failed", "timeout"]);

export function taskControlAction(status: TaskStatus): "pause" | "resume" | null {
  if (status === "paused") return "resume";
  if (PAUSABLE_STATUSES.has(status)) return "pause";
  return null;
}

export type TaskSortField = "id" | "created" | "duration" | "status";

export const TASK_SORT_FIELDS: readonly TaskSortField[] = ["id", "created", "duration", "status"];

export const TASK_SORT_PREFERENCE_KEY = "artex_task_list_sort";

export const TASK_FILTER_PREFERENCE_KEY = "artex_task_list_filters";

export const TASK_STATUS_RANK = new Map(STATUS_OPTIONS.map((option, index) => [option.value, index]));

export function taskIsPinned(task: Task): boolean {
  return task.pinned ?? Boolean(task.pinned_at);
}

export function compareTaskIDs(left: string, right: string): number {
  return left.localeCompare(right, undefined, { numeric: true, sensitivity: "base" });
}

export function taskCreatedUnix(task: Task): number {
  if (task.created_unix) return task.created_unix;
  const parsed = Date.parse(task.created_at);
  return Number.isNaN(parsed) ? 0 : Math.floor(parsed / 1000);
}

export function compareTasks(left: Task, right: Task, field: TaskSortField, direction: SortDirection, nowSec: number): number {
  const leftPinned = taskIsPinned(left);
  const rightPinned = taskIsPinned(right);
  if (leftPinned !== rightPinned) return leftPinned ? -1 : 1;
  if (leftPinned && left.pinned_at !== right.pinned_at) {
    return (right.pinned_at ?? "").localeCompare(left.pinned_at ?? "");
  }

  let compared = 0;
  switch (field) {
    case "created":
      compared = taskCreatedUnix(left) - taskCreatedUnix(right);
      break;
    case "duration":
      compared = taskDuration(left, nowSec) - taskDuration(right, nowSec);
      break;
    case "status":
      compared = (TASK_STATUS_RANK.get(left.status) ?? 0) - (TASK_STATUS_RANK.get(right.status) ?? 0);
      break;
    default:
      compared = compareTaskIDs(left.id, right.id);
  }
  if (compared !== 0) return direction === "asc" ? compared : -compared;
  return -compareTaskIDs(left.id, right.id);
}

export const ARCHIVE_PROCESSING_STATES = new Set<TaskArchiveState>([
  "archive_queued",
  "archiving",
  "restore_queued",
  "restoring",
  "delete_queued",
  "deleting",
]);

export const ARCHIVE_FAILED_STATES = new Set<TaskArchiveState>(["archive_failed", "restore_failed", "delete_failed"]);

export const deleteOptionKeys: (keyof DeleteTaskOptions)[] = [
  "delete_assets",
  "delete_traffic",
  "delete_files",
  "delete_findings",
  "delete_llm_records",
];

export const COMPANY_SCOPE_LABELS: Record<string, string> = {
  domain: "ドメイン",
  ip: "IP",
  cidr: "CIDR",
  icp: "ICP",
  keyword: "キーワード",
};

export type CategoryManagementView = number | "uncategorized" | "new";
