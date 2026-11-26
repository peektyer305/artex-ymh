package server

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/Autumn-27/artex/agent"
)

// getLLM returns the current LLM config (key never exposed).
func (s *Server) getLLM(w http.ResponseWriter, r *http.Request) {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	writeJSON(w, 200, map[string]any{
		"configured":       s.llmOn,
		"provider":         s.llmCfg.Provider(),
		"model":            s.llmCfg.Model,
		"base_url":         s.llmCfg.BaseURL,
		"proxy":            s.llmCfg.Proxy,
		"key_set":          s.llmCfg.APIKey != "",
		"rate_per_second":  s.llmCfg.RatePerSecond,
		"rate_per_minute":  s.llmCfg.RatePerMinute,
		"context_window_k": s.llmCfg.ContextWindowK,
		"thinking_type":    s.llmCfg.ThinkingType,
		"reasoning_effort": s.llmCfg.ReasoningEffort,
	})
}

// setLLM configures the LLM at runtime. A blank api_key keeps the existing key.
func (s *Server) setLLM(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider        string  `json:"provider"`
		Model           string  `json:"model"`
		BaseURL         string  `json:"base_url"`
		Proxy           string  `json:"proxy"`
		APIKey          string  `json:"api_key"`
		RatePerSecond   float64 `json:"rate_per_second"`
		RatePerMinute   float64 `json:"rate_per_minute"`
		ContextWindowK  int     `json:"context_window_k"`
		ThinkingType    string  `json:"thinking_type"`
		ReasoningEffort string  `json:"reasoning_effort"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	cfg := agent.ConfigFrom(req.Provider, req.Model, req.BaseURL, req.APIKey, req.Proxy)
	cfg.RatePerSecond, cfg.RatePerMinute = req.RatePerSecond, req.RatePerMinute
	cfg.ThinkingType = req.ThinkingType
	cfg.ReasoningEffort = req.ReasoningEffort
	if k := req.ContextWindowK; k > 0 { // 0 = keep default (200K); cap at 1M
		if k > 1000 {
			k = 1000
		}
		cfg.ContextWindowK = k
	}
	if cfg.APIKey == "" {
		s.cfgMu.Lock()
		cfg.APIKey = s.llmCfg.APIKey // keep existing key if not re-entered
		s.cfgMu.Unlock()
	}
	if cfg.APIKey == "" {
		writeErr(w, 400, "api_key required")
		return
	}
	// Validate provider construction before persisting it as the active profile.
	if _, err := cfg.NewProvider(); err != nil {
		writeErr(w, 400, "provider init failed: "+err.Error())
		return
	}
	if err := s.saveLLMConfig(cfg); err != nil {
		writeErr(w, 500, "persist provider failed: "+err.Error())
		return
	}
	s.invalidateProfileAgents()
	if _, ok := s.loadLLMConfig(); !ok {
		writeErr(w, 500, "saved provider is unavailable")
		return
	}
	if err := s.applyLLM(cfg); err != nil {
		writeErr(w, 400, "provider init failed: "+err.Error())
		return
	}
	log.Printf("[engine] LLM configured via UI: %s / %s", cfg.Provider(), cfg.Model)
	s.getLLM(w, r)
}

// testLLM makes a real minimal completion to verify the config works.
func (s *Server) testLLM(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Provider         string `json:"provider"`
		Model            string `json:"model"`
		BaseURL          string `json:"base_url"`
		Proxy            string `json:"proxy"`
		APIKey           string `json:"api_key"`
		ThinkingType     string `json:"thinking_type"`
		ReasoningEffort  string `json:"reasoning_effort"`
		ProfileID        *int64 `json:"profile_id"`         // 保存済み profile をテストするとき渡す: api_key が空なら保存済みキーを使う
		Streaming        *bool  `json:"streaming"`          // 省略=ストリーミング。profile 保存時と同じ既定
		SessionHeaderKey string `json:"session_header_key"` // 非空=テスト時にもこのカスタムセッションヘッダーを付け、値は使い捨てのランダム session id
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	cfg := agent.ConfigFrom(req.Provider, req.Model, req.BaseURL, req.APIKey, req.Proxy)
	// mirror production: send the SAME thinking params so a provider that rejects the
	// reasoning_effort/thinking field fails the test too (no false "test ok, run 400").
	cfg.ThinkingType = req.ThinkingType
	cfg.ReasoningEffort = req.ReasoningEffort
	// 同様に、送受信モードもその profile の選択に従う: 片方のチャネルしか扱えないエンドポイントはここで
	// 露見させるべきで、セッションに入ってから「テストに通った設定がまったく動かない」と気づくのは避ける。
	if req.Streaming != nil {
		cfg.Stream = *req.Streaming
	}
	// カスタムセッションヘッダー名もその設定に従う: 非空ならテストリクエストにも付け(値は使い捨てのランダム session id、
	// TestConnection 参照)。opencode zen など x-opencode-session を必須とするエンドポイントは、欠けると
	// そのまま 400 になるためテスト経路でも必ず付ける。さもないと「チャットは通るのにテストは 400」。
	cfg.SessionHeaderKey = req.SessionHeaderKey
	// API Key の解決優先順位: フォーム入力 > 指定 profile の保存キー > グローバル設定のキー。
	// 保存済み profile のキーはブラウザへ返さないため、保存済み設定をテストするときフォームは空になり、DB から取得する。
	// セッションヘッダー名も同様: フォームに無い場合は保存済み profile の値で補う。
	if req.ProfileID != nil && (cfg.APIKey == "" || cfg.SessionHeaderKey == "") {
		if p, err := s.m.pg.ProfileByID(*req.ProfileID); err == nil && p != nil {
			if cfg.APIKey == "" {
				cfg.APIKey = p.APIKey
			}
			if cfg.SessionHeaderKey == "" {
				cfg.SessionHeaderKey = p.SessionHeaderKey
			}
		}
	}
	if cfg.APIKey == "" {
		s.cfgMu.Lock()
		cfg.APIKey = s.llmCfg.APIKey
		s.cfgMu.Unlock()
	}
	if cfg.APIKey == "" {
		writeJSON(w, 200, map[string]any{"ok": false, "error": "API Key が指定されていません"})
		return
	}
	// 再試行パラメータは接続テストに【持ち込まない】: テストには 30s のハードタイムアウトがあり、設定の再試行回数/長い間隔を
	// 重ねると、本来使えるエンドポイントを「タイムアウト失敗」にしてしまう。テストが見るのは「このエンドポイントが通るか」で、再試行のリズムは
	// 実行してからの話。
	lat, reply, err := agent.TestConnection(r.Context(), cfg)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	// モデルの実際の返信を返し、「テスト成功」に根拠を持たせる: HTTP 200 だけでなく、確かに発話したことが見えるようにする。
	writeJSON(w, 200, map[string]any{
		"ok": true, "latency_ms": lat.Milliseconds(), "model": cfg.Model, "reply": truncateReply(reply),
	})
}

// truncateReply clips a connection-test reply for display. A model told to answer
// "OK" can still ramble (or think out loud); the UI only needs enough to show it
// really said something. Rune-based so multibyte text never splits mid-character.
func truncateReply(s string) string {
	const max = 200
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

type createTaskReq struct {
	Name                 string   `json:"name,omitempty"` // 任意のタスク名;省略/空=名称未設定
	CategoryID           *int64   `json:"category_id,omitempty"`
	Description          string   `json:"description"`
	Goal                 string   `json:"goal"`
	LLMProfileID         *int64   `json:"llm_profile_id,omitempty"`    // 本タスクを実行する LLM 設定を指定;省略/null=有効化設定を使用
	LLMProfileIDs        []int64  `json:"llm_profile_ids,omitempty"`   // 順序付きのタスク単位設定チェーン;先頭が初期有効
	SourceTaskIDs        []string `json:"source_task_ids,omitempty"`   // 直接かつ読み取り専用で継承するソースタスクのみ
	CompanyIDs           []int64  `json:"company_ids,omitempty"`       // 企業スコープを紐付け、現在の企業アセットをスナップショット;アセットの複製や意図の強制生成はしない
	TimeoutSeconds       int      `json:"timeout_seconds"`             // タスク単位のタイムアウト(秒);0/省略=無制限
	PlanHeartbeatSeconds int      `json:"plan_heartbeat_seconds"`      // planner ハートビートのトリガー間隔(秒);0/省略=既定 600(10min);下限=既定=600 で、下回る場合は自動で 600 に引き上げ
	SeedFirstIntent      *bool    `json:"seed_first_intent,omitempty"` // 作成時にシード意図を 1 件投入(内容=説明+ゴール)し、worker が最初の planner を待たずに走り出す;省略/null=既定オフで、標準の「まず計画してから実行」を通す。明示的に true を渡したときだけオン(CTF のように 1 つの work で解決できる場合は起動前の planner ターンを省ける)。
	CoverageEnabled      *bool    `json:"coverage_enabled,omitempty"`  // アセットカバレッジ機能;省略/null=既定オン(true)。false=カバレッジの計算/表示/範囲の自動累積を停止し、add_task_scope/list_untested_assets を隠す。company の紐付けには影響しない。
}
