package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/agent/chainskel"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/intercept"
	actool "github.com/Autumn-27/norma/tool"
)

// settingFindingVerifyMode は検証パイプラインのフェーズ別スイッチ: auto（デフォルト、外部ネットワーク段階では
// 自動検証）/ off（イントラネット段階ではデフォルトで無効——イントラネットの誤検知は推論ミスが多く再実行の価値が低く、しかも再実行
// = エクスプロイト動作の再トリガーで、EDR／監査／アカウントロックに当たる代償が質的に変わる。FIXPLAN-AND-EVALUATION.md
// 2026-09-13 の段階別検証方針を参照）。今回は単一スイッチのみ。タスク段階による自動切替は後続タスク。
// off のとき自動ディスパッチはスキップする（水位は通常どおり進み、再実行はしない）。手動の単件検証（POST checks）は影響を受けない。
const settingFindingVerifyMode = "finding_verify_mode"

// findingVerifyAutoEnabled reports whether the auto-dispatch path is on. Only the
// explicit "off" disables it — a missing/unknown value keeps the auto default.
func findingVerifyAutoEnabled(mode string) bool {
	return mode != "off"
}

func (s *Server) listFindingChecks(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "bad finding id")
		return
	}
	f, err := pg.GetFinding(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if f == nil {
		writeErr(w, 404, "finding not found")
		return
	}
	items, err := pg.ListFindingChecks(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"checks": items})
}

// startFindingCheck は手動で 1 回の検証を開始する（イントラネット段階の手動単件の出口。W3 統合後の増分
// 再検証も同じ入口 RequestFindingCheck を通る）。finding_verify_mode の制限を受けない。
func (s *Server) startFindingCheck(w http.ResponseWriter, r *http.Request) {
	pg := s.pg(w)
	if pg == nil {
		return
	}
	id, ok := pathInt(r, "id")
	if !ok || id <= 0 {
		writeErr(w, 400, "bad finding id")
		return
	}
	var req struct {
		Kind string `json:"kind"`
	}
	if !decodeConversationRequest(w, r, &req) {
		return
	}
	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		kind = db.FindingCheckKindVerify
	}
	if kind != db.FindingCheckKindVerify && kind != db.FindingCheckKindRetest {
		writeErr(w, 400, "kind は verify / retest のいずれかです")
		return
	}
	f, err := pg.GetFinding(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if f == nil {
		writeErr(w, 404, "finding not found")
		return
	}
	check, created, err := s.RequestFindingCheck(id, kind)
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, 404, "finding not found")
		return
	}
	if err != nil {
		writeErr(w, 409, err.Error())
		return
	}
	code := http.StatusOK
	if created {
		code = http.StatusAccepted
	}
	writeJSON(w, code, map[string]any{"check": check, "created": created})
}

// RequestFindingCheck は finding_checks パイプラインの再利用可能な入口（C2 用に用意）: スケジューラの
// 自動ディスパッチ、上記の手動 HTTP 入口、後続の統合（W3）による増分再検証がすべてこれを呼ぶ。まず
// finding_checks 行を作り（pending、スナップショット+セッションを含む）、次にセッションを実行する。同じ finding にすでに有効な
// check があるときは重複排除して返す（created=false）。
func (s *Server) RequestFindingCheck(findingID int64, kind string) (*db.FindingCheck, bool, error) {
	if s.m.pg == nil {
		return nil, false, errors.New("db が初期化されていません")
	}
	a, err := s.m.pg.GetAgentByKey(db.FindingVerifierAgentKey)
	if err != nil {
		return nil, false, err
	}
	if a == nil || !a.Enabled {
		return nil, false, errors.New("脆弱性検証 Agent が存在しないか無効です。Agent 管理で verifier を設定してください")
	}
	if s.resolveChatAgent(&db.Conversation{AgentKey: a.Key}) == nil {
		return nil, false, errors.New(s.chatUnavailableReason())
	}
	if s.ctx.Err() != nil {
		return nil, false, errors.New("サービスは停止中です")
	}
	check, conv, created, err := s.m.pg.CreateFindingCheck(s.ctx, findingID, kind)
	if err != nil {
		return nil, false, err
	}
	if created {
		busyKey := s.convBusyKey(conv.ID)
		s.chatMu.Lock()
		s.chatBusy[busyKey] = true
		s.chatMu.Unlock()
		s.runConversation(conv, check.InitialMessage(), busyKey)
	}
	return check, created, nil
}

// onFindingMerged は区分 A 統合後の C2 増分再検証フック: 新しい証拠が既存の「検証済み」
// 状態に便乗してしまうため、統合後の finding を再検証する必要がある。dispatchFindingVerify と同様に
// finding_verify_mode スイッチに従う。RequestFindingCheck の内部で有効な check を重複排除するので、重複した
// トリガーは安全。コールバックは Record トランザクションのコミット後に実行し、失敗時はログのみで報告の主フローには影響しない。
func (s *Server) onFindingMerged(findingID int64) {
	mode, _, err := s.m.pg.GetSetting(settingFindingVerifyMode)
	if err != nil {
		log.Printf("[verifier] %s の読み取りに失敗しました: %v", settingFindingVerifyMode, err)
		return
	}
	if !findingVerifyAutoEnabled(mode) {
		return
	}
	if _, _, err := s.RequestFindingCheck(findingID, db.FindingCheckKindVerify); err != nil {
		log.Printf("[verifier] 統合後の増分再検証 finding %d のディスパッチに失敗しました: %v", findingID, err)
	}
}

// dispatchFindingVerify は scheduler の on_finding 自動ディスパッチフック: イベントは
// node_id だけを持ち、FindingIDByNodeID で finding を逆引きする（ノードと finding は同一トランザクションで書かれ、この時点で
// 可視になっている）。finding_verify_mode=off のときはスキップ——再実行はせず、漏れは手動入口で補う。
func (s *Server) dispatchFindingVerify(e db.TaskEvent) {
	mode, _, err := s.m.pg.GetSetting(settingFindingVerifyMode)
	if err != nil {
		log.Printf("[verifier] %s の読み取りに失敗しました: %v", settingFindingVerifyMode, err)
		return
	}
	if !findingVerifyAutoEnabled(mode) {
		return
	}
	findingID, err := s.m.pg.FindingIDByNodeID(e.NodeID)
	if err != nil {
		log.Printf("[verifier] node %d からの finding 逆引きに失敗しました: %v", e.NodeID, err)
		return
	}
	if _, _, err := s.RequestFindingCheck(findingID, db.FindingCheckKindVerify); err != nil {
		log.Printf("[verifier] 自動検証 finding %d のディスパッチに失敗しました: %v", findingID, err)
	}
}

// Both tools derive the finding from server-owned conversation context. Tool
// arguments cannot redirect a result into a different finding or conversation.
func (s *Server) findingCheckTools() []actool.CoreTool {
	return []actool.CoreTool{
		roTool("get_finding_check_context", "現在の検証セッションに関連する脆弱性スナップショット（finding、影響を受けるアセット、元タスクの制約）と検証ステータスを読む。引数なし、このセッションのみ読み取り可能。",
			objSchema(map[string]any{}), func(ctx context.Context, _ json.RawMessage) (actool.Result, error) {
				c, err := s.m.pg.FindingCheckForConversation(ctx, intercept.ConvIDFromContext(ctx))
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				if c == nil {
					return actool.Errorf("現在のセッションには検証レコードが紐づいていません。脆弱性の詳細から検証を開始してください"), nil
				}
				var constraints []db.Constraint
				f, err := s.m.pg.GetFinding(c.FindingID)
				if err != nil {
					return actool.Errorf(err.Error()), nil
				}
				if f != nil && f.TaskID != nil {
					if task, ok := s.m.Task(strconv.FormatInt(*f.TaskID, 10)); ok {
						constraints, err = task.Store.ListConstraints()
						if err != nil {
							return actool.Errorf(err.Error()), nil
						}
					}
				}
				// chains L4 → verifier 判定層: finding の vulnclass に応じて反証用
				// チェックリストを注入する（共通層は常に付け、カテゴリ一致時は専用項目を追加）。結論を 1 件ずつ判定基準に通すため。
				vulnclass := ""
				if f != nil {
					vulnclass = f.VulnClass
				}
				return jsonResult(map[string]any{"check": c, "current_constraints": constraints,
					"verifier_checklist": chainskel.VerifierChecklist(vulnclass)})
			}),
		wrTool("record_finding_check_result", "現在の検証セッションの結論を 1 つだけ保存する（verified は真と確認 / false_positive は誤検知 / inconclusive は確認できず）。元の脆弱性のエビデンスとレポートは変更しない。セッションが正常終了し結論が verified のとき、システムは脆弱性のステータスを自動で「確認済み」に変える。false_positive のときは誤検知としてマークし探索ノードを隠す。inconclusive はステータスを変えない。今回の実際の確認内容のエビデンスを必ず示し、確認できないときはブロック要因を明記する。",
			objSchema(map[string]any{
				"verdict":  map[string]any{"type": "string", "enum": []string{"verified", "false_positive", "inconclusive"}},
				"summary":  strParam("今回の検証結論の要約"),
				"evidence": strParam("Markdown: 今回の実際の手順、観察、対照、結論の根拠。確認できない場合は確認済みの内容とブロック要因を列挙する"),
			}, "verdict", "summary", "evidence"), func(ctx context.Context, in json.RawMessage) (actool.Result, error) {
				var a struct {
					Verdict  string `json:"verdict"`
					Summary  string `json:"summary"`
					Evidence string `json:"evidence"`
				}
				if err := json.Unmarshal(in, &a); err != nil {
					return actool.Errorf(err.Error()), nil
				}
				if err := s.m.pg.RecordFindingCheckResult(ctx, intercept.ConvIDFromContext(ctx), a.Verdict, a.Summary, a.Evidence); err != nil {
					return actool.Errorf(err.Error()), nil
				}
				return jsonResult(map[string]any{"saved": true, "verdict": a.Verdict})
			}),
	}
}

// Seed the editable agent atomically, plus its on_finding trigger. Once seeded,
// user edits/deletion survive restarts; a pre-existing key is kept.
func (s *Server) seedFindingVerifier() error {
	for _, t := range s.findingCheckTools() {
		schema, _ := json.Marshal(t.InputSchema())
		bindings, _ := json.Marshal([]string{db.FindingVerifierAgentKey})
		if err := s.m.pg.SeedTool(t.Name(), t.Description(), schema, bindings); err != nil {
			return err
		}
	}
	const flag = "finding_verifier_seed_v1"
	tx, err := s.m.pg.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Lock this migration, including concurrent server initialization.
	if _, err = tx.Exec(`SELECT pg_advisory_xact_lock(7337741011)`); err != nil {
		return err
	}
	var done string
	err = tx.QueryRow(`SELECT value FROM settings WHERE key=$1`, flag).Scan(&done)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if done == "true" {
		return nil
	}
	var id int64
	err = tx.QueryRow(`INSERT INTO agents(key,name,description,role,builtin,enabled)
	VALUES ($1,'脆弱性検証','新しい脆弱性の報告後に自動でトリガーされ、PoC を再実行して誤検知を反証し、独立した検証結論を保存する。','assistant',false,true)
	ON CONFLICT (key) DO NOTHING RETURNING id`, db.FindingVerifierAgentKey).Scan(&id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if id > 0 {
		var pid int64
		if err = tx.QueryRow(`INSERT INTO agent_prompts(agent_id,version,template_text,note,updated_by)
		VALUES ($1,1,$2,'組み込みデフォルト','system') RETURNING id`, id, agent.VerifierDefaultPrompt).Scan(&pid); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE agents SET current_prompt_id=$1 WHERE id=$2`, pid, id); err != nil {
			return err
		}
	}
	if _, err = tx.Exec(`INSERT INTO settings(key,value) VALUES ($1,'true') ON CONFLICT(key) DO UPDATE SET value='true'`, flag); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if id > 0 {
		// on_finding トリガー: スケジューラはこれを使って新しい finding を finding_checks パイプラインへ
		// ディスパッチする（verifier は fireFindings で特別扱いし、汎用トリガーキューは通らない。FindingMessage は空のままにし、
		// セッションの最初のメッセージは CreateFindingCheck が生成する）。ユーザーはトリガー UI でこれを無効化でき、
		// finding_verify_mode=off と同等になる。
		if _, err := s.m.pg.CreateTrigger(&db.AgentTrigger{
			AgentKey:  db.FindingVerifierAgentKey,
			Enabled:   true,
			OnFinding: true,
		}); err != nil {
			log.Printf("[verifier] on_finding トリガーの作成に失敗しました: %v", err)
		}
	}
	return nil
}

func (s *Server) finishCheck(id int64, status, reason string) {
	if err := s.m.pg.FinishFindingCheck(id, status, reason); err != nil {
		// Surface failure to the conversation runner rather than inventing a result.
		log.Printf("[check %d] finish: %v", id, err)
	}
}
