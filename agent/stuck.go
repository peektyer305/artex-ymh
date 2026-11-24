package agent

import (
	"fmt"
	"hash/fnv"
	"regexp"
	"strings"
	"unicode/utf8"
)

// chains L1（docs/architecture/chains-integration.md §3 L1 行）：worker ループ内の同質レスポンス停滞検出。
// 振り返りの根拠 docs/operations/postmortem-red-sun-3.md R4：同一のクラッシュ署名（malloc(): memory
// corruption）が十数ターン続いても再試行し、169 拍を使い切った。chains meta の「15〜25 回
// 同質レスポンスなら方向転換」という規則はプロンプト内にしかなく、コードには入っていない。
// 本ファイルはそれを決定的な判定基準にする：純粋関数・I/O なし。呼び出し元（engine の
// steerHooks、PostToolUse 観察点）が各ステップの tool_result を渡す。発火後は steer 注入経路
// （PreToolUse drain）で次のツール呼び出しを中断し、方向転換の指示をモデルに返す。

// StuckConfig は検出パラメータ（デフォルトが本番値。単体テストではしきい値を絞って短縮できる）。
type StuckConfig struct {
	Window         int     // ローリングウィンドウ：直近 N 個の tool_result を判定に使う
	SigPrefix      int     // 正規化した出力の先頭 N 文字を署名ハッシュに入れる
	SameSigTrigger int     // 同一署名が連続 >= N 回 → 発火
	FailTrigger    int     // 同じツールで連続失敗 >= N 回かつ出力が類似 → 発火
	FailSimilarity float64 // 上記の類似度しきい値（0〜1、bigram Dice の平均）
	Cooldown       int     // 発火後 N ステップ（tool_result）は再発火しない
	MaxFires       int     // 検出器ごと（= 意図ごと）の最大発火回数
}

// DefaultStuckConfig は本番デフォルトを返す：同一署名が連続 12 回、または同じツールの連続失敗
// 15 回かつ類似度 >0.8 で「情報の勾配がゼロ」と判定する。発火後は 10 ステップ冷却し、
// 意図ごとに最大 3 回まで。
func DefaultStuckConfig() StuckConfig {
	return StuckConfig{
		Window: 25, SigPrefix: 200,
		SameSigTrigger: 12, FailTrigger: 15, FailSimilarity: 0.8,
		Cooldown: 10, MaxFires: 3,
	}
}

// 正規化の規則（順序に依存）：タイムスタンプ → パス/URL → 長い hex → 長いランダム文字列 →
// 数値 → 空白の畳み込み。目的：同じブロックページ／同じクラッシュ出力に埋め込まれた
// タイムスタンプ、リクエスト id、一時パス、ランダム token が「同質」を「別物」に
// 化けさせないようにする。
var (
	stuckReTimestamp = regexp.MustCompile(`\d{4}[-/]\d{1,2}[-/]\d{1,2}([t ]\d{1,2}:\d{2}(:\d{2})?(\.\d+)?(z|[+-]\d{2}:?\d{2})?)?|\d{1,2}:\d{2}:\d{2}(\.\d+)?`)
	stuckRePath      = regexp.MustCompile(`(?:[a-z]:)?[\w.~+-]*(?:[/\\][\w.~+-]+)+/?`)
	stuckReHex       = regexp.MustCompile(`\b[0-9a-f]{8,}\b`)
	stuckReRand      = regexp.MustCompile(`\b[\w+-]{24,}\b`)
	stuckReDigits    = regexp.MustCompile(`\d+`)
	stuckReSpace     = regexp.MustCompile(`\s+`)
)

// normalizeStuckOutput はツール出力を比較可能な形に正規化する：小文字化したうえで
// タイムスタンプ、パス、長い hex／ランダム文字列、数値を剥がし、空白を畳み込む。
func normalizeStuckOutput(s string) string {
	s = strings.ToLower(s)
	s = stuckReTimestamp.ReplaceAllString(s, "<ts>")
	s = stuckRePath.ReplaceAllString(s, "<path>")
	s = stuckReHex.ReplaceAllString(s, "<hex>")
	s = stuckReRand.ReplaceAllString(s, "<rand>")
	s = stuckReDigits.ReplaceAllString(s, "#")
	s = stuckReSpace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// stuckSignature は「レスポンス署名」：ツール名 + 正規化出力の FNV-1a ハッシュ。同じ署名なら必ず同じツール。
func stuckSignature(tool, norm string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(norm))
	return tool + ":" + fmt.Sprintf("%016x", h.Sum64())
}

// bigramDice は 2 つの正規化文字列の rune bigram Dice 類似度（0〜1）。両方が空文字列の
// 場合は 1 とみなす（どちらにも何もない = 同質）。
func bigramDice(a, b string) float64 {
	if a == b {
		return 1
	}
	ra, rb := []rune(a), []rune(b)
	if len(ra) < 2 || len(rb) < 2 {
		return 0
	}
	grams := make(map[[2]rune]int, len(ra)-1)
	for i := 0; i+1 < len(ra); i++ {
		grams[[2]rune{ra[i], ra[i+1]}]++
	}
	overlap := 0
	for i := 0; i+1 < len(rb); i++ {
		g := [2]rune{rb[i], rb[i+1]}
		if grams[g] > 0 {
			grams[g]--
			overlap++
		}
	}
	return 2 * float64(overlap) / float64(len(ra)+len(rb)-2)
}

// StuckEvent は 1 回の停滞判定の結果。
type StuckEvent struct {
	Tool      string // 停滞したツール名
	Kind      string // "same_signature" | "same_tool_failure"
	Count     int    // 発火時点の連続同質／連続失敗の回数
	Signature string // 命中したレスポンス署名の短いコード（ハッシュ先頭 8 桁）
	N         int    // この意図内で何回目の発火か（1 始まり）
}

type stuckEntry struct {
	tool  string
	sig   string
	norm  string
	isErr bool
}

// StuckDetector はローリング停滞検出器で、意図ごとに 1 つ（model_error の再実行ループの外に
// 置くため、再実行しても発火回数はクリアされない）。呼び出し元が tool_result を 1 つずつ渡す。
// 並行安全性はない。harness の hook は直列に呼ばれ、steerHooks の既存の前提と一致する。
type StuckDetector struct {
	cfg        StuckConfig
	recent     []stuckEntry // ローリングウィンドウ。cap = cfg.Window
	consecSig  int          // 最新署名の連続出現回数
	consecFail int          // 最新ツールの連続失敗回数
	cooldown   int          // 残り冷却ステップ数（>0 の間は発火しない）
	fires      int          // 発火済み回数
	pending    []string     // 注入待ちの介入メッセージ（PreToolUse drain、FIFO）
	// PivotHint は発火時に介入メッセージの後ろへ足すカテゴリ別の方向転換提案
	// （chainskel.PivotHints の産物。意図の chain_tags に応じて検出器の作成時に 1 度だけ
	// 計算し、その意図の間は変わらない）。
	// 空 = 追加しない（該当カテゴリがない場合は汎用の文言のまま）。
	PivotHint string
}

func NewStuckDetector(cfg StuckConfig) *StuckDetector {
	return &StuckDetector{cfg: cfg}
}

// Observe は tool_result を 1 ステップ分受け取る。「情報の勾配がゼロ」と判定したらイベントを
// 返し、介入メッセージを pending に積む（PreToolUse が次のツール呼び出し前に drain する）。
// そうでなければ nil。冷却期間中や発火上限に達した後の同質入力も通常どおり数え続ける
// （冷却が明ければ、まだ同質なら再び発火できる。MaxFires を使い切るまで）。
func (d *StuckDetector) Observe(tool, output string, isErr bool) *StuckEvent {
	norm := normalizeStuckOutput(output)
	if d.cfg.SigPrefix > 0 && utf8.RuneCountInString(norm) > d.cfg.SigPrefix {
		norm = string([]rune(norm)[:d.cfg.SigPrefix])
	}
	e := stuckEntry{tool: tool, sig: stuckSignature(tool, norm), norm: norm, isErr: isErr}

	if n := len(d.recent); n > 0 && d.recent[n-1].sig == e.sig {
		d.consecSig++
	} else {
		d.consecSig = 1
	}
	if n := len(d.recent); isErr && n > 0 && d.recent[n-1].isErr && d.recent[n-1].tool == tool {
		d.consecFail++
	} else if isErr {
		d.consecFail = 1
	} else {
		d.consecFail = 0
	}

	d.recent = append(d.recent, e)
	if d.cfg.Window > 0 && len(d.recent) > d.cfg.Window {
		d.recent = append([]stuckEntry(nil), d.recent[1:]...) // ウィンドウをコピーしてずらし、背後の配列が右へ流れ続けるのを防ぐ
	}

	if d.cooldown > 0 {
		d.cooldown--
		return nil
	}
	if d.fires >= d.cfg.MaxFires {
		return nil
	}

	var ev *StuckEvent
	switch {
	case d.consecSig >= d.cfg.SameSigTrigger:
		ev = &StuckEvent{Tool: tool, Kind: "same_signature", Count: d.consecSig, Signature: shortStuckSig(e.sig)}
	case d.consecFail >= d.cfg.FailTrigger && d.failSimilar():
		ev = &StuckEvent{Tool: tool, Kind: "same_tool_failure", Count: d.consecFail, Signature: shortStuckSig(e.sig)}
	}
	if ev == nil {
		return nil
	}
	d.fires++
	ev.N = d.fires
	d.cooldown = d.cfg.Cooldown
	msg := StuckIntervention(*ev)
	if d.PivotHint != "" {
		msg += "\n" + d.PivotHint
	}
	d.pending = append(d.pending, msg)
	return ev
}

// Drain は最も古い注入待ちの介入メッセージを取り出す（PreToolUse が次のツール呼び出し前に呼ぶ）。
func (d *StuckDetector) Drain() (string, bool) {
	if len(d.pending) == 0 {
		return "", false
	}
	msg := d.pending[0]
	d.pending = d.pending[1:]
	return msg, true
}

// Fires は発火済み回数を返す（観測・ログ用）。
func (d *StuckDetector) Fires() int { return d.fires }

// failSimilar は直近の連続失敗が「同質」かを判定する：直近 FailTrigger 個の連続失敗を取り、
// それぞれを最新の 1 件と bigram Dice で比べ、平均が FailSimilarity を超えることを求める。
func (d *StuckDetector) failSimilar() bool {
	n := d.cfg.FailTrigger
	if n <= 1 || len(d.recent) < n {
		return false
	}
	tail := d.recent[len(d.recent)-n:]
	latest := tail[n-1].norm
	sum := 0.0
	for _, e := range tail[:n-1] {
		sum += bigramDice(e.norm, latest)
	}
	return sum/float64(n-1) > d.cfg.FailSimilarity
}

// shortStuckSig は署名のハッシュ部分の先頭 8 桁を取り、提示・ログ表示用にする。
func shortStuckSig(sig string) string {
	if i := strings.IndexByte(sig, ':'); i >= 0 {
		sig = sig[i+1:]
	}
	if len(sig) > 8 {
		sig = sig[:8]
	}
	return sig
}

// StuckIntervention は発火時に worker セッションへ注入するシステムプロンプト本文。文言は
// chains meta の「同質レスポンスなら方向転換」規則に合わせる：次元を変えるか、fact を書き戻して
// この方向を掘り尽くしたと示し方向転換を求めるか。
func StuckIntervention(ev StuckEvent) string {
	what := "完全に同質"
	if ev.Kind == "same_tool_failure" {
		what = "類似度の高い連続失敗"
	}
	tool := ev.Tool
	if tool == "" {
		tool = "?"
	}
	return fmt.Sprintf("【停滞検出】直近 %d 回の応答が%s(署名 %s、ツール %s)で、情報の勾配がゼロです。"+
		"同軸での再試行を直ちに停止する:次元(パラメータ/チャネル/脆弱性の種類/入口)を変えるか、fact を書き戻してこの方向をやり尽くしたとマークし、方向転換を要求する。"+
		"(今回のツール呼び出しは未実行。先にこのシステム指示へ応答してください。これはこの意図で %d 回目の停滞の注意であり、注意できる回数には限りがあります。)",
		ev.Count, what, ev.Signature, tool, ev.N)
}
