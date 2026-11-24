package agent

import (
	"strings"

	"github.com/Autumn-27/norma/harness"
)

// 終了処理プロンプト（wrap-up / settlement prompt）:agent が【ステップ枯渇（MaxTurns）】または
// 【タイムアウト（run_seconds/MaxDuration）】で終了させられたとき、SDK の settlement 段階が
// このプロンプトを注入し、agent に判明済みで未書き戻しの内容を DB へ書き込ませ、その上で
// 一言まとめを出力させることで、中途半端な終わり方を避ける。
//
// 各 agent の終了処理プロンプトは管理画面から必要に応じて上書きできる（agents.wrapup_prompt に
// 保存）。空ならここでの組み込み既定を使う。編集できるのは【プロンプト本文】だけで、
// どのツールを無効にするか、終了処理自体に何ターン与えるかはコード固定のポリシー。

// WrapupOverride, if set, returns the stored wrap-up prompt for an agent key and
// whether a non-empty one exists. Wired by the server to the agents table (like
// PromptOverride for system prompts). nil / empty → the built-in default is used.
var WrapupOverride func(agentKey string) (string, bool)

// WrapupMaxTurnsOverride, if set, returns the admin-configured turn budget for the
// wrap-up phase of an agent and whether a positive one exists. Wired to the agents
// table. nil / ≤0 → the built-in per-agent default (wrapupTurnDefaults) is used.
var WrapupMaxTurnsOverride func(agentKey string) (int, bool)

// 組み込みの既定終了処理プロンプト。agent key で引く。worker は従来ハードコードだった
// settleWrapUpPrompt（worker.go に定義）を再利用し、planner/mainagent はそれぞれ 1 版を持つ。
// 該当が無い場合（カスタム agent）は汎用のフォールバックを使う。
var wrapupDefaults = map[string]string{
	"worker":    settleWrapUpPrompt,
	"planner":   plannerWrapUpDefault,
	"mainagent": mainAgentWrapUpDefault,
}

// wrapupTurnDefaults: 各 agent の終了処理段階【自身】のターン予算の組み込み既定（管理画面から
// 0 超で上書き可能）。いずれも 10 ターンとし、終了処理段階で DB へ書き込むのに十分なステップを
// 確保する。該当が無ければ genericWrapupTurns を使う。
var wrapupTurnDefaults = map[string]int{
	"worker":    10,
	"planner":   10,
	"mainagent": 10,
}

const genericWrapupTurns = 10

const plannerWrapUpDefault = "今ラウンドの計画ステップ数が間もなく尽きます。これは【このラウンド】が終わるだけであり、システムは今後も状況の変化に応じて再度あなたを起こして計画を続けさせます。タスクの終了ではないため、ここで計画全体を収束させる必要はありません。このラウンドで考えがまとまった結論を確定させ、このラウンドを無駄にしないでください。ただし【収束のために意図を無理にひねり出さない】でください(このラウンドの意図が 0 個でも全く正常な結果です)：(1) 【今まさに割り当てるべき】探索方向を判断できたなら、add_intent で一括提出してください(考えがまとまったものをため込んで出さない);(2) 何らかの発見/事実によって達成が証明された目標は、prove_goal を呼んで met とマークしてください(見落としがないように);(3) 段階的に進める必要のある直列の利用チェーンを特定したら、TodoWrite に記録し、次回起きたときに続きを割り当てられるようにしてください。終えたらそのままこのラウンドを終了し、まとめのテキストを出力する必要はありません。"

const mainAgentWrapUpDefault = "ステップ数が間もなく尽き、今回の対話は終了します。新しい探索/操作を開始しないでください。**一言のプレーンテキストだけ**で、現在の進捗、重要な結論、推奨する次の手順をユーザーへまとめてください。"

const genericWrapUpDefault = "予算を使い果たしたため間もなく終了します。まず、完了しているがまだデータベースへ書き込んでいない結果を書き戻し、それから**一言のプレーンテキストだけ**で、何をしてどのような重要な結論を得たかをまとめてください(この一言が今回の実行結果として表示されます)。"

// WrapupDefault returns the built-in default wrap-up prompt for an agent key —
// used by the admin UI as the "restore default" value and empty-field placeholder.
func WrapupDefault(agentKey string) string {
	if d, ok := wrapupDefaults[agentKey]; ok {
		return d
	}
	return genericWrapUpDefault
}

// WrapupTurnsDefault returns the built-in wrap-up turn budget for an agent key —
// used by the admin UI as the "0 = default N" hint.
func WrapupTurnsDefault(agentKey string) int {
	if n, ok := wrapupTurnDefaults[agentKey]; ok {
		return n
	}
	return genericWrapupTurns
}

// resolveWrapup returns the effective wrap-up prompt: the DB override (if set and
// non-empty) over the built-in default.
func resolveWrapup(agentKey string) string {
	if WrapupOverride != nil {
		if t, ok := WrapupOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return WrapupDefault(agentKey)
}

// resolveWrapupTurns returns the effective wrap-up turn budget: a positive DB
// override over the built-in per-agent default.
func resolveWrapupTurns(agentKey string) int {
	if WrapupMaxTurnsOverride != nil {
		if v, ok := WrapupMaxTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return WrapupTurnsDefault(agentKey)
}

// wrapupSelfCheck はコード固定の終了処理セルフチェック段（chains L1、docs/architecture/chains-integration.md §4）:
// 「問い返し」を知識からワークフロー内の強制アクションへ変えるもの。「先に書き戻し、その後に
// まとめる」という既存の分担と一致する。書き戻しの要求は本文側（DB で編集可能）にあり、
// このセルフチェックは末尾に付加されてコード固定なので、管理画面で本文を変えても外れない。
// 付加するのは worker のみ：4 つの問いはすべて「本意図」を前提とし、planner/mainagent には
// 単一の意図が無い。
const wrapupSelfCheck = "\n\n終了前の自己点検：① この意図の結論は record_fact 済みか？② 判定の根拠は本当に再現可能か（それとも単発/間接的な証拠か）？③ 現状は「壁に阻まれている」（同質の応答/一律の遮断）のか「この道は通じない」（原理的に通じない）のか？④ 次はどの次元（パラメータ/チャネル/脆弱性の種類/入口）に変えることを提案するか？自己点検の結論を最後のまとめの一文に含める。"

// withWrapupSelfCheck は worker の終了処理プロンプトにコード固定のセルフチェック段を追加する。他の agent はそのまま返す。
func withWrapupSelfCheck(agentKey, prompt string) string {
	if agentKey != "worker" {
		return prompt
	}
	return prompt + wrapupSelfCheck
}

// wrapupSettlement builds the settlement config for an agent's run. Prompt and the
// turn budget are admin-editable per agent; disabled tools are code-owned policy so
// a user can't edit away the "stop probing" guardrail. Resolved fresh each run
// (reads DB live), so edits apply on the next run without a restart.
func wrapupSettlement(agentKey string, disabledTools []string) *harness.Settlement {
	return &harness.Settlement{
		Prompt:        withWrapupSelfCheck(agentKey, resolveWrapup(agentKey)),
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
}

// ---------- タスク単位タイムアウトの終了処理プロンプト（docs/タスクレベルタイムアウトと終了処理設計.md を参照）----------
//
// per-run の終了処理プロンプトとは【別系統】:per-run は「この run の予算が尽きた」、タスク
// タイムアウトは「タスク全体が期限に達し、間もなく終わる」。意味はしばしば逆になる（特に
// planner:per-run は「止まらず計画を続けよ」、タスクタイムアウトは「期限なので計画を止め
// 最後の判定を行え」）。設定するのは worker/planner のみ。

// WrapupTaskTimeoutOverride / …TurnsOverride：タスクタイムアウト終了処理プロンプトとターン数の
// DB 上書き（agents.task_timeout_wrapup_prompt / _max_turns に配線。worker/planner のみ）。
var (
	WrapupTaskTimeoutOverride      func(agentKey string) (string, bool)
	WrapupTaskTimeoutTurnsOverride func(agentKey string) (int, bool)
)

var taskTimeoutWrapupDefaults = map[string]string{
	"worker":  workerTaskTimeoutDefault,
	"planner": plannerTaskTimeoutDefault,
}

const workerTaskTimeoutDefault = "**タスク全体がタイムアウト上限に達し、間もなく終了します**（今回の run の予算ではなく、探索全体の時間切れです）。これが最後の機会です：(1) 特定済みだがまだ書き戻していない内容を【すべて】データベースへ書き込んでください。新規アセットは insert_assets、探索の結論/事実は record_fact、確認済み脆弱性は report_finding を使います；(2) 新しいコマンド/探索を開始しないでください；(3) **最後に一言のプレーンテキストだけ**で、この意図での重要な結論をまとめてください。"

const plannerTaskTimeoutDefault = "**タスク全体がタイムアウト上限に達し、間もなく終了します**（このラウンドではなく、タスク全体の終了です）。現在の【すべての】事実と発見に基づいて、最後の目標判定を行ってください：証拠によって達成が証明された目標は prove_goal を呼んで met とマークしてください（見落としがないように）。**新しい意図を一切生成しないでください**（この時点で意図を割り当てても実行されません）。判定が済んだら収束し、まとめのテキストを出力する必要はありません。"

// TaskTimeoutWrapupDefault はある agent のタスクタイムアウト時の組み込み既定終了処理プロンプトを
// 返す（管理画面のプレースホルダ/既定に戻す用）。
func TaskTimeoutWrapupDefault(agentKey string) string {
	return taskTimeoutWrapupDefaults[agentKey] // 未設定(mainagent/chat)なら空文字を返す
}

// resolveTaskTimeoutWrapup:DB の上書き（非空）> 組み込み既定。空文字はその agent にタスク
// タイムアウト用プロンプトが無いこと（worker/planner 以外）を意味し、呼び出し側は per-run 用へ戻す。
func resolveTaskTimeoutWrapup(agentKey string) string {
	if WrapupTaskTimeoutOverride != nil {
		if t, ok := WrapupTaskTimeoutOverride(agentKey); ok && strings.TrimSpace(t) != "" {
			return t
		}
	}
	return TaskTimeoutWrapupDefault(agentKey)
}

func resolveTaskTimeoutTurns(agentKey string) int {
	if WrapupTaskTimeoutTurnsOverride != nil {
		if v, ok := WrapupTaskTimeoutTurnsOverride(agentKey); ok && v > 0 {
			return v
		}
	}
	return resolveWrapupTurns(agentKey) // 既定では per-run のターン数を流用する
}

// wrapupSettlementForTask builds settlement for a worker/planner run that is aware
// of the task deadline. See §5 of the design doc:
//   - clamped=true  → this run is clamped by the task deadline: wrap-up on Timeout means
//     the task hit its deadline → task-timeout prompt; wrap-up on MaxTurns means the steps
//     ran out inside the clamped window while the task still has minutes left → per-run prompt.
//   - clamped=false → the task is still far off: both reasons use the per-run prompt
//     (i.e. this degrades to wrapupSettlement).
//
// PromptByReason handed to harness picks by the ACTUAL reason at wrap-up time; without it
// the mapping would mismatch.
func wrapupSettlementForTask(agentKey string, disabledTools []string, clamped bool) *harness.Settlement {
	perRun := withWrapupSelfCheck(agentKey, resolveWrapup(agentKey))
	st := &harness.Settlement{
		Prompt:        perRun, // フォールバック（非 clamped 時は両 reason で使う値でもある）
		DisabledTools: disabledTools,
		MaxTurns:      resolveWrapupTurns(agentKey),
	}
	if clamped {
		if tt := resolveTaskTimeoutWrapup(agentKey); tt != "" {
			st.PromptByReason = map[harness.TerminalReason]string{
				harness.ReasonTimeout:  withWrapupSelfCheck(agentKey, tt), // タスクが期限到達
				harness.ReasonMaxTurns: perRun,                            // ステップが先に尽きたが、タスクにはまだ時間がある
			}
			st.MaxTurns = resolveTaskTimeoutTurns(agentKey)
		}
	}
	return st
}
