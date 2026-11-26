// Real backend client. /api/* is proxied to the Go backend (next.config rewrites).
// Returns the domain types in lib/types.ts. Shapes match the backend handlers;
// a few fields the backend serializes differently (e.g. created_at as a unix int)
// are passed through and formatted at the call site.
import { request } from "@/lib/request";
import type { Edge } from "@/lib/types/assets";
import type { ActiveFindingRetest, FindingRetest } from "@/lib/types/conversations";
import type { TaskNode } from "@/lib/types/exploration";
import type {
  Finding,
  FindingAssetTree,
  FindingDeepenResponse,
  FindingGroupsPage,
  FindingQuery,
  FindingStats,
  FindingStatus,
  FindingsPage,
  Severity,
} from "@/lib/types/findings";
import type {
  EvidenceBodyPreview,
  FindingTraffic,
  FindingTrafficDetail,
  TrafficEvidenceRef,
  TrafficEvidenceRole,
} from "@/lib/types/traffic";

import { findingFilterParams } from "./query";
import { arr, del, get, http, patch, post, tq } from "./transport";

export const findingsApi = {
  findings: (task?: string) => get<Finding[]>(`/exploration/findings${tq(task)}`).then(arr),

  findingsPage: (q: FindingQuery) => {
    const p = findingFilterParams(q);
    p.set("page", String(q.page));
    p.set("limit", String(q.pageSize));
    return get<FindingsPage>(`/exploration/findings?${p.toString()}`);
  },

  findingGroups: (q: FindingQuery) => {
    const p = findingFilterParams(q);
    p.set("page", String(q.page));
    p.set("limit", String(q.pageSize));
    return get<FindingGroupsPage>(`/exploration/findings/groups?${p.toString()}`);
  },

  // findingAssetTree は「アセット別」ビューの左ツリーを取る:ファインディングのあるアセットとその祖先のみを含み、
  // ノードは子孫の集計カウントを持つ。ページングしない。ツリーはナビゲーション構造なので一度に全部取る。
  findingAssetTree: (q: Omit<FindingQuery, "page" | "pageSize">) =>
    get<FindingAssetTree>(`/exploration/findings/asset-tree?${findingFilterParams(q).toString()}`),

  findingStats: () => get<FindingStats>("/exploration/findings/stats"),

  // exportFindings はファインディングページのエクスポートを実行しファイルをダウンロードする。scope=selected なら ids(finding_id のリスト)を渡す;
  // scope=filtered なら現在の絞り込み(FindingQuery の絞り込みフィールド)を渡す;scope=all は絞り込みを無視。
  exportFindings: async (opts: {
    format: "md-single" | "md-zip" | "csv" | "json";
    scope: "filtered" | "all" | "selected";
    filters?: Omit<FindingQuery, "page" | "pageSize">;
    ids?: string[];
  }) => {
    const p = new URLSearchParams({ format: opts.format, scope: opts.scope });
    if (opts.scope === "selected") {
      p.set("ids", (opts.ids ?? []).join(","));
    } else if (opts.scope === "filtered" && opts.filters) {
      for (const [k, v] of findingFilterParams(opts.filters)) p.set(k, v);
    }
    const r = await request(`/api/exploration/findings/export?${p.toString()}`, {});
    if (!r.ok) throw new Error(`export: ${r.status}`);
    const blob = await r.blob();
    // ファイル名はバックエンドの Content-Disposition を優先し、取れなければデフォルト名を使う。
    const disp = r.headers.get("Content-Disposition") ?? "";
    const m = disp.match(/filename="?([^"]+)"?/);
    const filename = m?.[1] ?? `findings-export`;
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = filename;
    document.body.appendChild(a);
    a.click();
    a.remove();
    URL.revokeObjectURL(url);
  },

  getFinding: (id: string, contextTaskId?: string) =>
    get<Finding>(
      `/exploration/findings/${id}${contextTaskId ? `?context_task=${encodeURIComponent(contextTaskId)}` : ""}`,
    ),

  findingTraffic: (id: string, contextTask?: string) =>
    get<FindingTraffic>(
      `/exploration/findings/${id}/traffic${contextTask ? `?context_task=${encodeURIComponent(contextTask)}` : ""}`,
    ),

  bindFindingTraffic: (id: string, traffic_refs: TrafficEvidenceRef[], contextTask?: string) =>
    post<FindingTraffic>(
      `/exploration/findings/${id}/traffic${contextTask ? `?context_task=${encodeURIComponent(contextTask)}` : ""}`,
      { traffic_refs },
    ),

  editFindingTraffic: (
    id: string,
    bindingId: string,
    version: number,
    fields: { role: TrafficEvidenceRole; note: string },
    contextTask?: string,
  ) =>
    patch<FindingTraffic>(
      `/exploration/findings/${id}/traffic/${bindingId}${contextTask ? `?context_task=${encodeURIComponent(contextTask)}` : ""}`,
      { version, ...fields },
    ),

  removeFindingTraffic: (id: string, bindingId: string, version: number, contextTask?: string) =>
    del<FindingTraffic>(
      `/exploration/findings/${id}/traffic/${bindingId}${contextTask ? `?context_task=${encodeURIComponent(contextTask)}` : ""}`,
      { version },
    ),

  orderFindingTraffic: (id: string, binding_ids: string[], version: number, contextTask?: string) =>
    http<FindingTraffic>(
      `/exploration/findings/${id}/traffic/order${contextTask ? `?context_task=${encodeURIComponent(contextTask)}` : ""}`,
      { method: "PUT", body: JSON.stringify({ binding_ids, version }) },
    ),

  findingTrafficDetail: (id: string, bindingId: string, contextTask?: string) =>
    get<FindingTrafficDetail>(
      `/exploration/findings/${id}/traffic/${bindingId}${contextTask ? `?context_task=${encodeURIComponent(contextTask)}` : ""}`,
    ),

  findingTrafficBody: (
    id: string,
    bindingId: string,
    side: "request" | "response",
    offset: number,
    contextTask?: string,
  ) =>
    get<EvidenceBodyPreview>(
      `/exploration/findings/${id}/traffic/${bindingId}/body?side=${side}&offset=${offset}${contextTask ? `&context_task=${encodeURIComponent(contextTask)}` : ""}`,
    ),

  downloadFindingTrafficBody: async (
    id: string,
    bindingId: string,
    side: "request" | "response",
    contextTask?: string,
  ) => {
    const response = await request(
      `/api/exploration/findings/${id}/traffic/${bindingId}/body?side=${side}&download=1${contextTask ? `&context_task=${encodeURIComponent(contextTask)}` : ""}`,
      {},
    );
    if (!response.ok) {
      const error = await response.json().catch(() => ({ error: "ダウンロードに失敗しました" }));
      throw new Error(error.error ?? "ダウンロードに失敗しました");
    }
    const blob = await response.blob();
    const url = URL.createObjectURL(blob);
    const a = document.createElement("a");
    a.href = url;
    a.download = `evidence-${bindingId}-${side}.bin`;
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 1000);
  },

  // 脆弱性チェーン:その脆弱性ノードからタスク初期ノードまで遡るサブグラフ(ノード + リレーション)。
  findingLineage: (id: string) => get<{ nodes: TaskNode[]; edges: Edge[] }>(`/exploration/findings/${id}/lineage`),

  setFindingStatus: (id: string, status: FindingStatus) => patch<Finding>(`/exploration/findings/${id}`, { status }),

  setFindingSeverity: (id: string, severity: Severity) => patch<Finding>(`/exploration/findings/${id}`, { severity }),

  // 脆弱性の名前/カテゴリ/深刻度をまとめて保存(ファインディング一覧のインライン編集用)、送るのは登場するフィールドだけ。
  updateFinding: (
    id: string,
    fields: { name?: string; vulnclass?: string; severity?: Severity; status?: FindingStatus },
  ) => patch<Finding>(`/exploration/findings/${id}`, fields),

  // 脆弱性を削除：findings レコード + 由来の探索ノードを削除(ファインディング一覧/タスクのファインディング Tab/探索グラフから一斉に消える)。
  deleteFinding: (id: string) => del<{ deleted: boolean; id: number }>(`/exploration/findings/${id}`),

  findingRetests: (id: string) =>
    get<{ retests: FindingRetest[] }>(`/exploration/findings/${encodeURIComponent(id)}/retests`).then((r) =>
      arr(r.retests),
    ),

  activeFindingRetests: () =>
    get<{ retests: ActiveFindingRetest[] }>("/exploration/findings/retests/active").then((r) => arr(r.retests)),

  startFindingRetest: (id: string, notes: string) =>
    post<{ retest: FindingRetest; created: boolean }>(`/exploration/findings/${encodeURIComponent(id)}/retests`, {
      notes,
    }),

  deepenFinding: (id: string, description: string) =>
    post<FindingDeepenResponse>(`/exploration/findings/${id}/deepen`, { description }),
};
