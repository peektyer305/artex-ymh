// Package session はフットホールドのセッションサブシステム(イントラネット侵入フェーズ 1a、INTRANET-PIVOT-DESIGN.md
// §4.2 参照)。すべてのドライバ(http_shell のワンライナー WebShell、ssh/reverse は予約)が同じ Session 抽象を実装し、
// agent から見えるのは「セッションツール」だけで、目標側コマンドの唯一の出口は Session.Exec/ReadFile/WriteFile。
//
// 設計の意味論は PivotHub pivothub/session を参考にした(Go での書き直しであり、流用ではない):
//   - センチネル同期: ランダム marker で stdout を包み、エコーの汚染/癒着/誤ヒットを防ぐ。
//   - クォート回避: 複雑なコマンドは base64 で目標の一時ファイルに落としてから sh で実行し、一時ファイルはランダム接頭辞で
//     使い終わったら削除する。
//   - 切断の誠実さ: disable_functions / WAF ブロックは一律で意味のあるエラーを返し、成功を偽装しない。
package session

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Session はすべてのフットホールドセッションの統一契約。実装は並行安全でなければならない(同一セッションが複数の
// worker の意図から同時に使われうる。HTTP の WebShell はステートレスなので元々並行可能)。
type Session interface {
	// ID は sessions テーブルの主キー。DB 保存前は 0。
	ID() int64
	// Kind はセッション種別: http_php / http_jsp / http_aspx / ssh / reverse(予約)。
	Kind() string
	// Test は疎通確認(無害なプローブ、例: echo センチネル)を行う。失敗時は原因付きのエラーを返す。
	Test(ctx context.Context) error
	// Exec は目標側で shell コマンドを 1 つ実行する。webshell チャネルでは stderr を分離できないため、
	// 実装は一律で 2>&1 を stdout にマージし、stderr は常に空(区別できるふりをしない誠実な意味論)。
	// timeout <= 0 のときはドライバのデフォルトタイムアウトを使う。ネットワーク失敗/実行関数の無効化/ブロックはいずれも err で返す。
	Exec(ctx context.Context, cmd string, timeout time.Duration) (stdout, stderr string, err error)
	// ReadFile は目標側のファイルを読む(大きなファイルは内部で分割)。
	ReadFile(ctx context.Context, path string) ([]byte, error)
	// WriteFile は目標側のファイルを書く(バイナリ安全、大きなファイルは内部で分割 append)。
	WriteFile(ctx context.Context, path string, data []byte) error
	// Close はリソースを解放する(HTTP セッションはリソースなし、reverse は予約)。
	Close(ctx context.Context) error
}

// Error はセッション層のエラー: ネットワーク失敗/プロトコル失敗/目標が非対応。テキストは agent 向けで、意味を持つ
// (何が原因か、どのブロックの可能性があるか)。成功を偽装しない。
type Error struct {
	Op  string // test/exec/read/write/probe
	Err string
}

func (e *Error) Error() string { return fmt.Sprintf("session %s: %s", e.Op, e.Err) }

func opError(op, format string, args ...any) error {
	return &Error{Op: op, Err: fmt.Sprintf(format, args...)}
}

// ErrSessionNotFound は Registry にセッションが見つからない場合のエラー。
var ErrSessionNotFound = errors.New("session が存在しないか、すでにクローズ済み")

// ---------------- 呼び出し側の名前空間接頭辞(振り返り R5) ----------------

// ctxTmpPrefixKey は目標側一時ファイルの名前空間接頭辞を保持する context キー。
type ctxTmpPrefixKey struct{}

// defaultTmpPrefix は目標側一時ファイルのデフォルト接頭辞(呼び出し側の接頭辞が未注入のとき)。
const defaultTmpPrefix = "/tmp/.artex_"

// WorkerTmpPrefix は per-worker の名前空間接頭辞を組み立てる: /tmp/.artex_<task>_<intent>_。
// 複数の worker が同じフットホールドを共有しても、それぞれの書き出しスクリプト/マーカーファイルが上書きし合わない(振り返り R5:
// 共有パスのハードコードにより出力が混線し、センチネルが「WebShell 失効」と誤報した)。
func WorkerTmpPrefix(taskID, intentID int64) string {
	return fmt.Sprintf("/tmp/.artex_%d_%d_", taskID, intentID)
}

// WithTmpPrefix は ctx に目標側一時ファイルの名前空間接頭辞を付ける(server 層が worker ごとに注入)。
// prefix は絶対パス形式のみを受け付け、文字集合は [A-Za-z0-9_./-] に限る(sh コマンドにインライン展開するため、
// クォート/空白/セミコロンなどのメタ文字は拒否する)。不正な入力はデフォルト接頭辞にフォールバックする。
func WithTmpPrefix(ctx context.Context, prefix string) context.Context {
	return context.WithValue(ctx, ctxTmpPrefixKey{}, sanitizeTmpPrefix(prefix))
}

// tmpPrefixFrom は呼び出し側の接頭辞を取得し、未注入ならデフォルト接頭辞を使う。
func tmpPrefixFrom(ctx context.Context) string {
	if v, ok := ctx.Value(ctxTmpPrefixKey{}).(string); ok && v != "" {
		return v
	}
	return defaultTmpPrefix
}

func sanitizeTmpPrefix(p string) string {
	p = strings.TrimSpace(p)
	if !strings.HasPrefix(p, "/") {
		return defaultTmpPrefix
	}
	for _, r := range p {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '_', r == '-', r == '.', r == '/':
		default:
			return defaultTmpPrefix
		}
	}
	return p
}

// Registry はプロセス内のセッションレジストリ: sessions テーブルの id → 生きたセッション。並行安全。起動時に
// server が DB から復元して読み込む(restore_reverse_listeners のパターン)。
type Registry struct {
	mu       sync.RWMutex
	sessions map[int64]Session
}

func NewRegistry() *Registry {
	return &Registry{sessions: map[int64]Session{}}
}

// Add はセッションを登録する(同じ id は上書きし、古いセッションをクローズする)。
func (r *Registry) Add(s Session) {
	r.mu.Lock()
	old := r.sessions[s.ID()]
	r.sessions[s.ID()] = s
	r.mu.Unlock()
	if old != nil && old != s {
		_ = old.Close(context.Background())
	}
}

// Get はセッションを取得する。
func (r *Registry) Get(id int64) (Session, bool) {
	r.mu.RLock()
	s, ok := r.sessions[id]
	r.mu.RUnlock()
	return s, ok
}

// Remove はセッションを削除してクローズする。
func (r *Registry) Remove(id int64) bool {
	r.mu.Lock()
	s, ok := r.sessions[id]
	delete(r.sessions, id)
	r.mu.Unlock()
	if ok {
		_ = s.Close(context.Background())
	}
	return ok
}

// List はすべての生きたセッションを返す(順序は不定)。
func (r *Registry) List() []Session {
	r.mu.RLock()
	out := make([]Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, s)
	}
	r.mu.RUnlock()
	return out
}
