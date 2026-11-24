package agent

// バッチ 6 L1（設計ドキュメント docs/architecture/honeypot-detection.md 二・L1 +「処置戦略」表）:
// ハニーポットの静的シグネチャ識別層を agent 側に組み込むフックと、planner の処置規律。
// - evaluateHoneypot: service アセットの登録後に honeydetect のシグネチャ評価を実行し、score>0 なら
//   アセットの honeypot_score/evidence を書き戻し（db.SetHoneypotScore、高いスコアを採用し証拠を重複排除）、
//   当該アセットをアンカーする fact を探索グラフへ 1 件書き込む（planner は毎ターンの状況で参照可）。すべて best-effort で、
//   失敗してもアセット登録の主処理には影響しない。
// - plannerHoneypotRules: 処置規律。バッチ 5 のレッドラインと同方式のコード固定テール（DB 本文で
//   上書き不可）で、plannerSystem が一律で追加する。

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/artex/honeydetect"
)

// plannerHoneypotRules は planner のハニーポット処置規律（設計ドキュメント「処置戦略」表）:
// 二重シグナルでのみ封鎖し、単一シグナルでは降格のみ。ハニーポットの疑いがある先には投下も突破も悪用もしない。
const plannerHoneypotRules = `

**ハニーポットの処置規律(編集不可)**:
1. 状況内の honeypot_assets は静的シグネチャが命中したハニーポット疑いのアセットで、【単一シグナル】に属する:これらへは**対話を降格**する。受動/読み取り専用の手段だけで観察を続け、**ツール/ペイロードを投下せず、ブルートフォースせず、悪用しない**。単一シグナルだけで方向を封鎖してはならない。
2. 【二重シグナル命中】(静的シグネチャ + 挙動/ログイン後の特徴。例: /proc/meminfo を連続で読んでも変わらない、ps のタイムスタンプが固定、egress の再接続が成功と主張するのにトラフィックが届かない)の場合だけ、ハニーポット確定と判定し、**その方向への意図の割り当てを停止する**。対抗型(MySQL のクライアントファイル読み取り、クライアント誘導)が絡む場合は、クライアント接続すら開始しない。
3. ハニーポット疑いのアセット上で既に意図が動いている場合は、最低限の対話で収束させ、高価値の動作を追加しない。偵察と悪用のリソースはハニーポットのシグナルが無い方向へ優先的に振り向ける。
4. 全陰性は【非ハニーポット】と【等しくない】(高対話型/強化型のハニーポットは自動識別に耐性がある):シグナルの無い対象にも高価値の exploit を投下せず、ノイズを制御する。`

// honeypotConfidence はスコアを処置段階へ対応付ける: ≥0.9 high、≥0.6 medium（それ未満は記録しない）。
func honeypotConfidence(score float64) string {
	if score >= 0.9 {
		return "high"
	}
	return "medium"
}

// evaluateHoneypot は service アセット登録後の L1 シグネチャ評価フック（insert_assets と
// httpx の書き戻しが同じ経路）。best-effort: 失敗はハニーポットのマークだけに影響し、アセット登録には影響しない。
func (t *ToolSet) evaluateHoneypot(assetID int64, v honeydetect.ServiceView) {
	if assetID <= 0 {
		return
	}
	score, evidence := honeydetect.Evaluate(v)
	if score <= 0 {
		return
	}
	if t.as != nil {
		_ = t.as.SetHoneypotScore(assetID, score, evidence)
	}
	if t.ts == nil || t.taskID <= 0 {
		return
	}
	conf := honeypotConfidence(score)
	payload := map[string]any{
		"summary": fmt.Sprintf("ハニーポットの疑いがあるアセット #%d: 静的シグネチャ命中（score=%.2f, %s）", assetID, score, conf),
		"detail":  "命中シグネチャ: " + strings.Join(evidence, "; ") + "。処置: 単一シグナルでは対話を降格（受動/読み取り専用）し、投下/突破/悪用はしない。二重シグナル命中でのみ方向を封鎖する。",
		// シグネチャ一致は banner/title/証明書への直接観測 → observed。
		// honeypot_confidence は別途処置段階を記録する（high/medium、スコア対応）。
		"confidence":          FactConfidenceObserved,
		"honeypot_score":      score,
		"honeypot_confidence": conf,
		"honeypot_signatures": evidence,
	}
	_, _ = t.ts.AddNode(db.KindFact, payload, 5, "confirmed", t.worker, []int64{assetID})
}

// urlPort は URL からポートを取る（省略時は scheme による）。取れなければ 0 を返す。
func urlPort(raw string) int {
	u, err := url.Parse(raw)
	if err != nil {
		return 0
	}
	if p := u.Port(); p != "" {
		var n int
		if _, err := fmt.Sscanf(p, "%d", &n); err == nil {
			return n
		}
		return 0
	}
	switch strings.ToLower(u.Scheme) {
	case "http":
		return 80
	case "https":
		return 443
	}
	return 0
}
