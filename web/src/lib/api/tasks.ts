import type { Activity } from "@/lib/types/activity";
import type { TaskLLMResolutions } from "@/lib/types/llm";
import type {
  ArchiveBatchItem,
  BatchCategoryItem,
  BatchControlItem,
  DeleteTaskOptions,
  DeleteTaskResult,
  Task,
  TaskArchive,
  TaskArchivePage,
  TaskCategory,
  TaskTemplate,
} from "@/lib/types/tasks";

import { arr, del, get, http, patch, post, put } from "./transport";

export const tasksApi = {
  // ---- tasks ----
  tasks: (signal?: AbortSignal) =>
    http<{ tasks: Task[]; active: string }>("/tasks", { signal }).then((r) => ({
      tasks: arr(r.tasks),
      active: r.active ?? "",
    })),

  task: (id: string) => get<Task>(`/tasks/${encodeURIComponent(id)}`),

  createTask: (input: {
    name?: string;
    categoryId?: number;
    description: string;
    goal: string;
    llmProfileIds?: number[];
    sourceTaskIds?: string[];
    companyIds?: number[];
    timeoutSeconds?: number;
    seedFirstIntent?: boolean;
    planHeartbeatSeconds?: number;
    coverageEnabled?: boolean;
  }) =>
    post<Task>("/tasks", {
      name: input.name ?? "",
      category_id: input.categoryId ?? null,
      description: input.description,
      goal: input.goal,
      llm_profile_ids: input.llmProfileIds ?? [],
      source_task_ids: input.sourceTaskIds ?? [],
      company_ids: input.companyIds ?? [],
      timeout_seconds: input.timeoutSeconds ?? 0,
      seed_first_intent: input.seedFirstIntent ?? false,
      plan_heartbeat_seconds: input.planHeartbeatSeconds ?? 0, // 0 = バックエンドがデフォルトの 600(10min)に正規化
      coverage_enabled: input.coverageEnabled ?? true, // デフォルトはオン;false=アセットカバレッジ機能をオフ
    }),

  taskCategories: () => get<{ categories: TaskCategory[] }>("/task-categories").then((r) => arr(r.categories)),

  updateTask: (id: string, input: { name?: string; pinned?: boolean }) => patch<Task>(`/tasks/${id}`, input),

  renameTask: (id: string, name: string) => patch<Task>(`/tasks/${id}`, { name }),

  pinTask: (id: string, pinned: boolean) => patch<Task>(`/tasks/${id}`, { pinned }),

  createTaskCategory: (name: string) => post<TaskCategory>("/task-categories", { name }),

  renameTaskCategory: (id: number, name: string) => patch<TaskCategory>(`/task-categories/${id}`, { name }),

  deleteTaskCategory: (id: number) => del<{ deleted: number }>(`/task-categories/${id}`),

  updateTaskCategory: (taskId: string, categoryId?: number) =>
    patch<Task>(`/tasks/${taskId}/category`, { category_id: categoryId ?? null }),

  // categoryId 省略/undefined = カテゴリから外す（バックエンドは null を受け取る）
  updateTasksCategory: (taskIds: string[], categoryId?: number) =>
    post<{ items: BatchCategoryItem[]; category: TaskCategory | null }>("/tasks/category/batch", {
      task_ids: taskIds,
      category_id: categoryId ?? null,
    }),

  taskTemplates: () => get<{ templates: TaskTemplate[] }>("/task-templates").then((r) => arr(r.templates)),

  createTaskTemplate: (input: Pick<TaskTemplate, "name" | "description" | "goal">) =>
    post<TaskTemplate>("/task-templates", input),

  updateTaskTemplate: (id: number, input: Partial<Pick<TaskTemplate, "name" | "description" | "goal">>) =>
    patch<TaskTemplate>(`/task-templates/${id}`, input),

  deleteTaskTemplate: (id: number) => del<{ deleted: number }>(`/task-templates/${id}`),

  updateTaskLLMProfiles: (id: string, llmProfileIds: number[], activeLLMProfileId?: number) =>
    put<{
      id: string;
      llm_profile_ids: number[];
      active_llm_profile_id?: number;
      llm_failover_state: string;
      reopened_intents: number;
      switch_event?: Activity;
    }>(`/tasks/${id}/llm`, {
      llm_profile_ids: llmProfileIds,
      active_llm_profile_id: activeLLMProfileId ?? null,
    }),

  deleteTask: (id: string, options: DeleteTaskOptions) => del<DeleteTaskResult>(`/tasks/${id}`, options),

  controlTask: (id: string, action: "pause" | "resume") =>
    post<{ id: string; paused: boolean; queued: boolean; status: string }>(`/tasks/${id}/control`, { action }),

  controlTasksBatch: (taskIds: string[], action: "pause" | "resume") =>
    post<{ items: BatchControlItem[] }>("/tasks/control/batch", { task_ids: taskIds, action }),

  taskArchives: (input?: { page?: number; size?: number; q?: string; state?: string }) => {
    const query = new URLSearchParams();
    query.set("page", String(input?.page ?? 1));
    query.set("size", String(input?.size ?? 20));
    if (input?.q) query.set("q", input.q);
    if (input?.state) query.set("state", input.state);
    return get<TaskArchivePage>(`/task-archives?${query.toString()}`).then((response) => ({
      ...response,
      items: arr(response.items),
    }));
  },

  taskArchive: (id: number) => get<TaskArchive>(`/task-archives/${id}`),

  archiveTask: (id: string) => post<TaskArchive>(`/tasks/${id}/archive`),

  archiveTasks: (taskIds: string[]) =>
    post<{ items: ArchiveBatchItem[] }>("/tasks/archive/batch", { task_ids: taskIds }),

  restoreTaskArchive: (id: number) => post<TaskArchive>(`/task-archives/${id}/restore`),

  restoreTaskArchives: (archiveIds: number[]) =>
    post<{ items: ArchiveBatchItem[] }>("/task-archives/restore/batch", { archive_ids: archiveIds }),

  deleteTaskArchive: (id: number) => del<TaskArchive>(`/task-archives/${id}`),

  deleteTaskArchives: (archiveIds: number[]) =>
    post<{ items: ArchiveBatchItem[] }>("/task-archives/delete/batch", { archive_ids: archiveIds }),

  controlIntent: (
    taskId: string,
    intentId: string,
    action: "pause" | "resume" | "cancel",
    reason?: string,
    // cancel 専用：soft(デフォルト、仮削除、意図を deleted にして削除理由を記録)| hard(実削除、独占の子孫を連鎖削除)。
    mode?: "soft" | "hard",
  ) =>
    post<{
      id: number;
      state: "paused" | "open" | "deleted" | "";
      deleted?: { intents: number; facts: number; findings: number; activities: number };
    }>(`/tasks/${taskId}/intents/${intentId}/control`, { action, reason: reason ?? "", mode: mode ?? "soft" }),

  sendWorkerMessage: (taskId: string, intentId: string, message: string, requestId: string) =>
    post<{
      id: number;
      state: "running";
      accepted: true;
      request_id: string;
    }>(`/tasks/${taskId}/intents/${intentId}/messages`, { message, request_id: requestId }),

  taskLLMResolution: (id: string) => get<TaskLLMResolutions>(`/tasks/${id}/llm/resolution`),

  // うまく走らなかった意図(blocked/exhausted/stopped)を再実行する：open に戻すと worker が再び引き取り、最初から走らせる。
  rerunIntent: (taskId: string, intentId: string) =>
    post<{ id: string; reopened: number }>(`/tasks/${taskId}/intents/${intentId}/rerun`),

  // 本タスクの blocked 意図をまとめて再実行（ネットワーク/LLM の断で blocked が複数出た時に一括で再試行）。
  rerunBlocked: (taskId: string) => post<{ id: string; reopened: number }>(`/tasks/${taskId}/intents/rerun-blocked`),

  setActive: (id: string) => post<{ active: string }>("/active", { id }),
};
