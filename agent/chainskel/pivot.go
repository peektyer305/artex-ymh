package chainskel

import (
	"encoding/json"
	"strings"
)

// pivotMarker はスケルトン行の「方向転換:」エントリのフィールドマーカー(9 カテゴリの
// スケルトンは「- 場面:...|判定基準:...|方向転換:...」という蒸留形式に統一)。
const pivotMarker = "方向転換:"

// PivotHints はカテゴリスケルトンから「方向転換:」エントリを抽出し、停滞介入メッセージ内の
// カテゴリ別方向転換サジェストに組み立てる:tags を優先し(正規化+合法性フィルタ、最大
// MaxTagsPerIntent 個)、空なら summary のキーワードマッチ(Match)へフォールバック。
// ヒットした各カテゴリで「方向転換:」より後の内容を先頭 max 条まで取る。
// マッチするカテゴリがない、スケルトンが欠落、またはスケルトンから「方向転換:」エントリを
// 抽出できない場合は "" を返す(呼び出し側は汎用の表現を使う)。
func PivotHints(tags []string, summary string, max int) string {
	if max <= 0 {
		return ""
	}
	var cats []string
	for _, t := range NormalizeChainTags(tags) {
		if Valid(t) {
			cats = append(cats, t)
		}
		if len(cats) >= MaxTagsPerIntent {
			break
		}
	}
	if len(cats) == 0 {
		cats = Match(summary)
	}
	if len(cats) == 0 {
		return ""
	}
	var lines []string
	for _, c := range cats {
		sk, ok := Skeleton(c)
		if !ok {
			continue // スケルトン欠落:静かにスキップ
		}
		n := 0
		for _, line := range strings.Split(sk, "\n") {
			idx := strings.Index(line, pivotMarker)
			if idx < 0 {
				continue
			}
			hint := strings.TrimSpace(line[idx+len(pivotMarker):])
			if hint == "" {
				continue
			}
			lines = append(lines, "["+c+"] "+hint)
			n++
			if n >= max {
				break
			}
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "同種シナリオで選べる方向転換パス(反問意思決定チェーンスケルトンから抜粋。本意図の攻撃面に応じて選び、適用可否を 1 条ずつ評価する):\n- " +
		strings.Join(lines, "\n- ")
}

// PivotHintsForIntent は PivotHints の意図 payload エントリ(server/engine の接続用):
// payload から chain_tags/summary を解いて同じロジックを通す。payload の解析に失敗したら "" を返す。
func PivotHintsForIntent(payload json.RawMessage, max int) string {
	var p struct {
		Summary   string   `json:"summary"`
		ChainTags []string `json:"chain_tags"`
	}
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}
	return PivotHints(p.ChainTags, p.Summary, max)
}

// RefsGuideForIntent は意図がカテゴリにヒットしたときに L4 原文ガイド行を返す(L3 スケルトン注入ブロックの
// 後に追記):worker に完全な反問意思決定チェーン原文の Read 可能パスを伝える。カテゴリ解決は ForIntent と
// 完全に一致(同じ intentCats)。マッチしなければ "" を返す(スケルトンと同様に静かに非注入)。
// テキストは静的で chain_tags に応じてのみ変わり、prompt キャッシュの粒度を壊さない。
func RefsGuideForIntent(payload json.RawMessage) string {
	cats := intentCats(payload)
	if len(cats) == 0 {
		return ""
	}
	paths := make([]string, len(cats))
	for i, c := range cats {
		paths[i] = "skills/chains-" + c + "/refs/"
	}
	return "\n【L4 原文ガイド】この領域の反問意思決定チェーン原文は " + strings.Join(paths, "、") +
		"(正確な篇目は対応する SKILL.md を参照)。停滞したとき、または完全な判定基準が必要なときに該当ファイルを Read する。"
}
