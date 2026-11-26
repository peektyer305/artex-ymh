// Domain types. Cross-domain dependencies are type-only.

// ---- App settings (runtime toggles) ----
export interface Settings {
  traffic_capture: boolean;
  agent_traffic_binding: boolean; // Agent がトラフィックのエビデンスを自動で紐づける、デフォルトはオフ；手動の紐づけには影響しない
  llm_record: boolean; // LLM レコードのスイッチ（デフォルトはオフ）；オフ時は LLM 呼び出しを一切記録しない
  // Web search. brave_key_set / tavily_key_set reflect whether a key is stored
  // (the values are never returned). On PUT, send the corresponding field to set/clear.
  web_search_enabled: boolean;
  web_search_backend: string; // "ddgs" | "brave-free" | "tavily" | "deepseek"
  brave_key_set: boolean;
  tavily_key_set: boolean;
  // write-only: only sent on PUT to store/clear the key.
  brave_search_api_key?: string;
  tavily_search_api_key?: string;
  // 検索エンドポイント用の独立出口プロキシ(http/https/socks5)；トラフィック記録用の MITM プロキシとは無関係。空=直接接続。
  web_search_proxy?: string;
  // グローバル出口プロキシ(http/https/socks5、user:pass 可)、全ターゲットのトラフィックが経由する。トラフィックキャプチャ有効時は
  // MITM の上流；キャプチャ無効時は agent の bash/WebFetch に直接注入。空=直接接続。
  global_proxy?: string;
  python_interpreter?: string; // カスタムスクリプトツールの python インタプリタパス(空=実行時に検出)
  workers?: number; // 並列で動く作業 agent 数(デフォルト 3)；以後に起動するタスクに適用
  // タスク並列上限:同時に「実行中」のタスク数の上限。オフ=無制限;オンで新規タスクが上限超過なら待機し、空きが出たら自動起動。
  task_concurrency_enabled?: boolean; // デフォルト false
  task_concurrency_limit?: number; // 有効時のデフォルト 5
  // LLM プーリング(フェイルオーバー)。デフォルトはオフ；有効時は「モデル未指定」の agent が現在の設定を使えない
  // （残高不足/key 失効/レート制限/サービス異常）ときに次の設定へ自動で切り替わる。
  llm_pool_enabled?: boolean; // デフォルト false
  // 特定の設定をバインドした agent/タスクの失敗時もプールチェーンへフォールバックするか。デフォルト false = バインドは専用。
  llm_pool_bind_fallback?: boolean;
  // 操作制約の注入範囲(デフォルトは全て有効):タスクの allow/deny 制約を対応する agent のシステムプロンプトに埋め込む。
  constraints_inject_planner?: boolean;
  constraints_inject_worker?: boolean;
}
