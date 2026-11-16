package db

// バッチ 6 L1 ハニーポット静的シグネチャ識別(docs/architecture/honeypot-detection.md 二·L1):db 層は
// 保存/取得のみを担い、シグネチャ照合ロジックはすべて honeydetect パッケージにある。スコアとエビデンスは
// server/agent のフックが service アセットの保存後に書き込み、planner への注入と UI はここから読み出す。

import (
	"encoding/json"
	"fmt"
)

// SetHoneypotScore はアセットのハニーポットスコアとエビデンスを書き込む。再評価では高い方のスコアを採り、
// エビデンス(JSON 配列テキスト)はマージして重複を除く。スコアが上がらずエビデンスも増えない場合は行を更新しない(冪等)。
func (s *AssetStore) SetHoneypotScore(assetID int64, score float64, evidence []string) error {
	if assetID <= 0 || score <= 0 {
		return nil
	}
	var curScore float64
	var curEvidence string
	if err := s.db.QueryRow(
		`SELECT honeypot_score, COALESCE(honeypot_evidence,'') FROM assets WHERE id = $1`,
		assetID).Scan(&curScore, &curEvidence); err != nil {
		return err
	}
	merged := MergeHoneypotEvidence(curEvidence, evidence)
	if score < curScore {
		score = curScore // 高い方のスコアを採る
	}
	if score == curScore && merged == curEvidence {
		return nil
	}
	_, err := s.db.Exec(
		`UPDATE assets SET honeypot_score = $2, honeypot_evidence = $3 WHERE id = $1`,
		assetID, score, merged)
	return err
}

// MergeHoneypotEvidence は新たにヒットしたエビデンスを既存のエビデンス(JSON 配列テキスト)へマージし、重複を除く。
// 旧値が有効な JSON 配列でない場合は旧値なしとして扱う(新しいエビデンスを失わない)。
func MergeHoneypotEvidence(existing string, add []string) string {
	var items []string
	if existing != "" {
		_ = json.Unmarshal([]byte(existing), &items)
	}
	seen := map[string]bool{}
	for _, it := range items {
		seen[it] = true
	}
	for _, it := range add {
		if it == "" || seen[it] {
			continue
		}
		seen[it] = true
		items = append(items, it)
	}
	if len(items) == 0 {
		return ""
	}
	b, err := json.Marshal(items)
	if err != nil {
		return existing
	}
	return string(b)
}

// HoneypotAsset は planner への注入に使う 1 行分のハニーポット疑いアセット。
type HoneypotAsset struct {
	ID       int64
	Label    string // url、それ以外は host:port
	Score    float64
	Evidence string // JSON 配列テキスト
}

// HoneypotAssetsByTask は本タスクの score>0 のアセットをスコア降順で一覧する。limit<=0 ならデフォルト 20。
func (s *AssetStore) HoneypotAssetsByTask(taskID int64, limit int) ([]HoneypotAsset, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`
SELECT id, COALESCE(url,''), COALESCE(domain,''), COALESCE(ip,''), port,
       honeypot_score, COALESCE(honeypot_evidence,'')
FROM assets
WHERE $1 = ANY(task_ids) AND honeypot_score > 0
ORDER BY honeypot_score DESC, id DESC
LIMIT $2`, taskID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HoneypotAsset
	for rows.Next() {
		var h HoneypotAsset
		var urlv, domain, ip string
		var port *int
		if err := rows.Scan(&h.ID, &urlv, &domain, &ip, &port, &h.Score, &h.Evidence); err != nil {
			return nil, err
		}
		switch {
		case urlv != "":
			h.Label = urlv
		case port != nil:
			host := domain
			if host == "" {
				host = ip
			}
			h.Label = fmt.Sprintf("%s:%d", host, *port)
		default:
			h.Label = domain
			if h.Label == "" {
				h.Label = ip
			}
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
