// Domain types. Cross-domain dependencies are type-only.

// ---- Command recording (Bash execution history) ----
export interface CommandRecord {
  id: number;
  exploration_id: number;
  worker: string;
  tool: string;
  command: string; // raw tool input (JSON)
  output: string;
  is_error: boolean;
  created_at: string;
}

// ツール単位の呼び出し統計（/commands/stats）；errors はうち失敗した回数。
export interface ToolStat {
  tool: string;
  total: number;
  errors: number;
}
