// Real backend client. /api/* is proxied to the Go backend (next.config rewrites).
// Returns the domain types in lib/types.ts. Shapes match the backend handlers;
// a few fields the backend serializes differently (e.g. created_at as a unix int)
// are passed through and formatted at the call site.

import { MOCK } from "@/lib/mock/enabled";
import { request } from "@/lib/request";
import type {
  AutoAllowState,
  InterceptApprovalFilter,
  InterceptApprovalRow,
  InterceptDetail,
  InterceptPending,
  InterceptRule,
  JudgeConfig,
} from "@/lib/types/intercept";

import { interceptPageQuery } from "./query";
import { arr, del, get, post, put } from "./transport";

export const interceptApi = {
  // ---- intercept rules ----
  interceptRules: () => get<{ rules: InterceptRule[] }>("/intercept/rules").then((r) => arr(r.rules)),

  createInterceptRule: (rule: Omit<InterceptRule, "id" | "created_at" | "updated_at">) =>
    post<InterceptRule>("/intercept/rules", rule),

  updateInterceptRule: (id: number, rule: Omit<InterceptRule, "id" | "created_at" | "updated_at">) =>
    put<InterceptRule>(`/intercept/rules/${id}`, rule),

  deleteInterceptRule: (id: number) => del<{ deleted: number }>(`/intercept/rules/${id}`),

  toggleInterceptRule: (id: number, enabled: boolean) =>
    post<{ ok: boolean; enabled: boolean }>(`/intercept/rules/${id}/toggle`, { enabled }),

  // ---- intercept pending (ask) ----
  interceptPending: () => get<{ pending: InterceptPending[] }>("/intercept/pending").then((r) => arr(r.pending)),

  interceptGetOne: (id: number) => get<InterceptPending>(`/intercept/pending/${id}`),

  interceptDecide: (id: number, decision: "allowed" | "denied") =>
    post<{ ok: boolean }>(`/intercept/pending/${id}/decide`, { decision }),

  interceptExecution: (id: number, conversationId?: number) =>
    get<import("@/lib/types").InterceptExecution>(
      `/intercept/history/${id}/execution${conversationId ? `?conversation=${conversationId}` : ""}`,
    ),

  interceptDetail: (id: number) => get<InterceptDetail>(`/intercept/history/${id}`),

  interceptHistory: () => get<{ items: InterceptApprovalRow[] }>("/intercept/history").then((r) => arr(r.items)),

  interceptHistoryPage: (page = 1, size = 20, filter: InterceptApprovalFilter = {}) =>
    get<{ items: InterceptApprovalRow[]; total?: number }>(
      `/intercept/history?${interceptPageQuery(page, size, filter)}`,
    ).then((r) => ({
      items: arr(r.items),
      total: r.total ?? r.items?.length ?? 0,
    })),

  interceptTask: (taskId: string, signal?: AbortSignal) =>
    get<{ items: InterceptApprovalRow[] }>(`/intercept/task/${taskId}`, signal).then((r) => arr(r.items)),

  interceptTaskPage: (taskId: string, page = 1, size = 20, filter: InterceptApprovalFilter = {}) =>
    get<{ items: InterceptApprovalRow[]; total?: number }>(
      `/intercept/task/${encodeURIComponent(taskId)}?${interceptPageQuery(page, size, filter)}`,
    ).then((r) => ({
      items: arr(r.items),
      total: r.total ?? r.items?.length ?? 0,
    })),

  // ---- intercept tool-config (グローバルなツールインターセプト範囲) ----
  interceptGetToolConfig: async (): Promise<{ enabled_tools: string[] }> => {
    if (MOCK) return { enabled_tools: ["bash"] };
    const r = await request("/api/intercept/tool-config", {});
    if (!r.ok) throw new Error(await r.text());
    return r.json();
  },

  interceptSetToolConfig: async (enabledTools: string[]): Promise<void> => {
    if (MOCK) return;
    const r = await request("/api/intercept/tool-config", {
      method: "PUT",
      headers: {
        "Content-Type": "application/json",
      },
      body: JSON.stringify({ enabled_tools: enabledTools }),
    });
    if (!r.ok) throw new Error(await r.text());
  },

  // ---- intercept LLM judge (モデルのフォールバック承認,グローバル設定) ----
  interceptGetJudgeConfig: () => get<JudgeConfig>("/intercept/judge"),

  interceptSetJudgeConfig: (cfg: JudgeConfig) => put<{ ok: boolean }>("/intercept/judge", cfg),

  // ---- guard auto-allow (ワンクリック許可,期限付きの ask 自動承認) ----
  getAutoAllow: () => get<AutoAllowState>("/guard/auto-allow"),

  setAutoAllow: (enabled: boolean, hours: number) => put<AutoAllowState>("/guard/auto-allow", { enabled, hours }),
};
