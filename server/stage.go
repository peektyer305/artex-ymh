package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/stage"
	"github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// 管理下ファイル配布（F13。stage パッケージの意味は stage/stage.go の先頭コメントを参照）。本ファイルの役割は 3 つ:
//   1. 組み立て: stage.Manager を生成し、記帳イベントをタスクの activity へ書き込む（タスクコンテキストがなければログのみ）;
//   2. ダウンロードルート GET /s/{token}/{name}（root mux、JWT の外——対象マシンはトークンを持たない。
//      gate のゲートは /s/ プレフィックスを除外する。gate.go を参照）;
//   3. 管理 API（POST/GET/DELETE /api/stage*、JWT の後）+ worker の stage_share host ツール。

// initStage はプロセス内のステージングマネージャを生成し、記帳を接続する。失敗時はログのみ
// （ステージングサービスなしへ降格）で、起動を止めない。
func (s *Server) initStage(dataDir string) {
	m, err := stage.New(s.ctx, filepath.Join(dataDir, "stage"))
	if err != nil {
		log.Printf("[stage] disabled: %v", err)
		return
	}
	m.OnEvent = s.recordStageEvent
	s.stage = m
	log.Printf("[stage] 管理下ステージングサービス準備完了: GET /s/<token>/<name>（TTL デフォルト %v、上限 %v）", stage.DefaultTTL, stage.MaxTTL)
}

// SetStageBaseURL はステージング URL に組み込む対外ベースアドレス（通常はメインのリッスンアドレス）を設定する。対象マシンが
// どのアドレスでプラットフォームへ戻れるかは配備次第なので、ここでは stage_share の戻り値にそのまま使える絶対 URL を用意するだけ。
func (s *Server) SetStageBaseURL(addr string) {
	if s.stage == nil {
		return
	}
	host := addr
	if strings.HasPrefix(host, "0.0.0.0") {
		host = strings.Replace(host, "0.0.0.0", "127.0.0.1", 1) // 全インターフェースをリッスンするときはループバックの仮値を入れ、実際には対象から到達できるアドレスで置き換える
	}
	s.stage.SetBaseURL("http://" + host)
}

// recordStageEvent は stage の記帳イベントを帰属タスクの activity へ書き込む（engine.emitActivity の
// system/text モードを参照）。タスクが終了済み、またはタスクコンテキストがないときは少なくともサーバー側ログを残す。
func (s *Server) recordStageEvent(ev stage.Event) {
	e := ev.Entry
	summary := fmt.Sprintf("stage %s: %s (token=%s… size=%d bytes, expires %s)",
		ev.Kind, e.Name, e.Token[:8], e.Size, e.ExpiresAt.Format(time.RFC3339))
	if ev.Kind == stage.EventDownload {
		summary = fmt.Sprintf("stage download: %s が %s により %d/%d バイト取得されました（累計ヒット %d 回）",
			e.Name, ev.Remote, ev.Bytes, e.Size, e.Hits)
	}
	log.Printf("[stage] task=%s %s", orDash(e.TaskID), summary)
	if e.TaskID == "" {
		return
	}
	s.m.mu.Lock()
	t := s.m.tasks[e.TaskID]
	s.m.mu.Unlock()
	if t == nil || t.Store == nil {
		return // タスクハンドルはすでに存在しない（終了／削除）。ログは残した
	}
	if _, err := t.Store.AppendActivity(db.Activity{
		Worker: "system", Kind: "text", Tool: "stage_share",
		Summary: summary,
		Detail:  fmt.Sprintf("token=%s name=%s size=%d one_shot=%v expires_at=%s remote=%s bytes=%d hits=%d",
			e.Token, e.Name, e.Size, e.OneShot, e.ExpiresAt.Format(time.RFC3339), ev.Remote, ev.Bytes, e.Hits),
	}); err != nil {
		log.Printf("[stage] activity の書き込みに失敗しました（task=%s）: %v", e.TaskID, err)
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// stageDownload はダウンロードを提供する。root mux に接続し（JWT の外）、列挙対策の一貫した 404 とレート制限は
// いずれも stage.Manager.Download にある。
func (s *Server) stageDownload(w http.ResponseWriter, r *http.Request) {
	if s.stage == nil {
		http.NotFound(w, r)
		return
	}
	s.stage.Download(w, r, r.PathValue("token"), r.PathValue("name"))
}

// --- 管理 API（JWT の後、worker ツールと UI が使用） ---

type stageCreateReq struct {
	Path          string `json:"path"`           // プラットフォーム側に既存のファイル（content と二択）
	Name          string `json:"name"`           // content モードでは必須
	ContentBase64 string `json:"content_base64"` // content モード
	TTLMinutes    int    `json:"ttl_minutes"`    // 0 = デフォルト 15 分、上限 120
	OneShot       bool   `json:"one_shot"`       // 最初の完全なダウンロード成功後に即破棄
	TaskID        string `json:"task_id"`        // 帰属タスク（activity 記帳）
}

func (s *Server) stageCreate(w http.ResponseWriter, r *http.Request) {
	if s.stage == nil {
		writeErr(w, 503, "stage service disabled")
		return
	}
	var req stageCreateReq
	if err := decode(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	opts := stage.PutOpts{
		TTL:     time.Duration(req.TTLMinutes) * time.Minute,
		OneShot: req.OneShot,
		TaskID:  strings.TrimSpace(req.TaskID),
	}
	var e *stage.Entry
	var err error
	switch {
	case strings.TrimSpace(req.Path) != "":
		e, err = s.stage.PutFile(strings.TrimSpace(req.Path), opts)
	case req.ContentBase64 != "":
		data, derr := base64.StdEncoding.DecodeString(req.ContentBase64)
		if derr != nil {
			writeErr(w, 400, "content_base64 のデコードに失敗しました: "+derr.Error())
			return
		}
		e, err = s.stage.PutContent(req.Name, data, opts)
	default:
		writeErr(w, 400, "path と content_base64 の少なくとも一方を指定してください")
		return
	}
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, e)
}

func (s *Server) stageList(w http.ResponseWriter, r *http.Request) {
	if s.stage == nil {
		writeJSON(w, 200, []stage.Entry{})
		return
	}
	writeJSON(w, 200, s.stage.List())
}

func (s *Server) stageDelete(w http.ResponseWriter, r *http.Request) {
	if s.stage == nil {
		writeErr(w, 503, "stage service disabled")
		return
	}
	if !s.stage.Delete(r.PathValue("token")) {
		writeErr(w, 404, "stage entry not found")
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": r.PathValue("token")})
}

// --- stage_share host ツール（デフォルトで worker にバインド。tools テーブルで agent ごとに絞り込む） ---

// stageShareTool は worker が「ワークスペース内で生成済みのファイル」を管理下ステージングサービスへ渡し、
// ランダムなトークン URL + 有効期限を受け取れるようにする。TaskID は RunInfo から権威的に注入し、モデルには公開しない。
func (s *Server) stageShareTool() actool.CoreTool {
	allow := func(context.Context, json.RawMessage, permission.Context) permission.Decision {
		return permission.Allowed()
	}
	return actool.Build(actool.Spec{
		Name: "stage_share",
		Description: "ワークスペース内で生成済みのファイルを、プラットフォームの管理下ステージングサービスで対象へ配布する: ランダムなトークンのダウンロード URL（/s/<token>/<name>、ディレクトリ一覧なし）と有効期限を返す。" +
			"ハード TTL はデフォルト 15 分（ttl_minutes で調整可能、上限 120）で、期限が来ると自動破棄される。one_shot=true なら対象が最初に完全にダウンロードした直後に即破棄する。" +
			"【対象へのツール／payload 配布にのみ使用】——戦利品／認証情報を配布しないこと。データの回収は必ずセッション読み取り／evidence 経由で行う。" +
			"返される URL のホストはプラットフォームのリッスンアドレスである。対象マシンからそのアドレスに到達できないときは、対象から到達できるプラットフォームのアドレス／トンネルでホスト部分を置き換える（パスとトークンは変えない）。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":        map[string]any{"type": "string", "description": "配布するローカルファイルの絶対パス（ワークスペース内で生成済みのツール／payload）"},
				"ttl_minutes": map[string]any{"type": "integer", "description": "生存時間（分）。デフォルト 15、上限 120"},
				"one_shot":    map[string]any{"type": "boolean", "description": "true = 対象が最初に完全なダウンロードに成功した直後に破棄（デフォルト false）"},
			},
			"required": []any{"path"},
		},
		ReadOnly:    func(json.RawMessage) bool { return false },
		Permissions: allow,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			if s.stage == nil {
				return actool.Errorf("stage ステージングサービスは有効になっていません"), nil
			}
			var a struct {
				Path       string `json:"path"`
				TTLMinutes int    `json:"ttl_minutes"`
				OneShot    bool   `json:"one_shot"`
			}
			_ = json.Unmarshal(in, &a)
			if strings.TrimSpace(a.Path) == "" {
				return actool.Errorf("path は必須パラメータです"), nil
			}
			ri := agent.RunInfoFrom(ctx)
			taskID := ""
			if ri.TaskID > 0 { // 0 = タスク以外の run（チャットセッション）で、タスクコンテキストなし
				taskID = strconv.FormatInt(ri.TaskID, 10)
			}
			e, err := s.stage.PutFile(strings.TrimSpace(a.Path), stage.PutOpts{
				TTL:     time.Duration(a.TTLMinutes) * time.Minute,
				OneShot: a.OneShot,
				TaskID:  taskID,
			})
			if err != nil {
				return actool.Errorf(err.Error()), nil
			}
			return jsonResult(map[string]any{
				"url":        e.URL,
				"expires_at": e.ExpiresAt.Format(time.RFC3339),
				"one_shot":   e.OneShot,
				"size":       e.Size,
				"note":       "対象マシンは curl/wget でこの URL を取得する。期限切れ、または（one_shot 有効時は）最初の完全なダウンロード後に自動破棄される。URL のホストはプラットフォームのリッスンアドレスで、対象から到達できないときは対象から到達できるアドレスでホスト部分を置き換える。",
			})
		},
	})
}

// seedStageShareTool は stage_share を tools テーブルへ seed し、デフォルトで worker にバインドする（トラフィックの
// host ツールと同じ SeedTool の初回挿入セマンティクス: 既存 DB のユーザー編集は上書きしない）。
func (s *Server) seedStageShareTool() {
	t := s.stageShareTool()
	schema, _ := json.Marshal(t.InputSchema())
	workerAgents, _ := json.Marshal([]string{"worker"})
	if err := s.m.PG().SeedTool(t.Name(), t.Description(), schema, workerAgents); err != nil {
		log.Printf("[tools] %s の seed に失敗しました: %v", t.Name(), err)
	}
}
