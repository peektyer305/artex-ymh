import type { TaskGoal } from "@/lib/types/exploration";

import { arr, del, get, patch, post } from "./transport";

export const goalsApi = {
  // ---- ゴール管理（概要）----
  // 本タスクの全ゴール（text/vulnclass/state）。
  taskGoals: (id: string) =>
    get<{ goals: TaskGoal[] | null }>(`/tasks/${id}/goals`).then((response) => ({ goals: arr(response.goals) })),

  // 手動でゴールを追加：グラフへ書き込み planner に通知し、タスクを復活させる。
  addGoal: (id: string, text: string, vulnclass?: string) =>
    post<TaskGoal>(`/tasks/${id}/goals`, { text, vulnclass: vulnclass ?? "" }),

  // 手動でゴールのテキスト（と vulnclass）を変更：planner に「old から new へ」通知し、タスクを復活させる。
  updateGoal: (id: string, goalId: string, text: string, vulnclass?: string) =>
    patch<TaskGoal>(`/tasks/${id}/goals/${goalId}`, { text, vulnclass: vulnclass ?? "" }),

  // 手動でゴールを削除（ハード削除）：planner に通知し、削除ではタスクを復活させない。
  deleteGoal: (id: string, goalId: string) => del<{ ok: boolean }>(`/tasks/${id}/goals/${goalId}`),
};
