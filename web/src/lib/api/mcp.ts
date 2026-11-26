import type { MCPServer, MCPTool } from "@/lib/types/mcp";

import { arr, del, get, post } from "./transport";

export const mcpApi = {
  // ---- mcp ----
  mcpServers: () => get<{ servers: MCPServer[] }>("/mcp").then((r) => arr(r.servers)),

  saveMcpServer: (m: Partial<MCPServer>) => post<{ id: number }>("/mcp", m),

  deleteMcpServer: (id: number) => del<{ deleted: number }>(`/mcp/${id}`),

  mcpTools: (id: number) => get<{ tools: MCPTool[] }>(`/mcp/${id}/tools`).then((r) => arr(r.tools)),

  refreshMcpServer: (id: number) => post<{ tools: MCPTool[] }>(`/mcp/${id}/refresh`, {}).then((r) => arr(r.tools)),
};
