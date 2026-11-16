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

const plannerWrapUpDefault = "你本轮规划的步数即将用尽——注意只是【这一轮】结束,系统之后仍会随态势变化再次唤醒你继续规划,并非任务终止,你无需在此收束整个规划。请把本轮已经想清楚的结论落地、别让这一轮白跑,但也【不要为了收尾硬凑意图】(本轮 0 个意图仍是完全正常的结果)：(1) 若已判断出【当前就该派发】的探索方向,用一次 add_intent 批量提交(想好的别憋着不发);(2) 对已被某发现/事实证明达成的目标,调 prove_goal 标记 met(别漏判);(3) 若识别出需要分步的串行利用链,用 TodoWrite 记下,便于下次唤醒接着派。做完直接结束本轮,无需输出总结文本。"

const mainAgentWrapUpDefault = "你的步数即将用尽,本次交互就要结束。不要再发起新的探索/操作。请**单独用一句话纯文本**向用户总结当前进展、关键结论,以及建议的下一步。"

const genericWrapUpDefault = "你即将因预算耗尽被终止。请先把已完成但未落库的结果写回,再**单独用一句话纯文本**总结你做了什么、得到哪些关键结论(这句会作为本次运行的结果展示)。"

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
// まとめる」という既存の分担と一致する——書き戻しの要求は本文側（DB で編集可能）にあり、
// このセルフチェックは末尾に付加されてコード固定なので、管理画面で本文を変えても外れない。
// 付加するのは worker のみ：4 つの問いはすべて「本意図」を前提とし、planner/mainagent には
// 単一の意図が無い。
const wrapupSelfCheck = "\n\n结束前对照自检：① 本意图的结论已 record_fact？② 判据是否真实可复现（还是单次/间接证据）？③ 当前是「被墙」（同质响应/统一拦截）还是「此路不通」（原理不通）？④ 下一步建议换什么维度（参数/信道/漏洞类/入口）？把自检结论并入最后那句总结。"

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

// ---------- タスク単位タイムアウトの終了処理プロンプト（docs/任务级超时与收尾设计.md を参照）----------
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

const workerTaskTimeoutDefault = "**整个任务已到达超时上限，即将结束**（不是你这次 run 的预算，是整场探索到点了）。这是最后机会：(1) 把你已识别但还没写回的内容【全部】落库——新资产 insert_assets、探索结论/事实 record_fact、确认漏洞 report_finding；(2) 不要再启动任何新命令/探测；(3) **最后单独用一句话纯文本**总结你在本意图上的关键结论。"

const plannerTaskTimeoutDefault = "**整个任务已到达超时上限，即将结束**（不是本轮，是整个任务终止）。请基于当前【全部】事实与发现，做最后一次目标判定：对已被证据证明达成的目标调 prove_goal 标记 met（别漏判）。**不要再生成任何新意图**（此时派意图也不会再被执行）。判定完即收束，无需输出总结文本。"

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
