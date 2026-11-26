// Domain types. Cross-domain dependencies are type-only.

// ---------------------------------------------------------------------------
// イントラネット運用（/intranet）：トポロジ / セッション（webshell) / トンネル / 認証情報。
// 形状はバックエンドの契約と 1 対 1 対応；Go 側の nil slice は null にシリアライズされ、api 層が arr() で一括フォールバック。
// ---------------------------------------------------------------------------

/** GET /api/intranet/topology のホストノード。 */
export interface IntranetNode {
  id: string;
  ip: string;
  segment: string;
  /** 生存セッションがあるか（図上でハイライト/枠線）。 */
  has_session: boolean;
  alive_sessions: number;
  /** プラットフォーム自身（コールバック側）のノード、図上で特殊表示。 */
  is_callback: boolean;
}

/** GET /api/intranet/topology のエッジ（トンネル経路）。state: alive / stopped / error。 */
export interface IntranetEdge {
  id: string;
  kind: string;
  from: string;
  to: string;
  state: string;
}

export interface IntranetSegment {
  name: string;
  color_key: string;
}

export interface IntranetTopology {
  nodes: IntranetNode[];
  edges: IntranetEdge[];
  segments: IntranetSegment[];
}

/** GET /api/sessions のセッション（webshell）レコード。status: alive / dead。 */
export interface ShellSession {
  id: string;
  /** ウェブシェルの種類、例: beholder / godzilla / antsword。 */
  kind: string;
  url: string;
  lang: string;
  platform: string;
  /** ホスト識別子（トポロジノード id または ip）。 */
  host_asset_id: string;
  status: string;
  last_beat: string;
  /** このセッションを作成したタスク id（バックエンドは JSON number、omitempty；0/未指定 = 未関連）。 */
  created_by_task?: number;
}

/** POST /api/sessions/{id}/exec の結果。 */
export interface SessionExecResult {
  stdout: string;
  stderr: string;
  ms: number;
  timed_out: boolean;
  /** チャネル側のエラー（例: セッション切断）。HTTP 層は成功だが実行が失敗したときバックエンドが設定。 */
  error?: string;
}

/** POST /api/sessions/{id}/list のディレクトリ項目。 */
export interface SessionFsEntry {
  name: string;
  is_dir: boolean;
  size: number;
}

/** POST /api/sessions/{id}/read の結果：テキストの content か base64（バイナリ）のどちらか。 */
export interface SessionReadResult {
  content?: string;
  content_base64?: string;
  truncated: boolean;
}

/** GET /api/tunnels のトンネル台帳。state: alive / stopped / error。 */
export interface TunnelInfo {
  id: string;
  /** 所属タスク id（バックエンドは JSON number；0 = 未関連）。 */
  task_id: number;
  kind: string;
  state: string;
  /** プラットフォーム側の入口（chisel server のバインドアドレス/ポート）。 */
  listen_host: string;
  listen_port: number;
  /** portfwd：トンネル経由でアクセスするイントラネットのターゲット。 */
  target_host?: string;
  target_port?: number;
  via_session_id: string;
  last_check: string;
  /** 生存確認/接続確立の失敗理由（state=error のとき通常バックエンドが設定）。 */
  error?: string;
}

/** GET /api/credentials の認証情報レコード（シークレットはマスク済み）。 */
export interface Credential {
  id: string;
  cred_type: string;
  username: string;
  domain: string;
  secret_masked: string;
  source: string;
  verified: boolean;
  created_at: string;
  /** 所属タスク id（バックエンドは JSON number；0 = 未関連）。 */
  task_id?: number;
}
