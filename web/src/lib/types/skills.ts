// Domain types. Cross-domain dependencies are type-only.

// ---- Skills ----
// Fields align with the agentskills.io open specification.
// description covers both "what the skill does" and "when to use it".
export interface SkillItem {
  name: string; // unique key = directory name
  description?: string; // required per spec; covers what + when to use
  license?: string; // optional: SPDX identifier or free text
  compatibility?: string; // optional: environment requirements
  mcps?: string[]; // MCP server names this skill unlocks on load
  files: string[]; // files in the skill directory
  // 呼び出し統計（skill_usage 台帳）。一度も呼ばれていない skill：calls=0、last_used は未設定。
  calls: number;
  tasks: number; // これを読み込んだタスク数（chat セッションは含まない）
  usage_agents: string[]; // これを読み込んだ agent key
  last_used?: string;
}

// SkillCall は Skill() 呼び出し 1 回（単一 skill の直近の呼び出し一覧）。
export interface SkillCall {
  ts: string;
  agent_key: string;
  task_id: number; // 0 = タスク以外の場面（チャットセッション）
  session_id: string;
  args_len: number;
}

// MissingSkill は指定されたが存在しない skill。「使いたいが無い」ギャップ。
export interface MissingSkill {
  skill: string;
  calls: number;
  agents: string[];
  last_used?: string;
}
