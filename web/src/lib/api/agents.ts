import type { Agent, AgentDetail } from "@/lib/types/agents";

import { arr, del, get, patch, post } from "./transport";

export const agentsApi = {
  // ---- agents ----
  agents: () => get<{ agents: Agent[] }>("/agents").then((r) => arr(r.agents)),

  getAgent: (key: string) => get<AgentDetail>(`/agents/${key}`),

  createAgent: (key: string, name: string, description = "") => post<Agent>("/agents", { key, name, description }),

  updateAgent: (key: string, name: string, description = "") =>
    patch<{ ok: boolean }>(`/agents/${key}`, { name, description }),

  deleteAgent: (key: string) => del<{ deleted: string }>(`/agents/${key}`),
};
