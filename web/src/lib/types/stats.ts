// Domain types. Cross-domain dependencies are type-only.

import type { EngineMode, Task } from "./tasks";

// ---- Stats ----
export interface Stats {
  assets: number;
  engine_mode: EngineMode;
  llm_configured: boolean;
  roe_enabled: boolean;
  findings_confirmed: number;
  active_task?: Partial<Task>;
}
