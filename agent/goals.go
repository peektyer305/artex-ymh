package agent

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/agentcore"
	"github.com/Autumn-27/norma/llm"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// goalsDefaultTmpl is the built-in EDITABLE body (段 [A]) of the goals-decomposer
// prompt, seeded into agent_prompts. No template vars are used today.
const goalsDefaultTmpl = `あなたはペネトレーションテストの目標分解器。あなたの責務は、ユーザー入力から**最終的に達成すべき結果**を識別することであり、攻撃ステップを計画することではない。

**第一歩(目標を分解する前に先に行う)：操作制約の抽出**
「タスク目標 / タスク説明」から、操作員による【何ができて、何の操作ができないか】の明確な規定を識別し、set_constraints を呼んで逐条登録する(説明・目標に操作制約が含まれない場合は操作制約の抽出を行わなくてよい)：
- type=deny：禁止される操作(例：「ポートをスキャンしない」「本番環境に書き込み/削除操作をしない」「ブルートフォース禁止」「あるサブドメインに触れない」)。
- type=allow：明確に許可/限定される操作範囲(例：「パッシブ偵察のみ許可」「特定のドメインのみ対象」)。
- 制約 ≠ 目標、かつ ≠ 攻撃ステップ。操作行為の境界についての規定である。
- **制約は【自己完結で、具体的な対象を固定して書く】ことが必須**：「現在の対象/現在のポート/現在のIP/現在のドメイン/当サイト」といった**指示語**を、タスク目標/説明にある**具体的な値**に置き換える。制約は実行段階のプロンプトに単独で注入されるため、文脈を離れると指示語は何を指すか判断できない。
  例：対象が https://abc.example.net なら、「abc.example.net のテストのみ許可」と書き、「現在の対象のテストのみ許可」としない。「対象のポート 443 のみテストし、他のポートはスキャンしない」と書き、「現在のポートのみテスト」としない。原文が「現在の対象」としか言っていなくても、対象アドレスが既に明らかならそのアドレスを入れる。
- **目標/説明で【明示的または強調された】制約だけを登録する。捏造は厳禁**。型が分からないときは deny を使う(より保守的)。
- 目標/説明に操作制約が本当に何もなければ、set_constraints を呼び出しては**ならない**。
制約の登録を終えたら(あれば)、次に下記の目標分解を行う。

**目標 = 最終的に納品可能/検証可能な結果**

**目標ではない内容(サブ目標として列挙するのは禁止)**：
- 情報収集、偵察、エンドポイントスキャン
- 脆弱性の分析と検証過程
- 攻撃ステップ、悪用手段
- 結果の検証ステップ

**分解の原則**：
- ユーザーが記述した最終目標が 1 つだけ → 1 つ出力する
- 複数の**相互に独立した**最終納品物が存在する → それぞれ列挙する
- 明確な脆弱性クラスに対応できるものは vulnclass を付ける。情報収集/ビジネスロジック系の目標は空にする
- ユーザーが言及していない目標の捏造は厳禁

set_goals を呼び出して結果を提出する。`

// goalsScopeTail is the code-owned tail appended after the editable goals body
// WHEN an asset store + task context are available. It teaches the decomposer to
// also lift the explicit asset scope out of the goal/description and register it
// via add_task_scope. Kept in code (not the DB-editable body) so it always applies
// on released DBs and can't be edited away — same pattern as the trafficTool tail.
const goalsScopeTail = `

**追加の責務：テスト対象アセット範囲の登録**
目標の分解に加えて、「タスク目標 / タスク説明」から**明示されたテスト対象アセット範囲**を識別し、add_task_scope を呼んで登録する(このタスクの認可境界であり、アセットテストのカバー率の分母でもある)。**最小範囲の原則：ユーザーが明確に指定したその 1 対象だけを登録し、勝手に広げない。**
- 対象が URL またはホスト名付きのアドレス(例：https://xxx.example.com/path、app.example.com)なら、その**完全なホスト名**を取り、kind=subdomain、value=完全なホスト名とする。
  例：対象 https://a1b2c3.lab.example.net/path → kind=subdomain、value=a1b2c3.lab.example.net(**ではなく** example.net)。
  サブドメイン付きのホスト名をルートドメインに縮めるのは**厳禁**。xxx.example.com を見て example.com 全体を登録すると、範囲をユーザーの対象の外にまで広げ、最小範囲の原則に反する。
- ユーザーが与えたのが**裸のルートドメインで、サブドメインを一切含まない**(例：直接 example.com と書かれている)か、「サイト全体 / すべてのサブドメイン / 全ドメイン」と明言している場合だけ、kind=root_domain、value=example.com を使う。
- 純粋な IP またはネットワークセグメント → kind=ip / cidr、value=IP または CIDR。
- 会社範囲(company)は登録**しない**。タスクができたばかりで、アセットシステムには通常まだその会社がなく登録できない。会社レベルの範囲は後続の plan 段階に委ねる。
その他の規則：
- **目標/説明に明記された**範囲だけを登録する。言及されていないドメイン/IP の捏造や推測は厳禁。
- reason には根拠がどの文から来たかを簡潔に記し、監査しやすくする。
- 目標/説明に明確なアセット範囲が何もなければ、add_task_scope を呼び出しては**ならない**。
先に add_task_scope で範囲を登録し(あれば)、次に set_goals を呼び出して目標を提出する。`

// GoalSpec is one decomposed objective.
type GoalSpec struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass,omitempty"`
}

// DecomposeGoals asks the LLM to break a pentest task goal into discrete,
// independently-verifiable objectives (each becomes a goal node). Returns nil if
// no provider is configured or the call yields nothing — the caller then falls
// back to a rule-based split so goal nodes always exist.
//
// prov is supplied by the caller (rather than built here from a Config) so goal
// decomposition rides the SAME provider instance as the rest of the engine — it
// shares the rate limiter, gets recorded by llmrec, and participates in LLM
// failover instead of quietly bypassing all three.
//
// desc is the task's free-text description (背景：ターゲット範囲/flag 数/交戦説明など).
// It is fed alongside the goal so the decomposer no longer splits blind — the
// prompt still forbids inventing anything the two texts don't state.
//
// emit, when non-nil, receives every LLM step (thinking/tool_use/result) with
// Worker="planner" so the round-0 goal-decomposition activity is visible in the UI.
//
// as + taskID, when non-nil/positive, wire the add_task_scope tool so the
// decomposer can register the explicit asset scope it extracts from the goal.
//
// ts is the task's exploration store: set_goals writes the decomposed goal nodes
// straight into it (the same managed tool the main agent uses to add goals at
// runtime). The returned specs are read back from the store so callers can emit
// per-goal activity and detect the "LLM produced nothing" case for their fallback.
func DecomposeGoals(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	return DecomposeGoalsWithProvider(ctx, prov, dataDir, goalText, desc, as, ts, taskID, false, 0, emit)
}

// DecomposeGoalsWithProvider is the task-runtime variant used when a task has an
// ordered provider chain. It preserves the same tools and write behavior while
// letting the caller own provider selection/failover. maxTokens is the profile's
// per-reply output cap (0 = send none).
func DecomposeGoalsWithProvider(ctx context.Context, prov llm.Provider, dataDir, goalText, desc string, as *db.AssetStore, ts *db.ExplorationStore, taskID int64, nonStreaming bool, maxTokens int, emit func(db.Activity)) []GoalSpec {
	if prov == nil {
		return nil
	}
	// worker="goals" tags the goal nodes' provenance; ts/taskID let set_goals link
	// each goal under the task root. This is the catalog's real set_goals tool, so a
	// web-edited description/schema on it applies here too.
	tsx := &ToolSet{as: as, ts: ts, taskID: taskID, worker: "goals"}
	// Description rides in the user message (same channel as the goal), NOT via the
	// {{.EngagementDescription}} template var — else a prompt that references the var
	// would inject the description twice. System prompt stays pure static instructions.
	sys := renderSystem("goals", goalsDefaultTmpl, GoalsVars{DataDir: dataDir, Now: nowStr()})
	// set_constraints は常に利用可能(asset store に依存しない)：本文に「先に操作制約を抽出し、
	// その後に目標を分割する」手順が含まれる(agent の編集ページで文言を変えられる)。ここでは
	// ツールを接続するだけでよい。
	tools := []actool.CoreTool{tsx.setGoals(), tsx.setConstraints()}
	// Wire add_task_scope only when we have a real asset store + task to write to.
	// The scope-extraction tail is appended in lockstep so the prompt never asks for
	// a tool that isn't present.
	if as != nil && taskID > 0 {
		tools = append(tools, tsx.addTaskScope())
		sys += goalsScopeTail
	}
	userMsg := "タスク目標：\n" + goalText
	if d := strings.TrimSpace(desc); d != "" {
		userMsg += "\n\nタスク説明（背景情報。対象範囲/flag 数/交戦の説明を含む場合がある。参考のみ。その中で言及されていない内容を捏造しない）：\n" + d
	}
	// Use captureRun so every LLM step is emitted as an activity record (visible in
	// the plan tab under the round-0 marker). Falls back gracefully when emit is nil.
	captureEmit := func(r db.Activity) {
		if emit != nil {
			r.Worker = "planner"
			emit(r)
		}
	}
	captureRun(ctx, agentcore.Options{
		Provider:               prov,
		SystemPrompt:           []string{sys},
		Tools:                  tools,
		PermissionMode:         acperm.ModeBypass,
		DisableBackgroundTasks: true,
		// 3 ステップ(制約の抽出 → 範囲の登録 → 目標の分割)にそれぞれ 1 回のツール呼び出しが要る。
		// ターンを十分に与え、終了前に set_goals を呼び忘れないようにする。
		MaxTurns:     8,
		NonStreaming: nonStreaming, // この profile で非ストリーミングを選んだ場合は Provider.Complete を通る
		MaxTokens:    maxTokens,    // 0 = 上限を送らず、サーバー側のデフォルト値に任せる
	}, userMsg, captureEmit)
	// set_goals persisted the goals directly; read them back so the caller sees what
	// was written (empty slice ⇒ the LLM produced nothing ⇒ caller falls back).
	if ts == nil {
		return nil
	}
	nodes, _ := ts.ListByKind(db.KindGoal, 10000)
	var out []GoalSpec
	for _, n := range nodes {
		var p struct {
			Text      string `json:"text"`
			VulnClass string `json:"vulnclass"`
		}
		_ = json.Unmarshal(n.Payload, &p)
		if strings.TrimSpace(p.Text) != "" {
			out = append(out, GoalSpec{Text: p.Text, VulnClass: p.VulnClass})
		}
	}
	return out
}
