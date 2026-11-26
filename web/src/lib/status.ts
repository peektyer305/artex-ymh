import type { TaskStatus } from "@/lib/types";

// Centralised status → color/label semantics, reused across the whole app.
// Spec §8.3: 意図 / カバレッジ / タスク / 深刻度 each have a consistent color set.

export type Tone = "neutral" | "blue" | "green" | "amber" | "red" | "rose" | "violet" | "slate";

export const toneClasses: Record<Tone, string> = {
  neutral: "bg-muted text-muted-foreground border-transparent",
  blue: "bg-blue-500/15 text-blue-600 dark:text-blue-400 border-blue-500/20",
  green: "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400 border-emerald-500/20",
  amber: "bg-amber-500/15 text-amber-600 dark:text-amber-400 border-amber-500/20",
  red: "bg-red-500/15 text-red-600 dark:text-red-400 border-red-500/20",
  // rose は「緊急」用。塗りつぶしの高強調で、視覚的に「高」の淡い赤枠よりはっきり強い。
  rose: "bg-rose-600 text-white border-rose-600 dark:bg-rose-600 dark:text-white",
  violet: "bg-violet-500/15 text-violet-600 dark:text-violet-400 border-violet-500/20",
  slate: "bg-slate-500/15 text-slate-600 dark:text-slate-400 border-slate-500/20",
};

export const toneDot: Record<Tone, string> = {
  neutral: "bg-muted-foreground",
  blue: "bg-blue-500",
  green: "bg-emerald-500",
  amber: "bg-amber-500",
  red: "bg-red-500",
  rose: "bg-white",
  violet: "bg-violet-500",
  slate: "bg-slate-500",
};

interface StatusMeta {
  label: string;
  tone: Tone;
}

const intent: Record<string, StatusMeta> = {
  open: { label: "未割り当て", tone: "slate" },
  running: { label: "実行中", tone: "blue" },
  paused: { label: "一時停止中", tone: "amber" },
  done: { label: "完了", tone: "green" },
  // blocked = モデル/API/ネットワーク障害で再試行を使い切り、実際にはほぼ突破できていない（目標のブロックではない）。
  blocked: { label: "実行エラー", tone: "red" },
  // exhausted = ステップ数/時間予算に達して途中で打ち切られ、結果の一部だけを書き戻した（方向性の探求が尽きたわけではない）。
  exhausted: { label: "実行上限に到達", tone: "violet" },
  // stopped = 過去の論理削除状態（ノードは保持、過去データ）。
  stopped: { label: "停止済み", tone: "slate" },
  // deleted = ユーザーが該当意図を仮削除した（ノードとリネージは保持、削除理由は delete_reason フィールド）。
  deleted: { label: "削除済み", tone: "slate" },
};

const task: Record<string, StatusMeta> = {
  created: { label: "作成済み", tone: "slate" },
  queued: { label: "待機中", tone: "amber" },
  running: { label: "実行中", tone: "blue" },
  paused: { label: "一時停止中", tone: "amber" },
  done: { label: "完了", tone: "green" },
  failed: { label: "失敗", tone: "red" },
  timeout: { label: "タイムアウト", tone: "amber" },
};

const severity: Record<string, StatusMeta> = {
  critical: { label: "緊急", tone: "rose" },
  high: { label: "高", tone: "red" },
  medium: { label: "中", tone: "amber" },
  low: { label: "低", tone: "slate" },
};

const finding: Record<string, StatusMeta> = {
  pending: { label: "未処理", tone: "amber" },
  in_progress: { label: "処理中", tone: "blue" },
  confirmed: { label: "確認済み", tone: "red" },
  resolved: { label: "対応済み", tone: "green" },
  fixed: { label: "修正済み", tone: "green" },
  false_positive: { label: "誤検知", tone: "slate" },
  ignored: { label: "無視", tone: "neutral" },
  duplicate: { label: "重複", tone: "neutral" },
  risk_accepted: { label: "リスク受容", tone: "violet" },
};

const engine: Record<string, StatusMeta> = {
  exploring: { label: "探索中", tone: "blue" },
  paused: { label: "一時停止中", tone: "amber" },
  stalled: { label: "停滞", tone: "red" },
  idle: { label: "アイドル", tone: "neutral" },
};

const goal: Record<string, StatusMeta> = {
  open: { label: "進行中", tone: "blue" },
  met: { label: "達成済み", tone: "green" },
  abandoned: { label: "放棄済み", tone: "slate" },
};

const audit: Record<string, StatusMeta> = {
  allow: { label: "許可", tone: "green" },
  block: { label: "ブロック", tone: "red" },
};

const node: Record<string, StatusMeta> = {
  observed: { label: "観測", tone: "slate" },
  confirmed: { label: "確認", tone: "green" },
  tombstoned: { label: "廃棄", tone: "neutral" },
};

const maps = {
  intent,
  task,
  severity,
  finding,
  engine,
  goal,
  audit,
  node,
} as const;

export type StatusDomain = keyof typeof maps;

export function statusMeta(domain: StatusDomain, key: string): StatusMeta {
  return maps[domain][key] ?? { label: key, tone: "neutral" };
}

export const taskStatusOrder: TaskStatus[] = ["created", "queued", "running", "paused", "done", "failed", "timeout"];
export const taskStatusOptions = taskStatusOrder.map((value) => ({ value, label: statusMeta("task", value).label }));
