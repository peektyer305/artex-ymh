// Domain types. Cross-domain dependencies are type-only.

// ---- Tools (組み込みツールカタログ) ----
// key + handler live in Go; only these fields are page-editable. system tools lock
// the key and the parameter *structure* (name/type/required) — the per-param
// description/default and the agent binding are what move.
export interface Tool {
  key: string;
  system: boolean;
  description: string;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  schema: Record<string, any>; // full JSON-Schema (object with properties)
  agents: string[]; // bound agent keys
  enabled: boolean;
  kind?: "builtin" | "shell" | "command" | "script" | "http"; // カスタムツールの種類
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  exec?: Record<string, any>; // カスタムツールの実行仕様(kind!=builtin)
  deferred?: boolean; // schema の遅延(SearchExtraTools/ExecuteExtraTool)
  calls?: number; // persistent runtime invocation count (older APIs may omit it)
}
