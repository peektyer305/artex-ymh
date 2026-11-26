import type { CoverageAssetRefs } from "@/lib/types/assets";
import type { TaskConstraint } from "@/lib/types/exploration";
import type { TaskScopeRow } from "@/lib/types/tasks";

import { arr, del, get, patch, post } from "./transport";

export const constraintsApi = {
  // ---- 操作制約管理（概要）----
  // 本タスクの全操作制約（allow/deny）。
  taskConstraints: (id: string) =>
    get<{ constraints: TaskConstraint[] | null }>(`/tasks/${id}/constraints`).then((response) => ({
      constraints: arr(response.constraints),
    })),

  // 制約を追加（planner へは通知せず、次回の計画が自然に読む）。
  addConstraint: (id: string, text: string, kind: TaskConstraint["kind"]) =>
    post<TaskConstraint>(`/tasks/${id}/constraints`, { text, kind }),

  // 制約を変更（テキスト + allow/deny）。
  updateConstraint: (id: string, constraintId: string, text: string, kind: TaskConstraint["kind"]) =>
    patch<TaskConstraint>(`/tasks/${id}/constraints/${constraintId}`, { text, kind }),

  // 制約を削除。
  deleteConstraint: (id: string, constraintId: string) =>
    del<{ ok: boolean }>(`/tasks/${id}/constraints/${constraintId}`),

  // 本タスクのテスト範囲リスト（元タスクから継承した範囲を含む）。
  taskScope: (id: string) => get<{ scope: TaskScopeRow[] }>(`/tasks/${id}/scope`),

  // 手動でテスト範囲を 1 件追加（kind=company/root_domain/subdomain/ip/cidr）。
  addTaskScope: (id: string, kind: string, value: string, reason?: string) =>
    post<TaskScopeRow>(`/tasks/${id}/scope`, { kind, value, reason }),

  // 本タスクのテスト範囲を 1 件削除。
  deleteTaskScope: (id: string, scopeId: number) => del<{ ok: boolean }>(`/tasks/${id}/scope/${scopeId}`),

  // あるアセットが本タスクで関連する意図 / ファクト / ファインディング（カバレッジグラフのノードドロワー用）。
  taskAssetRefs: (id: string, assetId: number) => get<CoverageAssetRefs>(`/tasks/${id}/asset-refs?asset_id=${assetId}`),
};
