package server

// バッチ 5 B2(設計ドキュメント HONEYPOT-DETECTION-DESIGN.md 二-B「自身保護」第 3 条):出口審査の
// フィンガープリント組み立て。EgressGuard はプロセス単位のシングルトン(Manager.egress)で、各タスクの Guard は
// taskFromPG / agentGuard で SetEgress を通じて同じものを参照する——そのためここでは「起動」時
// と「LLM プロファイル変更」時(applyLLM が唯一の経路)に、この 1 つへフィンガープリントを登録/置換するだけで、すべての
// タスク(既存を含む)に即時反映され、タスクごとの注入は不要。登録はハッシュのみを保持し(guard/egress.go 参照)、
// 本ファイルは原文が唯一現れる場所で、ログにもディスクにも残さない。

import (
	"encoding/base64"
	"encoding/hex"
	"net/url"
	"regexp"
	"strings"

	"github.com/Autumn-27/artex/config"
)

// registerEgressFingerprints は現在のプラットフォームの機密情報を共有 EgressGuard へ登録する:
// 有効な LLM プロファイルの API key(変更時は全体を置換)、jwt.key ファイルの内容(原文/hex/base64
// の 3 種のテキスト形態)、gate 入口パス、PG DSN 内のパスワード、config.json の callback_addr。
// 冪等:繰り返し呼んでも重複したフィンガープリントは作られない(guard.AddFingerprint が内部で重複排除)。
func (s *Server) registerEgressFingerprints() {
	if s.m == nil || s.m.egress == nil {
		return
	}
	eg := s.m.egress
	// 有効な LLM プロファイルの API key。プロファイルで key が変わっている可能性があるため、まず種類ごと外してから現在値を登録する。
	eg.RemoveKind("llm_api_key")
	if s.m.pg != nil {
		if p, err := s.m.pg.ActiveProfile(); err == nil && p != nil {
			eg.AddFingerprint("llm_api_key", p.APIKey)
		}
	}
	// jwt.key:32 バイトの署名鍵。コマンドテキスト内では通常、原文/hex/base64 のいずれかで現れる。
	if len(s.jwtKey) > 0 {
		eg.AddFingerprint("jwt_key", string(s.jwtKey))
		eg.AddFingerprint("jwt_key", hex.EncodeToString(s.jwtKey))
		eg.AddFingerprint("jwt_key", base64.StdEncoding.EncodeToString(s.jwtKey))
	}
	// gate 入口パス(対偵察ゲートの隠し入口で、漏れると無効になる)。
	if s.gate != nil {
		eg.AddFingerprint("gate_path", s.gate.path)
	}
	// PG DSN 内のパスワード(env ARTEX_PG_DSN または config.json。起動時の解析と同じソース)。
	if dsn, _, err := config.PostgresDSN(); err == nil {
		eg.AddFingerprint("pg_password", dsnPassword(dsn))
	}
	// config.json の callback_addr(リバースシェル/トンネルのコールバック先)。
	eg.AddFingerprint("callback_addr", config.CallbackAddr())
}

// dsnPassword は DSN からパスワードを取り出す。URL 形態(postgres://user:pass@host/db)
// と keyword 形態(host=... password=...)に対応する。取得できなければ空文字列を返す(登録側がスキップする)。
func dsnPassword(dsn string) string {
	if u, err := url.Parse(dsn); err == nil && u.User != nil {
		if pw, ok := u.User.Password(); ok {
			return pw
		}
	}
	if m := reKeywordPassword.FindStringSubmatch(dsn); m != nil {
		return strings.Trim(m[1], "'")
	}
	return ""
}

// reKeywordPassword は keyword 形態 DSN の password 部分に一致する(値は単一引用符で囲まれていてもよい)。
var reKeywordPassword = regexp.MustCompile(`(?:^|\s)password=('[^']*'|[^\s]+)`)
