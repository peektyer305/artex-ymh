import type { PendingScopeRow } from "@/lib/types/intercept";

import { arr, get, post } from "./transport";

export const scopeApi = {
  // ---- 承認待ちのネットワークセグメント(エンジンが承認境界の外で検出したセグメント,人手による権限拡大待ち) ----
  // デフォルトは pending のみ;all=true なら決定済み(approved/dismissed)も含む。
  pendingScope: (taskId: string, all = false) =>
    get<{ items: PendingScopeRow[] }>(`/tasks/${encodeURIComponent(taskId)}/pending-scope${all ? "?all=1" : ""}`).then(
      (r) => arr(r.items),
    ),

  // approve はセグメントをタスク scope に書き込み、以後 guard が許可する;dismiss は無視。
  // 409 = 既に(並行して)決定済み,呼び出し側はリストを更新すればよい。
  decidePendingScope: (id: number, action: "approve" | "dismiss") =>
    post<PendingScopeRow>(`/pending-scope/${id}/decide`, { action }),
};
