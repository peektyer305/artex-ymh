import type {
  Credential,
  IntranetTopology,
  SessionExecResult,
  SessionFsEntry,
  SessionReadResult,
  ShellSession,
  TunnelInfo,
} from "@/lib/types/intranet";

import { arr, del, get, post, taskQuery } from "./transport";

export const intranetApi = {
  // ---- イントラネット運用（トポロジ / セッション / トンネル / 認証情報）----
  // 4 つのリストは任意の taskId に対応(?task_id= を付ける);渡さなければグローバルビュー。
  intranetTopology: (taskId?: string) =>
    get<IntranetTopology>(`/intranet/topology${taskQuery(taskId)}`).then((r) => ({
      nodes: arr(r.nodes),
      edges: arr(r.edges),
      segments: arr(r.segments),
    })),

  shellSessions: (taskId?: string) =>
    get<{ items: ShellSession[] }>(`/sessions${taskQuery(taskId)}`).then((r) => arr(r.items)),

  // timeout の単位は秒、省略するとバックエンドがデフォルト値を使う。
  sessionExec: (id: string, command: string, timeout?: number) =>
    post<SessionExecResult>(`/sessions/${encodeURIComponent(id)}/exec`, {
      command,
      ...(timeout ? { timeout } : {}),
    }),

  sessionList: (id: string, path: string) =>
    post<{ entries: SessionFsEntry[] }>(`/sessions/${encodeURIComponent(id)}/list`, { path }).then((r) =>
      arr(r.entries),
    ),

  sessionRead: (id: string, path: string) =>
    post<SessionReadResult>(`/sessions/${encodeURIComponent(id)}/read`, { path }),

  sessionWrite: (id: string, path: string, contentBase64: string) =>
    post<{ ok: boolean; bytes: number }>(`/sessions/${encodeURIComponent(id)}/write`, {
      path,
      content_base64: contentBase64,
    }),

  // 手動の生存確認:成功なら alive=true;失敗なら alive=false で error に理由(バックエンドは既に正直に dead を付ける)。
  probeSession: (id: string) =>
    post<{ alive: boolean; ms?: number; error?: string }>(`/sessions/${encodeURIComponent(id)}/probe`),

  // イントラネットへ引き継ぎ:そのセッションのフットホールドホストを初期 scope にして新しいイントラネットタスクを作り、新タスク id(文字列)を返す。
  // 400 = フットホールドの IP を特定できない;404 = セッションが存在しない。
  handoffSession: (id: string) => post<{ task_id: string }>(`/sessions/${encodeURIComponent(id)}/handoff`),

  deleteSession: (id: string) => del<{ ok: boolean }>(`/sessions/${encodeURIComponent(id)}`),

  tunnels: (taskId?: string) => get<{ items: TunnelInfo[] }>(`/tunnels${taskQuery(taskId)}`).then((r) => arr(r.items)),

  teardownTunnel: (id: string) => post<{ ok: boolean }>(`/tunnels/${encodeURIComponent(id)}/teardown`),

  credentials: (taskId?: string) =>
    get<{ items: Credential[] }>(`/credentials${taskQuery(taskId)}`).then((r) => arr(r.items)),
};
