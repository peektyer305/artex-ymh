package agent

// バッチ 6 L1（設計ドキュメント HONEYPOT-DETECTION-DESIGN.md 二・L1 +「処置戦略」表）:
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

**蜜罐处置纪律(不可编辑)**:
1. 态势里的 honeypot_assets 是静态签名命中的疑似蜜罐资产,属【单信号】:对它们**降级交互**——只用被动/只读手段继续观察,**不投递工具/载荷、不爆破、不利用**;仅凭单信号不得封锁方向。
2. 只有【双信号命中】(静态签名 + 行为/登录后特征,如 /proc/meminfo 连读不变、ps 时间戳固定、egress 回连声称成功却无流量到达)才判定确认蜜罐,**停止在该方向派意图**;涉及反制型(MySQL 读客户端文件、客户端诱导)时,连客户端连接都不发起。
3. 疑似蜜罐资产上已有意图在跑的,让它以最低交互收尾,不追加高价值动作;侦察与利用资源优先投向无蜜罐信号的方向。
4. 全阴性【不等于】非蜜罐(高交互/加固蜜罐对自动化识别免疫):对无信号目标同样不投放高价值 exploit、控制噪声。`

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
