package server

import (
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
)

// 再試行ポリシーのサーバ側解決（LLM 再試行の設計メモを参照）。5 層のうち：
//   - 接続 / 空応答 / 同一 provider の安全ウィンドウ は「エンドポイントに付随する」もので、
//     LLM 設定ごとにグローバル既定を上書きできる（profile の項目が空ならグローバルを継承し、
//     グローバルも未設定なら組み込み既定を使う）;
//   - サーキットブレーカー / 意図の再実行 はプロセス単位で、グローバルに 1 つだけ。
//
// グローバルポリシーは settings の 1 行を 1 回読むだけで、呼び出し箇所はいずれも低頻度な
// 経路（provider の構築、work の終了処理、設定の保存）なので、これ以上のキャッシュ層は
// 不要。サーキットブレーカーのパラメータは例外で、失敗経路で毎回読むため、
// applyRetryPolicy が Registry に渡して保持させる。

// retryPolicy reads the global policy; a nil DB yields the zero policy (all
// layers on their built-in defaults).
func (s *Server) retryPolicy() db.LLMRetryPolicy {
	if s.m == nil || s.m.pg == nil {
		return db.LLMRetryPolicy{}
	}
	return s.m.pg.LLMRetryPolicy()
}

// resolveRetry layers one profile's override on top of the global policy and
// converts the result into the form agent.Config carries. Rules combine field by
// field, so a profile that only pins an interval still inherits the global count.
func resolveRetry(o db.RetryOverride, pol db.LLMRetryPolicy) agent.RetryConfig {
	connect := o.Connect.Or(pol.Connect)
	empty := o.Empty.Or(pol.Empty)
	stream := o.Stream.Or(pol.Stream)
	return agent.RetryConfig{
		// 回数はここでは「0=既定 / 負=無効」という元の意味のまま保持する：SDK の MaxRetries /
		// EmptyResponseRetries と完全に同型なので、解決は SDK 側に任せる。
		ConnectAttempts: connect.Attempts, ConnectInterval: connect.Interval(),
		EmptyAttempts: empty.Attempts, EmptyInterval: empty.Interval(),
		StreamAttempts: stream.Attempts, StreamInterval: stream.Interval(),
	}
}

// applyProfileRetry fills cfg.Retry for a profile read from the DB.
func (s *Server) applyProfileRetry(cfg *agent.Config, p *db.LLMProfile) {
	if p == nil {
		return
	}
	cfg.Retry = resolveRetry(p.Retry, s.retryPolicy())
}

// サーキットブレーカー（ポーリングのクールダウン）の既定値で、llmpool の組み込み値と一致する
// ——ここでは「ユーザーが値を設定した」場合にだけ上書きする。
// 意図の再実行の既定値は engine.go の modelErrorRetries / modelErrorRetryBackoff を参照。

// applyRetryPolicy pushes the process-wide layers of the policy into the objects
// that consume them on a hot path: the circuit-breaker registry. Called at
// startup and whenever the policy is saved.
func (s *Server) applyRetryPolicy() {
	pol := s.retryPolicy()
	if s.llmHealth != nil {
		s.llmHealth.SetPolicy(pol.Breaker.Attempts, pol.Breaker.Interval())
	}
}

// modelErrorRetryPolicy resolves the intent-level replay knobs (layer ⑤): how
// many times a model_error work is re-run and how long to back off between runs.
func (e *Engine) modelErrorRetryPolicy() (retries int, backoff time.Duration) {
	retries, backoff = modelErrorRetries, modelErrorRetryBackoff
	if e == nil || e.m == nil || e.m.pg == nil {
		return retries, backoff
	}
	rule := e.m.pg.LLMRetryPolicy().Intent
	if rule.Attempts != 0 {
		retries = max(rule.Attempts, 0)
	}
	if d := rule.Interval(); d > 0 {
		backoff = d
	}
	return retries, backoff
}

// emptyTurnNudgeLimit resolves how many empty-turn continuations one work may
// inject (see steerHooks.Stop). It deliberately reuses layer ②'s knob ——
// 「空応答再試行回数」: 両者は同じ事柄に対する 2 つの手段である。SDK の層は「コンテンツブロックが
// 1 つも無い」を扱い、手段は同じリクエストをそのまま再送すること。ここは「思考だけで、
// 本文もツールも無い」を扱い、手段は指示を 1 つ追加してモデルに既存の思考を抱えたまま
// 続けさせること（そのまま再送しても、コンテキストの形で決まるこの空転には意味がない）。
// 空判定の基準が違うのは、SDK が「イベントを yield したか」で判断し、思考デルタ自体が
// イベントになるため——だがユーザーが「空応答を何回再試行するか」を設定するときに
// 意味しているのは「モデルが実質的な内容を出さなければもう一度」であり、2 つの層が
// 回数を共有してこそこの認識に合う。
//
// 特定の profile の上書きではなくグローバルポリシーを読む：1 回の run の途中で
// フェイルオーバーにより profile が変わりうるが、これは意図全体の総量制限であり、
// 端点の切り替えに合わせて変わってはいけない。意味は SDK の emptyRetries() と同型：
// 0 = 既定の defaultEmptyTurnNudges、-1（負）= 空転の継続を無効化、>0 = その値を使う。
func (e *Engine) emptyTurnNudgeLimit() int {
	if e == nil || e.m == nil || e.m.pg == nil {
		return defaultEmptyTurnNudges
	}
	switch n := e.m.pg.LLMRetryPolicy().Empty.Attempts; {
	case n == 0:
		return defaultEmptyTurnNudges
	case n < 0:
		return 0
	default:
		return n
	}
}
