package guard

// バッチ 5 B3（設計ドキュメント HONEYPOT-DETECTION-DESIGN.md 二-B「自己防護」第 5 条）:tarpit サーキットブレーカー。
// Nepenthes / Cloudflare AI Labyrinth のようなクローラー迷路は無限の偽ページで
// クローラーを拘束し、タスク予算を焼き尽くす。本ファイルは「タスクごと・host ごと」に
// WebFetch の呼び出しを数え、予算超過で一度だけサーキットブレーカーを発動する:
// Hint コールバック経由で探索グラフに hint ノードを書き込み（planner は毎ターン
// graph overview で hints を必ず読む。停滞検知 hint の既存チャネルを参照）、
// この方向への割り当て停止を促す。以降その host への取得は warn で通過させる——
// ハード block はせず、実在する大規模サイトの誤爆を防ぐ。
//
// カウンタはメモリ上（タスクごとに 1 つの Guard。server taskFromPG 参照）。タスク再起動で
// ゼロに戻り、再クロール時に再発火する。コストは許容範囲で、DB には保存しない。

import (
	"encoding/json"
	"log"
	"net/url"
	"strconv"

	"github.com/Autumn-27/norma/hook"
)

// defaultTarpitFetchBudget は host あたりの取得呼び出しのデフォルト予算（settings キー
// tarpit_fetch_budget で設定可能。<=0 または不正な値ならこれにフォールバックする）。
const defaultTarpitFetchBudget = 40

// TarpitConfig は tarpit サーキットブレーカーの組み立て。2 つのコールバックは
// いずれも server が注入する（guard は db/server に逆依存しない）。
type TarpitConfig struct {
	// Budget は現在の予算を返す（settings tarpit_fetch_budget）。nil または <=0 を返す → デフォルト 40。
	Budget func() int
	// Hint は発動時（host ごとに 1 回だけ）のコールバック。server が探索グラフの hint ノード書き込みとして組み立てる。
	// nil = guard の監査/ログのみ記録。
	Hint func(host string, count int)
}

// tarpitState は Guard ごと（=タスクごと）の tarpit カウント状態。Guard.mu で直列化する。
type tarpitState struct {
	cfg    TarpitConfig
	counts map[string]int
	fired  map[string]bool // host ごとに 1 回だけ発動
}

// SetTarpit は tarpit サーキットブレーカーを組み立てる。ゼロ値/nil コールバックの再設定は
// 単なる機能低下（hint を書かない）で、エラーにはしない。
func (g *Guard) SetTarpit(cfg TarpitConfig) {
	g.mu.Lock()
	g.tarpit = &tarpitState{cfg: cfg, counts: map[string]int{}, fired: map[string]bool{}}
	g.mu.Unlock()
}

func (c TarpitConfig) budget() int {
	if c.Budget == nil {
		return defaultTarpitFetchBudget
	}
	if b := c.Budget(); b > 0 {
		return b
	}
	return defaultTarpitFetchBudget
}

// checkTarpit は同一 host に対する WebFetch の取得呼び出しを数える。予算超過時に一度だけ発動し（hint を書く）、
// 以降その host へは一律 warn で通過させる（監査のみ記録し、block しない）。WebFetch 以外 / host を
// 解析できない呼び出しはそのまま通過させる。
func (g *Guard) checkTarpit(ev hook.Event) {
	if ev.ToolName != "WebFetch" {
		return
	}
	g.mu.Lock()
	st := g.tarpit
	g.mu.Unlock()
	if st == nil {
		return
	}
	var in struct {
		URL string `json:"url"`
	}
	if json.Unmarshal(ev.Input, &in) != nil || in.URL == "" {
		return
	}
	u, err := url.Parse(in.URL)
	if err != nil || u.Hostname() == "" {
		return
	}
	host := u.Hostname()

	g.mu.Lock()
	st.counts[host]++
	count := st.counts[host]
	budget := st.cfg.budget()
	over := count > budget
	first := over && !st.fired[host]
	if first {
		st.fired[host] = true
	}
	g.mu.Unlock()

	if !over {
		return
	}
	if first {
		// 発動:監査を記録し、コールバックで hint（host/回数）を書き、planner にこの方向への割り当て停止を促す。
		g.record(ev.ToolName, "allow", tarpitMessage(host, count, budget), "")
		log.Printf("[guard][tarpit] host %s の取得呼び出しが %d 回で予算 %d を超過しました。発動を警告します（warn で通過）", host, count, budget)
		if st.cfg.Hint != nil {
			st.cfg.Hint(host, count)
		}
		return
	}
	// 発動済みの host は warn で通過させる——ハード block はせず（実在する大規模サイトの誤爆防止）、
	// 毎回軽量な監査を 1 行残し、後から「発動後も取得している」動作を復元できるようにする。
	g.record(ev.ToolName, "allow", "tarpit 熔断后仍抓取 "+host, "")
}

// tarpitMessage は発動監査/hint で共用する文言:疑わしい tarpit 迷路（host、回数）。
func tarpitMessage(host string, count, budget int) string {
	return "疑似 tarpit 迷宫(" + host + ",抓取 " + strconv.Itoa(count) + " 次,预算 " + strconv.Itoa(budget) + ")"
}
