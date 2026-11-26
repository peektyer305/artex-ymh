import type {
  LLMPoolStatus,
  LLMProfile,
  LLMRecordDetail,
  LLMRecordItem,
  LLMRetryOverride,
  LLMRetryPolicy,
  LLMTask,
} from "@/lib/types/llm";
import type { ModelTokenStat } from "@/lib/types/tokens";

import { arr, del, get, post } from "./transport";

export const llmApi = {
  // ---- LLM ----
  getLLM: () =>
    get<{
      configured: boolean;
      provider: string;
      model: string;
      base_url: string;
      proxy?: string;
      key_set: boolean;
      rate_per_second?: number;
      rate_per_minute?: number;
      context_window_k?: number;
      thinking_type?: string;
      reasoning_effort?: string;
    }>("/llm"),

  setLLM: (
    provider: string,
    model: string,
    base_url: string,
    api_key: string,
    rate_per_second = 0,
    rate_per_minute = 0,
    proxy = "",
    context_window_k = 0,
  ) => post("/llm", { provider, model, base_url, proxy, api_key, rate_per_second, rate_per_minute, context_window_k }),

  testLLM: (
    provider: string,
    model: string,
    base_url: string,
    api_key: string,
    proxy = "",
    thinking_type = "",
    reasoning_effort = "",
    profile_id?: number,
    streaming = true, // この設定の実際の送受信モードでテストし、"ストリーミングは通るが非ストリーミングは通らない"を見逃さない
    session_header_key = "", // 非空=テストリクエストにもそのカスタムセッションヘッダを付ける（値は使い捨ての session id）
  ) =>
    // reply = モデルの実際の応答(切り詰め済み);1 文字も返さない設定はバックエンドが即失敗と判定
    post<{ ok: boolean; error?: string; latency_ms?: number; model?: string; reply?: string }>("/llm/test", {
      provider,
      model,
      base_url,
      proxy,
      api_key,
      thinking_type,
      reasoning_effort,
      profile_id,
      streaming,
      session_header_key,
    }),

  llmProfiles: () => get<{ profiles: LLMProfile[] }>("/llm/profiles").then((r) => arr(r.profiles)),

  saveLLMProfile: (p: {
    id?: number; // omit/0 = create; set = update that profile
    name: string;
    format: string;
    model: string;
    base_url?: string;
    proxy?: string;
    api_key?: string; // blank on update keeps the existing key
    rate_per_second?: number;
    rate_per_minute?: number;
    context_window_k?: number;
    thinking_type?: string; // ""(送信しない)|"disabled"|"enabled"
    reasoning_effort?: string; // ""(送信しない)|"low"|"medium"|"high"|"xhigh"|"max"
    priority?: number; // ポーリング順位、大きいほど先に使う
    pool_exclude?: boolean; // true=フェイルオーバー先にしない
    streaming?: boolean; // true(デフォルト)=ストリーミング | false=非ストリーミング
    max_tokens?: number; // 1 回の応答出力の上限；0=送信せず、サーバ側のデフォルト値で決まる
    max_tokens_field?: string; // ""=max_tokens(デフォルト) | "max_completion_tokens"（openai 形式のみ）
    session_header_key?: string; // 非空=毎回のリクエストにその HTTP ヘッダを付け、値=現在のセッション session id；""=送信しない
    retry?: LLMRetryOverride; // この設定の再試行オーバーライド；各項目 0 のまま = グローバルの再試行ポリシーに従う
  }) => post<{ id: number }>("/llm/profiles", p),

  deleteLLMProfile: (id: string) => del<{ deleted: number }>(`/llm/profiles/${id}`),

  activateLLMProfile: (id: string) => post<{ ok: boolean }>("/llm/profiles/active", { id: Number(id) }),

  // ポーリングチェーンの実際の順序 + 各設定のサーキットブレーカー状態。
  llmPool: () => get<LLMPoolStatus>("/llm/pool"),

  // サーキットブレーカーを解除し、次の呼び出しでこの設定をすぐ再試行させる；id を渡さない = 全部解除。
  resetLLMPool: (id?: string) => post<LLMPoolStatus>("/llm/pool/reset", { id: id ? Number(id) : 0 }),

  // グローバルの再試行ポリシー（5 層それぞれの回数+間隔）。全て 0 = 全て組み込みのデフォルト。
  llmRetryPolicy: () => get<LLMRetryPolicy>("/llm/retry-policy"),

  saveLLMRetryPolicy: (p: LLMRetryPolicy) => post<LLMRetryPolicy>("/llm/retry-policy", p),

  fetchLLMModels: (provider: string, base_url: string, api_key: string, proxy = "", profile_id?: number) =>
    post<{ ok: boolean; error?: string; models?: string[] }>("/llm/models", {
      provider,
      base_url,
      api_key,
      proxy,
      profile_id,
    }),

  // ---- LLM records ----
  llmRecords: (params?: { model?: string; session?: string; task?: string; page?: number; size?: number }) => {
    const sp = new URLSearchParams();
    if (params?.model) sp.set("model", params.model);
    if (params?.session) sp.set("session", params.session);
    if (params?.task) sp.set("task", params.task);
    if (params?.page !== undefined) sp.set("page", String(params.page));
    sp.set("size", String(params?.size ?? 50));
    return get<{ records: LLMRecordItem[]; total: number }>(`/llm/records?${sp}`);
  },

  llmRecordDetail: (id: number) => get<LLMRecordDetail>(`/llm/records/${id}`),

  llmTasks: () => get<{ tasks: LLMTask[] }>(`/llm/records/tasks`),

  llmRecordsDeleteTask: (task: string) => del<{ deleted: number }>(`/llm/records?task=${encodeURIComponent(task)}`),

  // 本タスクの token 使用量をモデル別に集計（常時記録の llm_usage 計量台帳から、呼び出しごとに正確、
  // per-agent バインド / ポーリング / 中断の消費もカバー）。
  tokensByModel: (task: string) =>
    get<{ models: ModelTokenStat[] }>(`/llm/records/by-model?task=${encodeURIComponent(task)}`),
};
