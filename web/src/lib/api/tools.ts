import type { Tool } from "@/lib/types/tools";

import { arr, del, get, post, put } from "./transport";

export const toolsApi = {
  // ---- tools (組み込みツールカタログ) ----
  tools: () => get<{ tools: Tool[] }>("/tools").then((r) => arr(r.tools)),

  saveTool: (key: string, patch: Pick<Tool, "description" | "schema" | "agents" | "enabled">) =>
    put<{ ok: boolean }>(`/tools/${key}`, patch),

  resetTool: (key: string) => post<{ ok: boolean }>(`/tools/${key}/reset`, {}),

  // custom tools (カスタムツール)
  createCustomTool: (
    t: Pick<Tool, "key" | "description" | "schema" | "agents" | "enabled" | "kind" | "exec" | "deferred">,
  ) => post<{ key: string }>("/tools/custom", t),

  updateCustomTool: (
    key: string,
    t: Pick<Tool, "description" | "schema" | "agents" | "enabled" | "kind" | "exec" | "deferred">,
  ) => put<{ ok: boolean }>(`/tools/custom/${key}`, t),

  deleteCustomTool: (key: string) => del<{ deleted: string }>(`/tools/custom/${key}`),

  testCustomTool: (body: { kind: string; exec: Record<string, unknown>; params: Record<string, unknown> }) =>
    post<{ output: string; is_error: boolean }>("/tools/custom/test", body),

  detectPython: () => post<{ python_interpreter: string }>("/settings/python/detect", {}),
};
