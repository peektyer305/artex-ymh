// Package honeydetect はバッチ 6 L1 のハニーポット静的シグネチャ識別層
// （設計ドキュメント docs/architecture/honeypot-detection.md 二·L1）。service アセットの
// 既知の静的特徴（banner/HTTP タイトル/本文/TLS 証明書）を部分一致で
// シグネチャ照合し、0-1 のハニーポット信頼度スコアとヒットのエビデンスを
// 返す。シグネチャライブラリは既定で本パッケージに埋め込み（go:embed
// signatures.json）。server 起動時に dataDir/signatures/honeypot.json が
// あればそれを優先し mtime でホットリロードする（ディスクに書かずに
// シグネチャを運用できる）。なければ埋め込みライブラリを使い、空にはならない。
// シグネチャは公開ソースのあるものだけを収録する（出典は各シグネチャの source フィールド）。
package honeydetect

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
)

// embeddedSignatures は埋め込みシグネチャライブラリ（signatures.json と同一
// ファイル）で、シグネチャファイル欠落時のフォールバック。どのデプロイ形態でも
// シグネチャ識別が使え、dataDir のシードコピーに依存しない。
//
//go:embed signatures.json
var embeddedSignatures []byte

// ServiceView は 1 回のシグネチャ評価の入力ビュー。呼び出し側が手元にある
// フィールドを埋め、なければ空のままにする。
type ServiceView struct {
	Port        int    // サービスポート（0=不明）
	Banner      string // プロトコル banner / HTTP Server ヘッダ等
	Title       string // HTTP <title>
	Body        string // HTTP 本文（またはその断片）
	CertSubject string // TLS 証明書 subject
	CertIssuer  string // TLS 証明書 issuer
}

// MatchRule は 1 条のシグネチャのマッチ条件。列挙した条件が【すべて】命中した
// ときのみ命中とし、条件が 1 つもないシグネチャは決して命中しない（無条件の
// シグネチャがすべてにマッチするのを防ぐ）。
type MatchRule struct {
	Port              *int   `json:"port,omitempty"`
	BannerSubstr      string `json:"banner_substr,omitempty"`
	HTTPTitleSubstr   string `json:"http_title_substr,omitempty"`
	HTTPBodySubstr    string `json:"http_body_substr,omitempty"`
	CertSubjectSubstr string `json:"cert_subject_substr,omitempty"`
	CertIssuerSubstr  string `json:"cert_issuer_substr,omitempty"`
}

// Signature は 1 条のハニーポット静的シグネチャ。
type Signature struct {
	Name       string    `json:"name"`       // シグネチャ識別子（snake_case）
	Product    string    `json:"product"`    // ハニーポット製品名（Cowrie/OpenCanary/...）
	Confidence float64   `json:"confidence"` // 0-1。命中時に寄与するスコア
	Source     string    `json:"source"`     // ソース（必須。出典のあるものだけ収録）
	Match      MatchRule `json:"match"`
}

func (m MatchRule) empty() bool {
	return m.Port == nil && m.BannerSubstr == "" && m.HTTPTitleSubstr == "" &&
		m.HTTPBodySubstr == "" && m.CertSubjectSubstr == "" && m.CertIssuerSubstr == ""
}

// matches はビュー v がこのルールに命中するか（列挙した条件がすべて命中）を
// 報告する。部分一致は大文字小文字を区別しない（banner/title/証明書の表記は
// 実装ごとに揺れるため、シグネチャは安定した部分文字列のみを取る）。
func (m MatchRule) matches(v ServiceView) bool {
	if m.empty() {
		return false
	}
	if m.Port != nil && *m.Port != v.Port {
		return false
	}
	if m.BannerSubstr != "" && !containsFold(v.Banner, m.BannerSubstr) {
		return false
	}
	if m.HTTPTitleSubstr != "" && !containsFold(v.Title, m.HTTPTitleSubstr) {
		return false
	}
	if m.HTTPBodySubstr != "" && !containsFold(v.Body, m.HTTPBodySubstr) {
		return false
	}
	if m.CertSubjectSubstr != "" && !containsFold(v.CertSubject, m.CertSubjectSubstr) {
		return false
	}
	if m.CertIssuerSubstr != "" && !containsFold(v.CertIssuer, m.CertIssuerSubstr) {
		return false
	}
	return true
}

func containsFold(haystack, needle string) bool {
	return haystack != "" && strings.Contains(strings.ToLower(haystack), strings.ToLower(needle))
}

// Engine はシグネチャエンジン。シグネチャファイルのパスを保持し、mtime で
// キャッシュを再読み込みする（ホットリロード）。
type Engine struct {
	mu      sync.RWMutex
	path    string
	modTime time.Time
	sigs    []Signature
}

// NewEngine は指定したシグネチャファイルパスでエンジンを作る。パスの欠落や
// 破損でもエラーにせず、Evaluate は 0 点を返す。
func NewEngine(path string) *Engine { return &Engine{path: path} }

// signatures は現在有効なシグネチャを返す。ファイルがあれば mtime でホット
// リロードし、欠落/読み取り不可/解析失敗時は埋め込みライブラリにフォールバック
// する（解析は 1 回だけ、以降はキャッシュに当たる）。
func (e *Engine) signatures() []Signature {
	fi, err := os.Stat(e.path)
	if err != nil {
		e.mu.Lock()
		if e.sigs == nil {
			e.sigs = parseSignatures(embeddedSignatures)
		}
		e.modTime = time.Time{}
		sigs := e.sigs
		e.mu.Unlock()
		return sigs
	}
	e.mu.RLock()
	if !e.needsReload(fi.ModTime()) {
		sigs := e.sigs
		e.mu.RUnlock()
		return sigs
	}
	e.mu.RUnlock()

	e.mu.Lock()
	defer e.mu.Unlock()
	sigs := parseSignatures(mustRead(e.path))
	if sigs == nil {
		sigs = parseSignatures(embeddedSignatures)
	}
	e.sigs, e.modTime = sigs, fi.ModTime()
	return e.sigs
}

func mustRead(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return data
}

// parseSignatures はシグネチャライブラリの JSON を解析する。解析失敗時は nil
// を返す（呼び出し側が独自にフォールバックする）。
func parseSignatures(data []byte) []Signature {
	if len(data) == 0 {
		return nil
	}
	var file struct {
		Signatures []Signature `json:"signatures"`
	}
	if json.Unmarshal(data, &file) != nil {
		return nil
	}
	return file.Signatures
}

// needsReload はキャッシュしたシグネチャがこの mtime より古いかを報告する
// （読み取りロック内の高速パス）。
func (e *Engine) needsReload(mt time.Time) bool { return !mt.Equal(e.modTime) }

// Evaluate はビュー v に対して全シグネチャを実行し、命中した中の最高信頼度と
// エビデンス（各件 "name(product, confidence)"）を返す。命中なしなら 0 点、
// エビデンスは空。
func (e *Engine) Evaluate(v ServiceView) (score float64, evidence []string) {
	for _, s := range e.signatures() {
		if s.Confidence <= 0 || !s.Match.matches(v) {
			continue
		}
		evidence = append(evidence, fmt.Sprintf("%s(%s, %.2f)", s.Name, s.Product, s.Confidence))
		if s.Confidence > score {
			score = s.Confidence
		}
	}
	return score, evidence
}

// defaultSignaturesPath は既定のシグネチャファイル相対パス（開発時はリポジトリ
// ルートから実行すれば有効）。server 起動時に dataDir の絶対パスを
// SetSignaturesPath で上書きする。
const defaultSignaturesPath = "data/signatures/honeypot.json"

var defaultEngine = NewEngine(defaultSignaturesPath)

// SetSignaturesPath は既定エンジンのシグネチャファイルパスを上書きし、キャッシュ
// を空にする（server 起動時に組み立てる）。
func SetSignaturesPath(path string) {
	defaultEngine.mu.Lock()
	defaultEngine.path = path
	defaultEngine.sigs = nil
	defaultEngine.modTime = time.Time{}
	defaultEngine.mu.Unlock()
}

// Evaluate は既定エンジンで評価する（Engine.Evaluate を参照）。
func Evaluate(v ServiceView) (score float64, evidence []string) {
	return defaultEngine.Evaluate(v)
}
