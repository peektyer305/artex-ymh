// Domain types. Cross-domain dependencies are type-only.

export interface LLMAuditProfile {
  id: number;
  name: string;
  format: string;
  model: string;
}

export interface LLMTransition {
  mode: "automatic" | "manual" | "exhausted";
  reason: string;
  previous?: LLMAuditProfile;
  next?: LLMAuditProfile;
}

export interface TaskLLMResolution {
  profile_id?: number;
  name: string;
  format: string;
  model: string;
  source: "task_chain" | "agent_binding" | "global_profile" | "environment" | "global";
  available: boolean;
  reason?: string;
}

export interface TaskLLMResolutions {
  mainagent: TaskLLMResolution;
  planner: TaskLLMResolution;
  worker: TaskLLMResolution;
}

// ---- LLM config ----
export interface LLMProfile {
  id: string;
  name: string;
  format: "openai" | "anthropic" | "openai-responses";
  base_url?: string;
  proxy?: string;
  model: string;
  api_key_hint?: string;
  rate_per_second: number;
  rate_per_minute: number;
  context_window_k?: number;
  // 思考スイッチ(thinking.type): ""=送信しない(デフォルト) | "disabled"=オフ | "enabled"=オン
  thinking_type?: string;
  // 思考の強度: ""=送信しない(デフォルト) | "low"/"medium"/"high"/"xhigh"/"max"
  reasoning_effort?: string;
  is_default: boolean;
  // プール順位：大きいほど先に選ばれる。アクティブ設定は常にチェーン先頭で、この値とは無関係。
  priority?: number;
  // true = フェイルオーバー先にならない（agent/タスクの明示バインドでは引き続き使用可）。
  pool_exclude?: boolean;
  // true（デフォルト）= ストリーミング(SSE) | false = 真·非ストリーミング(stream:false、一括返却)。
  streaming?: boolean;
  // 1 回の応答の出力上限(token)。0 = このフィールドを送信せず、サーバー側のデフォルト値に任せる。
  // context_window_k と混同しないこと：後者はモデルの総容量で、ローカルの圧縮しきい値にのみ使う。
  max_tokens?: number;
  // 上限に使うリクエストフィールド名、format="openai" のときのみ意味を持つ：
  // ""=max_tokens(デフォルト) | "max_completion_tokens"(OpenAI 推論モデルはこれのみ)
  max_tokens_field?: string;
  // カスタムセッションヘッダ名：非空なら毎回のリクエストにこの HTTP ヘッダを付け、値=現在のセッション/意図の session id。
  // ""=送信しない。session-id ヘッダでプロンプトキャッシュ/スティッキールーティングするゲートウェイ向け。
  session_header_key?: string;
  // この設定によるリトライの上書き（接続/空応答/同一 provider の安全ウィンドウ）。空/全て 0 = グローバルポリシーに従う。
  retry?: LLMRetryOverride;
}

// ---- LLM リトライポリシー ----
// 1 層のリトライの 2 つのノブ。どちらも「0 = 未設定」：
//   attempts    0=デフォルト回数 | -1=この層のリトライをオフ | >0=リトライ回数
//   interval_ms 0=デフォルトの指数バックオフ | >0=この固定ミリ秒間隔に変更
export interface LLMRetryRule {
  attempts: number;
  interval_ms: number;
}

// 1 つの LLM 設定が上書きできる 3 層（いずれも「エンドポイントに従う」リトライ）。
export interface LLMRetryOverride {
  connect: LLMRetryRule; // 接続リトライ：接続リセット/タイムアウト/429/5xx、ストリーム開始前
  empty: LLMRetryRule; // 空応答リトライ：完了したが内容が皆無（openai 形式のみ）
  stream: LLMRetryRule; // 同一 provider の安全ウィンドウリトライ：出力未送出のまま切断した場合の再生
}

// グローバルポリシー = 上記 3 層のデフォルト値 + グローバル専用の 2 層：
//   breaker プール遮断（attempts=何回連続で一時失敗したら遮断するか、interval_ms=固定の冷却時間）
//   intent  意図の再実行（worker が model_error で終わった後に意図全体を再実行）
export interface LLMRetryPolicy extends LLMRetryOverride {
  breaker: LLMRetryRule;
  intent: LLMRetryRule;
}

// ---- LLM プーリング（フェイルオーバー）----
// プールチェーン内での 1 設定の位置と健全性。state:
//   ok       正常
//   degraded 連続失敗があるが遮断しきい値に未達
//   tripped  遮断済み、冷却期間中はスキップ（cooldown_secs は残り秒数）
export interface LLMPoolMember {
  profile_id: string;
  name: string;
  model: string;
  format: string;
  priority: number;
  active: boolean; // 現在アクティブな設定か（常にチェーン先頭）
  excluded: boolean; // pool_exclude：プールに参加しない
  state: "ok" | "degraded" | "tripped";
  fails: number;
  trips: number;
  cooldown_secs: number;
  last_error?: string;
  last_at?: string;
}

export interface LLMPoolStatus {
  enabled: boolean;
  bind_fallback: boolean;
  chain: LLMPoolMember[];
}

// ---- LLM recording ----
export interface LLMRecordItem {
  id: number;
  ts: string;
  model: string;
  profile_name: string;
  session_id: string;
  task_id: string;
  worker: string;
  latency_ms: number;
  input_tokens: number;
  output_tokens: number;
  cache_read: number;
  cache_write: number;
  status: string;
  error?: string;
}

export interface LLMRecordDetail extends LLMRecordItem {
  request_body: string;
  response_body: string;
  // provider が実際に送受信した HTTP 原文：リクエストは buildBody() が送る完全な body（ツール
  // schema を含む）、レスポンスは生の SSE フレーム。上の request_body/response_body は正規化ビューで、
  // ツール schema と tool_use ブロックを落としている。古いレコードでは空。
  raw_request?: string;
  raw_response?: string;
}

// One distinct task with its LLM-record count (task picker on the records page).
export interface LLMTask {
  task_id: string;
  count: number;
}
