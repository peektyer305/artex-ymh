// stage は管理されたファイル受け渡し用の一時保管サービス（F13。アーキテクチャ原則は INTRANET-PIVOT-DESIGN.md 4.3a、
// セマンティクスは PivotHub filestage を参照）：worker が「ワークスペース内で生成済みのツール/payload」をプラットフォームに渡し、
// プラットフォームは 24 バイトのランダムトークンのパス（/s/<token>/<name>）でダウンロードを提供する。ディレクトリ列挙なし、厳格な TTL
// （デフォルト 15 分、上限 2h、期限切れで自動破棄）、任意でワンショット消去、全行程を記録
// （Put/ダウンロード/期限切れ/破棄）。ダウンロードパスは JWT 不要（対象マシンはトークンを持たない）で、ランダムトークン
// + トークンごとのレート制限により列挙を防ぐ。誤ったトークンと存在しないパスの応答はバイト単位で一致する。
//
// 境界：一時保管は【外部へのツール受け渡し】にのみ使う。戦利品/認証情報の還流はすべてセッション読み取り/evidence を通し、
// 制度的に、一時保管サービスが突破されても漏えいする機密データがないことを保証する。
package stage

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// DefaultTTL は明示指定がない場合の生存期間。MaxTTL は厳格な上限——一時保管はストレージではないので、
	// 期限超過は即オーファン（python -m http.server を止め忘れる定番の失敗もここに由来する）。
	DefaultTTL = 15 * time.Minute
	MaxTTL     = 2 * time.Hour
	minTTL     = time.Minute

	tokenBytes = 24 // crypto/rand → base64url で 32 文字

	gcInterval = time.Minute

	// ダウンロードの列挙防止レート制限：単一トークンあたり毎分の試行上限 + 単一ソース IP あたり毎分の試行上限。
	// 制限に掛かった場合と「トークンが存在しない」場合の応答は一致する（同じ 404）。列挙する側に一切の手掛かりを与えない。
	tokenRateLimit  = 10
	ipRateLimit     = 60
	rateLimitWindow = time.Minute
)

// PutOpts は 1 回の一時保管のオプション。TTL<=0 なら DefaultTTL、MaxTTL を超える場合は MaxTTL に丸められる。
type PutOpts struct {
	TTL     time.Duration
	OneShot bool   // 初回の完全なダウンロード成功後に消去
	TaskID  string // 所属タスク（activity 記録用。空 = タスクコンテキストなし）
}

// Entry は一時保管レコードの外部向けビュー。
type Entry struct {
	Token     string    `json:"token"`
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	URL       string    `json:"url"` // baseURL が既知なら絶対アドレス、そうでなければ /s/ 相対パス
	ExpiresAt time.Time `json:"expires_at"`
	OneShot   bool      `json:"one_shot"`
	TaskID    string    `json:"task_id,omitempty"`
	Hits      int64     `json:"hits"`         // 完全なダウンロードに成功した回数
	Bytes     int64     `json:"bytes_served"` // 実際に書き出した累計バイト数（不完全なダウンロードを含む）
	CreatedAt time.Time `json:"created_at"`
}

// Event は 1 件の記録イベント（Put/ダウンロード/期限切れ/破棄）。OnEvent コールバック経由でホストに渡し、
// activity / ログとして書き込まれる。Kind の値は EventKind* 定数を参照。
type Event struct {
	Kind   string // put | download | expire | destroy
	Entry  Entry
	Remote string // ダウンロード元 IP（download のみ）
	Bytes  int64  // 今回書き出したバイト数（download のみ）
}

const (
	EventPut      = "put"
	EventDownload = "download"
	EventExpire   = "expire"
	EventDestroy  = "destroy"
)

// Manager はプロセス内の一時保管マネージャ。エントリは本プロセスのメモリ内にのみ存在する——再起動後は data/stage/
// 配下に残ったファイルの台帳がないため、New 時に全体を消去する（期限切れリソースをプロセスを跨いで延命させない）。
type Manager struct {
	dir string

	mu      sync.Mutex
	entries map[string]*Entry

	baseURL string // 任意の絶対ベースアドレス（例 http://10.0.0.2:8787）。Entry.URL の形を決める

	// OnEvent が非 nil なら、記録イベントごとに 1 回同期でコールバックする。コールバックは高速であること（ロック外で呼ぶが、
	// 遅いコールバックは呼び出し元 goroutine を引きずる）。activity / ログの書き込みはホスト側で実装する。
	OnEvent func(Event)

	rateMu   sync.Mutex
	tokenHit map[string][]time.Time // token → 直近のダウンロード試行時刻
	ipHit    map[string][]time.Time

	stopGC context.CancelFunc
}

// New はマネージャを作成する：ディレクトリ 0700、前プロセスの残留ファイルを消去、定期クリーンアップ goroutine を起動
// （ctx のキャンセルで終了しタイマーを停止）。
func New(ctx context.Context, dir string) (*Manager, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("stage dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil { // 既存ディレクトリは権限が緩い可能性がある
		return nil, fmt.Errorf("stage dir chmod: %w", err)
	}
	// 台帳はプロセス内のものなので再起動で全て無効になる——残留ファイルに存在理由はない。消す。
	if errs := clearDir(dir); errs != nil {
		return nil, fmt.Errorf("stage dir sweep: %w", errs[0])
	}
	m := &Manager{
		dir:      dir,
		entries:  map[string]*Entry{},
		tokenHit: map[string][]time.Time{},
		ipHit:    map[string][]time.Time{},
	}
	gcCtx, cancel := context.WithCancel(ctx)
	m.stopGC = cancel
	go m.gcLoop(gcCtx)
	return m, nil
}

// Close はバックグラウンドのクリーンアップ goroutine を停止する（未期限切れのエントリは削除せず、プロセス終了で台帳が無効になる）。
func (m *Manager) Close() {
	if m.stopGC != nil {
		m.stopGC()
	}
}

// SetBaseURL は外部向け絶対ベースアドレスを設定する（Entry.URL とツールの戻り値に組み込まれる）。空 = 相対パスのみを返し、
// ホスト名は呼び出し元が補う（対象マシンからプラットフォームへの到達可能アドレスはプラットフォーム側では権威的に知り得ない）。
func (m *Manager) SetBaseURL(u string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.baseURL = strings.TrimRight(u, "/")
}

// PutFile はディスク上に既にあるファイルを一時保管する（ストリームコピーでメモリを食わない）。name はソースファイルの basename。
func (m *Manager) PutFile(srcPath string, o PutOpts) (*Entry, error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return nil, fmt.Errorf("open source: %w", err)
	}
	defer src.Close()
	st, err := src.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("not a regular file: %s", srcPath)
	}
	name := filepath.Base(srcPath)
	return m.put(name, src, o)
}

// PutContent はメモリ上の内容を一時保管する。name は必須（basename を取り、パス脱出を防ぐ）。
func (m *Manager) PutContent(name string, data []byte, o PutOpts) (*Entry, error) {
	return m.put(name, bytes.NewReader(data), o)
}

func (m *Manager) put(name string, r io.Reader, o PutOpts) (*Entry, error) {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "" || name == "." || name == string(filepath.Separator) {
		return nil, errors.New("invalid file name")
	}
	ttl := o.TTL
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if ttl < minTTL {
		ttl = minTTL
	}
	if ttl > MaxTTL {
		ttl = MaxTTL
	}
	token, err := newToken()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(m.dir, token)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, err
	}
	size, copyErr := io.Copy(f, r)
	closeErr := f.Close()
	if copyErr != nil || closeErr != nil {
		_ = os.RemoveAll(dir)
		if copyErr != nil {
			return nil, copyErr
		}
		return nil, closeErr
	}
	now := time.Now()
	e := &Entry{
		Token:     token,
		Name:      name,
		Size:      size,
		ExpiresAt: now.Add(ttl),
		OneShot:   o.OneShot,
		TaskID:    o.TaskID,
		CreatedAt: now,
	}
	m.mu.Lock()
	m.entries[token] = e
	e.URL = m.urlLocked(e)
	m.mu.Unlock()
	m.emit(Event{Kind: EventPut, Entry: *e})
	return e, nil
}

// List は現在生存しているエントリを作成日時の降順で返す（管理 API 用）。
func (m *Manager) List() []Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Entry, 0, len(m.entries))
	for _, e := range m.entries {
		out = append(out, *e)
	}
	// 単純な挿入ソートで十分——一時保管エントリはごく少数。
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].CreatedAt.After(out[j-1].CreatedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// Delete はエントリを前倒しで破棄する（管理 API）。ヒットしたかどうかを返す。
func (m *Manager) Delete(token string) bool {
	e := m.remove(token, EventDestroy)
	return e != nil
}

// Download はダウンロードを 1 回提供する（GET /s/{token}/{name} にマウント）。列挙防止の要点：
// token が存在しない / name 不一致 / 期限切れ / レート制限超過——すべて同じ 404 を返す。
func (m *Manager) Download(w http.ResponseWriter, r *http.Request, token, name string) {
	ip := remoteIP(r)
	if !m.allowAttempt(token, ip) {
		notFound(w)
		return
	}
	m.mu.Lock()
	e, ok := m.entries[token]
	if !ok || e.Name != name || time.Now().After(e.ExpiresAt) {
		m.mu.Unlock()
		notFound(w)
		return
	}
	m.mu.Unlock()

	path := filepath.Join(m.dir, token, name)
	f, err := os.Open(path)
	if err != nil {
		notFound(w)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", e.Name))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", e.Size))
	n, _ := io.Copy(w, f)
	// defer ではなく明示的に閉じる：ワンショット消去では直後にファイルを削除するが、Windows では開いたままのハンドルを削除できない。
	_ = f.Close()

	m.mu.Lock()
	cur, alive := m.entries[token]
	if alive {
		cur.Bytes += n
	}
	complete := n == e.Size
	if alive && complete {
		cur.Hits++
	}
	snap := e
	oneShotBurn := alive && complete && e.OneShot
	m.mu.Unlock()

	m.emit(Event{Kind: EventDownload, Entry: *snap, Remote: ip, Bytes: n})
	if oneShotBurn {
		// ワンショット消去：初回の完全なダウンロード成功後に即破棄する（記録上の Hits は先に反映済み）。
		m.remove(token, EventDestroy)
	}
}

// remove はエントリを外してファイルを削除し、削除したエントリを返す（nil = 存在しない）。kind は記録イベントの種別。
func (m *Manager) remove(token, kind string) *Entry {
	m.mu.Lock()
	e, ok := m.entries[token]
	if ok {
		delete(m.entries, token)
	}
	m.mu.Unlock()
	if !ok {
		return nil
	}
	_ = os.RemoveAll(filepath.Join(m.dir, token))
	m.emit(Event{Kind: kind, Entry: *e})
	return e
}

func (m *Manager) gcLoop(ctx context.Context) {
	t := time.NewTicker(gcInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			m.sweep(now)
		}
	}
}

// sweep は期限切れのエントリをすべて破棄する。テストから直接呼べるよう切り出してある。
func (m *Manager) sweep(now time.Time) {
	m.mu.Lock()
	var expired []string
	for token, e := range m.entries {
		if now.After(e.ExpiresAt) {
			expired = append(expired, token)
		}
	}
	m.mu.Unlock()
	for _, token := range expired {
		m.remove(token, EventExpire)
	}
}

// allowAttempt はダウンロード試行のレート制限を実装する：単一トークンと単一ソース IP の 2 軸のスライディングウィンドウ。
// 期限切れのウィンドウはついでに掃除するので、map が無限に膨らむことはない。
func (m *Manager) allowAttempt(token, ip string) bool {
	now := time.Now()
	cutoff := now.Add(-rateLimitWindow)
	m.rateMu.Lock()
	defer m.rateMu.Unlock()
	if len(m.tokenHit) > 4096 { // メモリ爆発の防止：極端な場合はいったん全体をリセットする
		m.tokenHit = map[string][]time.Time{}
	}
	if len(m.ipHit) > 4096 {
		m.ipHit = map[string][]time.Time{}
	}
	th := append(prune(m.tokenHit[token], cutoff), now)
	m.tokenHit[token] = th
	ih := append(prune(m.ipHit[ip], cutoff), now)
	m.ipHit[ip] = ih
	return len(th) <= tokenRateLimit && len(ih) <= ipRateLimit
}

func prune(ts []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(ts) && ts[i].Before(cutoff) {
		i++
	}
	return ts[i:]
}

func (m *Manager) emit(ev Event) {
	if m.OnEvent != nil {
		m.OnEvent(ev)
	}
}

func (m *Manager) urlLocked(e *Entry) string {
	p := "/s/" + e.Token + "/" + urlPathEscape(e.Name)
	if m.baseURL == "" {
		return p
	}
	return m.baseURL + p
}

func newToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// urlPathEscape はファイル名を URL パスセグメント向けにエスケープする（/ と % は必ずエンコードする）。
func urlPathEscape(name string) string {
	r := strings.NewReplacer("%", "%25", "/", "%2F", "?", "%3F", "#", "%23", " ", "%20")
	return r.Replace(name)
}

func notFound(w http.ResponseWriter) {
	// バイト単位で一致する 404：誤ったトークン、誤った name、期限切れ、レート制限超過はすべてここを通る。
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusNotFound)
	_, _ = io.WriteString(w, "404 page not found\n")
}

func remoteIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// clearDir は dir 配下の全内容を削除する（dir 自体は残す）。エラーは 1 件ずつ収集する。
func clearDir(dir string) []error {
	items, err := os.ReadDir(dir)
	if err != nil {
		return []error{err}
	}
	var errs []error
	for _, it := range items {
		if err := os.RemoveAll(filepath.Join(dir, it.Name())); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}
