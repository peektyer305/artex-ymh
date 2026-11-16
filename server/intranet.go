// intranet.go は「イントラネット作戦ページ」の手動操作 API（フェーズ 1-3 の機能の Web 公開面）:
// セッション台帳/手動 exec/ファイルの一覧・読み書き・削除、トンネル台帳/teardown、認証情報のマスク済み一覧、トポロジ組み立て。
// すべて /api 配下の requireAuth の後ろ（JWT）にあり、agent の host ツール（session.go/
// tunnel.go/credentials.go）とは完全に分離されている——あちらはモデルが guard 承認を経るチャネル、こちらは
// ログイン済み操作者の手動チャネルで、監査はサーバーログ（[intranet] プレフィックス、操作者/セッション/コマンドを含む）で行う。
package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/session"
)

// auditIntranet は手動操作の監査ログを出力する:操作者（JWT subject）/送信元アドレス/操作の詳細。
// 手動 exec/write/teardown/delete は必ずここを通る;失敗時も記録する（結果は詳細に含める）。
func (s *Server) auditIntranet(r *http.Request, format string, args ...any) {
	op := "unknown"
	if tok := extractToken(r); tok != "" {
		if claims, err := parseAccessToken(tok, s.jwtKey); err == nil && claims.Subject != "" {
			op = claims.Subject
		}
	}
	log.Printf("[intranet] op=%s remote=%s %s", op, r.RemoteAddr, fmt.Sprintf(format, args...))
}

// intranetSessionsReady はルートレベルの前置チェック:セッションサブシステム（DB/レジストリ）が未準備の場合は 503 を返す。
func (s *Server) intranetSessionsReady(w http.ResponseWriter) bool {
	if s.sessStore == nil || s.sessReg == nil {
		writeErr(w, 503, "セッションサブシステムが未準備です（DB 未接続）")
		return false
	}
	return true
}

func pathID(r *http.Request, name string) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("不正な %s: %q", name, r.PathValue(name))
	}
	return id, nil
}

// sessionItem は台帳行の Web 投影：secret（接続パスワード/暗号化鍵）は含めない（server から外に出さない）。
func sessionItem(rec *db.SessionRecord) map[string]any {
	item := map[string]any{
		"id": rec.ID, "kind": rec.Kind, "url": rec.URL, "lang": rec.Lang,
		"host_asset_id": rec.HostAssetID, "status": rec.Status,
		"last_beat": rec.LastBeat, "created_at": rec.CreatedAt,
		"created_by_task": rec.CreatedByTask, "created_by_intent": rec.CreatedByIntent,
	}
	// platform は secret JSON の中（検出時のフィンガープリント）;解析失敗時はフィールドが欠けるだけで、一覧には影響しない。
	var sec session.Secret
	if err := json.Unmarshal(rec.Secret, &sec); err == nil && sec.Platform != "" {
		item["platform"] = sec.Platform
	}
	return item
}

// GET /api/sessions?task_id= — セッション台帳の全量（dead を含む、フロント側で絞り込む）;
// task_id を渡すと本タスクが作成したセッションだけを残し（トポロジと同じ基準）、渡さなければ全体ビューを維持する。
func (s *Server) intranetListSessions(w http.ResponseWriter, r *http.Request) {
	if !s.intranetSessionsReady(w) {
		return
	}
	taskID, ok := parseTaskIDQuery(w, r)
	if !ok {
		return
	}
	recs, err := s.sessStore.List(r.Context(), "")
	if err != nil {
		writeErr(w, 500, "セッション台帳の読み込みに失敗しました: "+err.Error())
		return
	}
	if taskID > 0 {
		filtered := recs[:0]
		for _, rec := range recs {
			if rec.CreatedByTask == taskID {
				filtered = append(filtered, rec)
			}
		}
		recs = filtered
	}
	items := []map[string]any{}
	for _, rec := range recs {
		items = append(items, sessionItem(rec))
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

// GET /api/sessions/{id} — 単一レコードの詳細。
func (s *Server) intranetGetSession(w http.ResponseWriter, r *http.Request) {
	if !s.intranetSessionsReady(w) {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	rec, err := s.sessStore.Get(r.Context(), id)
	if err != nil {
		writeErr(w, 500, "セッションの読み込みに失敗しました: "+err.Error())
		return
	}
	if rec == nil {
		writeErr(w, 404, fmt.Sprintf("セッション %d は存在しません", id))
		return
	}
	writeJSON(w, 200, sessionItem(rec))
}

// lookupLiveSession はレジストリから生きたセッションを取得する;存在しない/既に死んでいる場合は意味の明確なエラーを返す（手動チャネル向けの文言）。
func (s *Server) lookupLiveSession(id int64) (session.Session, error) {
	sess, ok := s.sessReg.Get(id)
	if !ok {
		return nil, fmt.Errorf("セッション %d はレジストリにありません（未登録/削除済み/プロセス再起動後の復元失敗）", id)
	}
	return sess, nil
}

// POST /api/sessions/{id}/exec {command, timeout?} → {stdout,stderr,ms,timed_out}。
// 手動実行:監査ログを必ず記録する（操作者/セッション/コマンド/所要時間/結果）。
func (s *Server) intranetExec(w http.ResponseWriter, r *http.Request) {
	if !s.intranetSessionsReady(w) {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var a struct {
		Command string `json:"command"`
		Timeout int    `json:"timeout"`
	}
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		writeErr(w, 400, "リクエストボディの JSON 解析に失敗しました: "+err.Error())
		return
	}
	a.Command = strings.TrimSpace(a.Command)
	if a.Command == "" {
		writeErr(w, 400, "command は必須です")
		return
	}
	if a.Timeout <= 0 {
		a.Timeout = 15
	}
	if a.Timeout > 300 {
		a.Timeout = 300 // 手動チャネルの上限。長いコマンドでページが固まるのを防ぐ
	}
	sess, err := s.lookupLiveSession(id)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	start := time.Now()
	stdout, stderr, execErr := sess.Exec(r.Context(), a.Command, time.Duration(a.Timeout)*time.Second)
	ms := time.Since(start).Milliseconds()
	s.sessStore.Touch(r.Context(), id)
	// ドライバ内部は context.WithTimeout でタイムアウトを実装しており、エラー文言に "deadline exceeded" が含まれる。
	timedOut := execErr != nil && (errors.Is(r.Context().Err(), context.DeadlineExceeded) ||
		strings.Contains(execErr.Error(), "deadline exceeded"))
	if execErr != nil && !timedOut {
		s.auditIntranet(r, "exec session=%d cmd=%q ms=%d 失敗: %v", id, a.Command, ms, execErr)
		writeErr(w, 502, "実行に失敗しました: "+execErr.Error())
		return
	}
	s.auditIntranet(r, "exec session=%d cmd=%q ms=%d timed_out=%v stdout=%dB",
		id, a.Command, ms, timedOut, len(stdout))
	out := map[string]any{
		"stdout": truncateOutput(stdout), "stderr": truncateOutput(stderr),
		"ms": ms, "timed_out": timedOut,
	}
	if timedOut {
		out["error"] = "実行タイムアウト: " + execErr.Error()
	}
	if len(stdout) > maxSessionToolOutput || len(stderr) > maxSessionToolOutput {
		out["truncated"] = true
	}
	writeJSON(w, 200, out)
}

// sessionProbeTimeout は手動ヘルスチェック（Test プローブ）の共通タイムアウト。
const sessionProbeTimeout = 10 * time.Second

// POST /api/sessions/{id}/probe — 手動でヘルスチェックを実行する:セッションに無害な Test() プローブを
// 実行する（10s タイムアウト）。成功 → status=alive + last_beat を更新;失敗 → 正直に dead を記録
// （「演習場は停止済みなのに台帳は alive のまま」という偽の alive を是正）。{alive, ms, error?} を返す。
func (s *Server) intranetProbeSession(w http.ResponseWriter, r *http.Request) {
	if !s.intranetSessionsReady(w) {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	sess, err := s.lookupLiveSession(id)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), sessionProbeTimeout)
	defer cancel()
	start := time.Now()
	probeErr := sess.Test(ctx)
	ms := time.Since(start).Milliseconds()
	if probeErr != nil {
		// dead の記録には独立した短い ctx を使う:チェック用 ctx はタイムアウトでキャンセル済みの可能性があり、dead の記録は必ず永続化する必要がある。
		dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
		uerr := s.sessStore.UpdateStatus(dctx, id, db.SessionDead)
		dcancel()
		if uerr != nil {
			writeErr(w, 500, "ヘルスチェックに失敗し、状態の永続化にも失敗しました: "+uerr.Error())
			return
		}
		s.auditIntranet(r, "probe session=%d ms=%d dead: %v", id, ms, probeErr)
		writeJSON(w, 200, map[string]any{"alive": false, "ms": ms, "error": probeErr.Error()})
		return
	}
	// ヘルスチェック成功:状態を alive に戻し（dead から復活可能）、ハートビートを更新する。
	dctx, dcancel := context.WithTimeout(context.Background(), 5*time.Second)
	uerr := s.sessStore.UpdateStatus(dctx, id, db.SessionAlive)
	dcancel()
	if uerr != nil {
		writeErr(w, 500, "状態の永続化に失敗しました: "+uerr.Error())
		return
	}
	s.sessStore.Touch(r.Context(), id)
	s.auditIntranet(r, "probe session=%d ms=%d alive", id, ms)
	writeJSON(w, 200, map[string]any{"alive": true, "ms": ms})
}

// shellQuote は customtool.go と同じ実装を再利用する（シングルクォートで囲み、' → '\''）。

type dirEntry struct {
	Name  string `json:"name"`
	IsDir bool   `json:"is_dir"`
	Size  int64  `json:"size"`
}

// parseLsLa は `ls -la` の出力をエントリ一覧に解析する（純粋関数、テスト可能）。
// total 行、. / ..、判別できない行はスキップする;symlink の "name -> target" は name だけを残す。
func parseLsLa(out string) []dirEntry {
	entries := []dirEntry{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" || strings.HasPrefix(line, "total ") {
			continue
		}
		fields := strings.Fields(line)
		// perms links owner group size mon day time/year name...
		if len(fields) < 9 {
			continue
		}
		perms := fields[0]
		if len(perms) != 10 || !strings.ContainsRune("-dlbcps", rune(perms[0])) {
			continue
		}
		size, err := strconv.ParseInt(fields[4], 10, 64)
		if err != nil {
			continue
		}
		name := strings.Join(fields[8:], " ")
		if i := strings.Index(name, " -> "); perms[0] == 'l' && i >= 0 {
			name = name[:i]
		}
		if name == "." || name == ".." || name == "" {
			continue
		}
		entries = append(entries, dirEntry{Name: name, IsDir: perms[0] == 'd', Size: size})
	}
	return entries
}

// POST /api/sessions/{id}/list {path} → {entries:[{name,is_dir,size}]}。
// ドライバに ListDir 抽象がないため、一律 `ls -la` を Exec 経由で実行して解析する（Windows の webshell ではドライバエラーになる）。
func (s *Server) intranetListDir(w http.ResponseWriter, r *http.Request) {
	if !s.intranetSessionsReady(w) {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var a struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		writeErr(w, 400, "リクエストボディの JSON 解析に失敗しました: "+err.Error())
		return
	}
	a.Path = strings.TrimSpace(a.Path)
	if a.Path == "" {
		writeErr(w, 400, "path は必須です")
		return
	}
	sess, err := s.lookupLiveSession(id)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	stdout, _, err := sess.Exec(r.Context(), "ls -la "+shellQuote(a.Path), 15*time.Second)
	s.sessStore.Touch(r.Context(), id)
	if err != nil {
		writeErr(w, 502, "ディレクトリ一覧の取得に失敗しました: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"entries": parseLsLa(stdout)})
}

// POST /api/sessions/{id}/read {path} → {content?|content_base64?, truncated}。
func (s *Server) intranetReadFile(w http.ResponseWriter, r *http.Request) {
	if !s.intranetSessionsReady(w) {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var a struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		writeErr(w, 400, "リクエストボディの JSON 解析に失敗しました: "+err.Error())
		return
	}
	a.Path = strings.TrimSpace(a.Path)
	if a.Path == "" {
		writeErr(w, 400, "path は必須です")
		return
	}
	sess, err := s.lookupLiveSession(id)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	data, err := sess.ReadFile(r.Context(), a.Path)
	s.sessStore.Touch(r.Context(), id)
	if err != nil {
		writeErr(w, 502, "読み込みに失敗しました: "+err.Error())
		return
	}
	s.auditIntranet(r, "read session=%d path=%q size=%dB", id, a.Path, len(data))
	out := map[string]any{"size": len(data)}
	truncated := false
	if len(data) > maxSessionToolOutput {
		data = data[:maxSessionToolOutput]
		truncated = true
	}
	if utf8.Valid(data) {
		out["content"] = string(data)
	} else {
		out["content_base64"] = base64.StdEncoding.EncodeToString(data)
	}
	if truncated {
		out["truncated"] = true
	}
	writeJSON(w, 200, out)
}

// POST /api/sessions/{id}/write {path, content_base64} → {ok, bytes}。
// 高リスクの書き込み操作:成否にかかわらず監査ログを記録する。
func (s *Server) intranetWriteFile(w http.ResponseWriter, r *http.Request) {
	if !s.intranetSessionsReady(w) {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var a struct {
		Path          string `json:"path"`
		ContentBase64 string `json:"content_base64"`
	}
	if err := json.NewDecoder(r.Body).Decode(&a); err != nil {
		writeErr(w, 400, "リクエストボディの JSON 解析に失敗しました: "+err.Error())
		return
	}
	a.Path = strings.TrimSpace(a.Path)
	if a.Path == "" {
		writeErr(w, 400, "path は必須です")
		return
	}
	data, err := base64.StdEncoding.DecodeString(a.ContentBase64)
	if err != nil {
		writeErr(w, 400, "content_base64 のデコードに失敗しました: "+err.Error())
		return
	}
	sess, err := s.lookupLiveSession(id)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	if err := sess.WriteFile(r.Context(), a.Path, data); err != nil {
		s.auditIntranet(r, "write session=%d path=%q bytes=%d 失敗: %v", id, a.Path, len(data), err)
		writeErr(w, 502, "書き込みに失敗しました: "+err.Error())
		return
	}
	s.sessStore.Touch(r.Context(), id)
	s.auditIntranet(r, "write session=%d path=%q bytes=%d 成功", id, a.Path, len(data))
	writeJSON(w, 200, map[string]any{"ok": true, "bytes": len(data)})
}

// DELETE /api/sessions/{id} — 閉じてレジストリ/DB から削除する（冪等:存在しなくても ok）。
func (s *Server) intranetDeleteSession(w http.ResponseWriter, r *http.Request) {
	if !s.intranetSessionsReady(w) {
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	inReg := s.sessReg.Remove(id) // Remove が内部でセッションを閉じる
	if err := s.sessStore.Delete(r.Context(), id); err != nil {
		writeErr(w, 500, "セッションの削除に失敗しました: "+err.Error())
		return
	}
	s.auditIntranet(r, "delete session=%d in_registry=%v", id, inReg)
	writeJSON(w, 200, map[string]any{"ok": true, "id": id, "was_live": inReg})
}

// GET /api/tunnels?task_id= — 台帳の全量（stopped を含む、全フィールド）;
// task_id を渡すと本タスクのトンネルだけを残し、渡さなければ全体ビューを維持する（旧フロントとの互換）。
// 注意：deploy_params にはトンネルの auth token（プラットフォームが生成するワンタイムトークン）が含まれ、JWT 通過後のみ参照できる。
func (s *Server) intranetListTunnels(w http.ResponseWriter, r *http.Request) {
	if s.m == nil || s.m.pg == nil {
		writeErr(w, 503, "トンネル台帳が未準備です（DB 未接続）")
		return
	}
	taskID, ok := parseTaskIDQuery(w, r)
	if !ok {
		return
	}
	store := db.NewTunnelStore(s.m.pg)
	var recs []*db.TunnelRecord
	var err error
	if taskID > 0 {
		recs, err = store.ListByTask(r.Context(), taskID)
	} else {
		recs, err = store.List(r.Context())
	}
	if err != nil {
		writeErr(w, 500, "トンネル台帳の読み込みに失敗しました: "+err.Error())
		return
	}
	items := []*db.TunnelRecord{}
	items = append(items, recs...)
	writeJSON(w, 200, map[string]any{"items": items})
}

// POST /api/tunnels/{id}/teardown — トンネルを回収する（tunnel.Manager 経由、冪等）。
func (s *Server) intranetTeardownTunnel(w http.ResponseWriter, r *http.Request) {
	if s.tunnels == nil {
		writeErr(w, 503, "トンネルサブシステムが未準備です")
		return
	}
	id, err := pathID(r, "id")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.tunnels.Teardown(r.Context(), id); err != nil {
		s.auditIntranet(r, "tunnel_teardown id=%d 失敗: %v", id, err)
		writeErr(w, 502, "回収に失敗しました: "+err.Error())
		return
	}
	s.auditIntranet(r, "tunnel_teardown id=%d 成功", id)
	writeJSON(w, 200, map[string]any{"tunnel_id": id, "state": db.TunnelStopped})
}

// GET /api/credentials?task_id= — タスク横断の認証情報のマスク済み一覧（secret は前後 2 文字ずつのみ表示）;
// task_id を渡すと本タスクの認証情報だけを残し、渡さなければ全体ビューを維持する（旧フロントとの互換）。
// 再利用 score は含まない:採点はタスクのコンテキスト（認証情報×本タスクのアセット）で計算するため、全体一覧にはこの
// コンテキストがない。ページで提案が必要なら agent ツール credential_reuse_plan を使う。
func (s *Server) intranetListCredentials(w http.ResponseWriter, r *http.Request) {
	if s.credStore == nil {
		writeErr(w, 503, "認証情報サブシステムが未準備です（DB 未接続）")
		return
	}
	taskID, ok := parseTaskIDQuery(w, r)
	if !ok {
		return
	}
	var creds []*db.CredentialRecord
	var err error
	if taskID > 0 {
		creds, err = s.credStore.ListByTask(r.Context(), taskID)
	} else {
		creds, err = s.credStore.ListAll(r.Context())
	}
	if err != nil {
		writeErr(w, 500, "認証情報の読み込みに失敗しました: "+err.Error())
		return
	}
	items := []map[string]any{}
	for _, c := range creds {
		items = append(items, map[string]any{
			"id": c.ID, "task_id": c.TaskID, "username": c.Username,
			"cred_type": c.CredType, "secret_masked": db.MaskSecret(c.Secret),
			"domain": c.Domain, "source": c.Source, "verified": c.Verified,
			"host_asset_id": c.HostAssetID, "created_at": c.CreatedAt,
		})
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

// ---------------- トポロジ組み立て（純粋関数、テスト可能） ----------------

type topoNode struct {
	ID            string           `json:"id"` // asset-<id> | sess-<sessionID> | pf-<tunnelID> | platform
	IP            string           `json:"ip"`
	Segment       string           `json:"segment"`
	HasSession    bool             `json:"has_session"`
	AliveSessions int              `json:"alive_sessions"`
	Services      []map[string]any `json:"services"`
	IsCallback    bool             `json:"is_callback"`
}

type topoEdge struct {
	ID    int64  `json:"id"`
	Kind  string `json:"kind"`
	From  string `json:"from"`
	To    string `json:"to"`
	State string `json:"state"`
}

type topoSegment struct {
	Name     string `json:"name"`
	ColorKey string `json:"color_key"` // 安定した着色キー（並び順の連番をプレフィックスにする）
}

// segmentOf はアセットのセグメントを取得する:優先は c_segment 列、空なら IPv4 /24（IPv6 /48）で導出する。
func segmentOf(ip, cSegment string) string {
	if cSegment != "" {
		return cSegment
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ""
	}
	if v4 := parsed.To4(); v4 != nil {
		return (&net.IPNet{IP: v4.Mask(net.CIDRMask(24, 32)), Mask: net.CIDRMask(24, 32)}).String()
	}
	return (&net.IPNet{IP: parsed.Mask(net.CIDRMask(48, 128)), Mask: net.CIDRMask(48, 128)}).String()
}

// buildTopology は純粋な組み立て：ip アセットと生きたセッションのホストを重複排除して nodes に、トンネル台帳を edges に、
// 出現したセグメントを集約して segments にする。トンネルがある場合は platform の合成ノードを追加して辺の共通起点にする;
// callbackHost が非空なら同一 IP のホストノードにも is_callback を立てる。
func buildTopology(assets []*db.Asset, recs []*db.SessionRecord, tunnels []*db.TunnelRecord, callbackHost string) map[string]any {
	nodes := map[string]*topoNode{} // key: ip
	for _, a := range assets {
		if a == nil || a.IP == "" {
			continue
		}
		n := nodes[a.IP]
		if n == nil {
			n = &topoNode{ID: fmt.Sprintf("asset-%d", a.ID), IP: a.IP, Services: []map[string]any{}}
			nodes[a.IP] = n
		}
		for _, p := range a.OpenPorts {
			if port, ok := p["port"].(float64); ok && port > 0 {
				svc := map[string]any{"port": int(port)}
				if s, ok := p["service"].(string); ok && s != "" {
					svc["service"] = s
				}
				n.Services = append(n.Services, svc)
			}
		}
	}
	// セッションの集約:優先は host_asset_id に対応するアセットの IP;アセット紐付けがない場合は URL の hostname で
	// 合成ノード（sess-<id>）を作り、「セッションがあるホスト」が必ず図に現れるようにする。
	assetIP := map[int64]string{}
	for _, a := range assets {
		if a != nil && a.IP != "" {
			assetIP[a.ID] = a.IP
		}
	}
	sessionHost := map[int64]string{} // session id → ホスト ip/hostname（辺の to に使う）
	for _, rec := range recs {
		host := ""
		if u, err := url.Parse(rec.URL); err == nil {
			host = u.Hostname()
		}
		ip := assetIP[rec.HostAssetID]
		if ip == "" {
			ip = host
		}
		if ip == "" {
			continue
		}
		sessionHost[rec.ID] = ip
		n := nodes[ip]
		if n == nil {
			n = &topoNode{ID: fmt.Sprintf("sess-%d", rec.ID), IP: ip, Services: []map[string]any{}}
			nodes[ip] = n
		}
		n.HasSession = true
		if rec.Status == db.SessionAlive {
			n.AliveSessions++
		}
	}
	// IP/ホスト名 → ノード id:辺の from/to はノード id でなければならず、存在しないノードを指すと G6 で線が描けない。
	nodeIDByIP := map[string]string{}
	for ip, n := range nodes {
		nodeIDByIP[ip] = n.ID
	}
	edges := []topoEdge{}
	for _, t := range tunnels {
		to := ""
		if t.Kind == "portfwd" && t.TargetHost != "" {
			if id, ok := nodeIDByIP[t.TargetHost]; ok {
				to = id // 対象は図上に既にあるホストなので、そのまま接続する
			} else {
				// 対象が台帳にない:合成ノード（pf-<tunnel_id>、ラベルは host:port）を作り、辺に必ず着地点を持たせる。
				addr := net.JoinHostPort(t.TargetHost, strconv.Itoa(t.TargetPort))
				n := nodes[addr]
				if n == nil {
					n = &topoNode{ID: fmt.Sprintf("pf-%d", t.ID), IP: addr, Services: []map[string]any{}}
					nodes[addr] = n
					nodeIDByIP[addr] = n.ID
				}
				to = n.ID
			}
		} else if h := sessionHost[t.ViaSessionID]; h != "" {
			to = nodeIDByIP[h] // socks:経由セッションのホストノードに接続する（セッション集約によりノードの存在は保証される）
		}
		// 経由セッションが現在のビューにない（タスクで絞り込まれた/削除済み）場合は辺の着地点がないのでスキップする;台帳ページでは引き続き見える。
		if to == "" {
			continue
		}
		edges = append(edges, topoEdge{ID: t.ID, Kind: t.Kind, From: "platform", To: to, State: t.State})
	}
	nodeList := make([]*topoNode, 0, len(nodes))
	segSet := map[string]bool{}
	for _, n := range nodes {
		n.Segment = segmentOf(n.IP, assetCSegment(assets, n.IP))
		if callbackHost != "" && n.IP == callbackHost {
			n.IsCallback = true
		}
		if n.Segment != "" {
			segSet[n.Segment] = true
		}
		nodeList = append(nodeList, n)
	}
	sort.Slice(nodeList, func(i, j int) bool { return nodeList[i].IP < nodeList[j].IP })
	// プラットフォーム自身の合成ノード:トンネルの辺は一律ここから出る（from="platform"）。ラベルはコールバックアドレスを使い、
	// 未設定時は「プラットフォーム自身」を表示する;辺がある場合だけ作成し、空のトポロジに孤立点を残さない。
	// nodes テーブルには入れない:コールバックアドレスが既存アセットと同一 IP だと、IP での集約に飲み込まれるため。
	if len(edges) > 0 {
		ip := callbackHost
		if ip == "" {
			ip = "プラットフォーム自身"
		}
		nodeList = append(nodeList, &topoNode{ID: "platform", IP: ip, IsCallback: true, Services: []map[string]any{}})
	}
	segs := make([]string, 0, len(segSet))
	for s := range segSet {
		segs = append(segs, s)
	}
	sort.Strings(segs)
	segList := make([]topoSegment, 0, len(segs))
	for i, name := range segs {
		segList = append(segList, topoSegment{Name: name, ColorKey: fmt.Sprintf("seg-%d", i)})
	}
	return map[string]any{"nodes": nodeList, "edges": edges, "segments": segList}
}

// assetCSegment はある IP に対応するアセット行の c_segment を取得する（空文字列は未登録を表す）。
func assetCSegment(assets []*db.Asset, ip string) string {
	for _, a := range assets {
		if a != nil && a.IP == ip {
			return a.CSegment
		}
	}
	return ""
}

// parseTaskIDQuery は任意の ?task_id= を解析する:省略/空は (0, true) を返して全体ビューを表す;
// 不正な値は 400 を返して false を返すので、呼び出し側はそのまま return する。
func parseTaskIDQuery(w http.ResponseWriter, r *http.Request) (int64, bool) {
	v := strings.TrimSpace(r.URL.Query().Get("task_id"))
	if v == "" {
		return 0, true
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "不正な task_id: "+v)
		return 0, false
	}
	return id, true
}

// GET /api/intranet/topology?task_id= — 純粋な組み立て、読み取り専用。
func (s *Server) intranetTopology(w http.ResponseWriter, r *http.Request) {
	if s.m == nil || s.m.pg == nil || s.sessStore == nil {
		writeErr(w, 503, "トポロジデータが未準備です（DB 未接続）")
		return
	}
	taskID, ok := parseTaskIDQuery(w, r)
	if !ok {
		return
	}
	var assets []*db.Asset
	var err error
	if taskID > 0 {
		assets, err = s.m.pg.Assets().QueryByTask(taskID, "ip", 5000, 0)
	} else {
		assets, err = s.m.pg.Assets().QueryByType("ip", 5000, 0)
	}
	if err != nil {
		writeErr(w, 500, "アセットの読み込みに失敗しました: "+err.Error())
		return
	}
	recs, err := s.sessStore.List(r.Context(), "")
	if err != nil {
		writeErr(w, 500, "セッション台帳の読み込みに失敗しました: "+err.Error())
		return
	}
	if taskID > 0 { // タスクビューでは本タスクが作成したセッションだけを残し、図の混線を避ける
		filtered := recs[:0]
		for _, rec := range recs {
			if rec.CreatedByTask == taskID {
				filtered = append(filtered, rec)
			}
		}
		recs = filtered
	}
	tunnels, err := db.NewTunnelStore(s.m.pg).List(r.Context())
	if err != nil {
		writeErr(w, 500, "トンネル台帳の読み込みに失敗しました: "+err.Error())
		return
	}
	if taskID > 0 {
		filtered := tunnels[:0]
		for _, t := range tunnels {
			if t.TaskID == taskID {
				filtered = append(filtered, t)
			}
		}
		tunnels = filtered
	}
	cb, _ := callbackHost() // コールバックアドレス未設定時は is_callback がすべて false
	writeJSON(w, 200, buildTopology(assets, recs, tunnels, cb))
}

// taskIntranet はタスクがイントラネット期にあるか判定する（フェーズ 4、worker.intranet プロンプト変種のスイッチ）:
// そのタスクの alive なトンネル、またはそのタスクが作成した存続中のフットホールドセッションが存在する場合。
// worker は run のたびに照会するので、トンネル/セッションの状態変化は即座に反映される;読み込み失敗時は外部ネットワーク扱いにする（変種を誤って切り替えない）。
func (s *Server) taskIntranet(taskID int64) bool {
	if taskID <= 0 {
		return false
	}
	if s.tunnels != nil {
		if recs, err := s.tunnels.TaskRecords(s.ctx, taskID); err == nil {
			for _, r := range recs {
				if r.State == db.TunnelAlive {
					return true
				}
			}
		}
	}
	if s.sessStore != nil {
		if recs, err := s.sessStore.List(s.ctx, db.SessionAlive); err == nil {
			for _, r := range recs {
				if r.CreatedByTask == taskID {
					return true
				}
			}
		}
	}
	return false
}
