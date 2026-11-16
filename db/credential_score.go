// credential_score.go は認証情報の再利用スコアリング（サーバ側版、INTRANET-PIVOT-DESIGN.md §4.4）。
// PivotHub cred.js:44-50 のフロントエンドの naive なスコアリングを参考に、ここでは純関数 + データ表駆動のルールにした:
// ルールごとに 1 つの点数と可読な reason を持ち、ルール追加は表の変更だけで済み、ロジックには触れない。DB 依存ゼロで直接単体テストできる。
package db

import (
	"fmt"
	"sort"
	"strings"
)

// ReuseCandidate は再利用スコアリングの候補ホストアセットのビュー（呼び出し側が assets 表から射影する）。
type ReuseCandidate struct {
	AssetID  int64    `json:"asset_id"`
	IP       string   `json:"ip"`
	CSegment string   `json:"c_segment"` // 例 "10.0.0.0/24"
	Services []string `json:"services"`  // アセット上で検出済みのサービス名（大文字小文字を区別しない）
}

// ReuseSuggestion は 1 件の再利用提案:対象アセット + スコア + 各加点の可読な理由。
type ReuseSuggestion struct {
	AssetID int64    `json:"asset_id"`
	IP      string   `json:"ip"`
	Score   int      `json:"score"`
	Reasons []string `json:"reasons"`
}

// sourceServiceHints は「認証情報のソース記述のキーワード → サービス名」のデータ表：source に
// キーワードが現れれば、その認証情報が対応サービスに使える可能性があるとみなす（例 "ssh 私钥" → SSH）。追加は行を足すだけ。
var sourceServiceHints = []struct {
	Keyword string
	Service string
}{
	{"ssh", "SSH"},
	{"rdp", "RDP"},
	{"smb", "SMB"},
	{"winrm", "WinRM"},
	{"mysql", "MySQL"},
	{"mssql", "MSSQL"},
	{"postgres", "PostgreSQL"},
	{"redis", "Redis"},
	{"ftp", "FTP"},
	{"telnet", "Telnet"},
	{"vnc", "VNC"},
	{"ldap", "LDAP"},
	{"kerberos", "Kerberos"},
	{"http", "HTTP"},
}

// sourceServices は source の記述からヒットしたサービス名を抽出する（重複排除、順序維持）。
func sourceServices(source string) []string {
	lower := strings.ToLower(source)
	out := []string{}
	seen := map[string]bool{}
	for _, h := range sourceServiceHints {
		if seen[h.Service] || !strings.Contains(lower, h.Keyword) {
			continue
		}
		seen[h.Service] = true
		out = append(out, h.Service)
	}
	return out
}

// reuseRule は 1 つのスコアリングルール：check はヒット時の可読な reason を返す（空文字列 = 非ヒット）。
// ルール表駆動:加点項目の追加は reuseRules の変更だけで済み、ScoreReuse の主処理には触れない。
type reuseRule struct {
	points int
	check  func(cred *CredentialRecord, src, cand *ReuseCandidate) string
}

// reuseRules はスコアリングルール表（点数は設計 §4.4/タスクの取り決めによる）:
var reuseRules = []reuseRule{
	{40, func(_ *CredentialRecord, src, cand *ReuseCandidate) string {
		if src != nil && src.CSegment != "" && src.CSegment == cand.CSegment {
			return fmt.Sprintf("与凭据来源主机同网段(%s)", cand.CSegment)
		}
		return ""
	}},
	{35, func(cred *CredentialRecord, _, _ *ReuseCandidate) string {
		if strings.TrimSpace(cred.Domain) != "" {
			return fmt.Sprintf("域凭据(domain=%s),可用于域内横向", cred.Domain)
		}
		return ""
	}},
	{20, func(cred *CredentialRecord, _, cand *ReuseCandidate) string {
		have := map[string]bool{}
		for _, s := range cand.Services {
			have[strings.ToUpper(s)] = true
		}
		overlap := []string{}
		for _, s := range sourceServices(cred.Source) {
			if have[strings.ToUpper(s)] {
				overlap = append(overlap, s)
			}
		}
		if len(overlap) > 0 {
			return "目标服务与凭据来源服务重叠:" + strings.Join(overlap, "/")
		}
		return ""
	}},
	{15, func(cred *CredentialRecord, _, _ *ReuseCandidate) string {
		if cred.CredType == CredHashNT {
			return "NTLM hash 可直接传递(PtH),无需明文"
		}
		return ""
	}},
	{25, func(cred *CredentialRecord, _, _ *ReuseCandidate) string {
		if cred.Verified {
			return "凭据已实测验证可用"
		}
		return ""
	}},
}

// ScoreReuse は 1 件の認証情報 × 候補ホストアセットの一覧を採点し、スコア降順の提案を返す（同点は
// asset_id 昇順で決定性を保証）。src は認証情報のソースホストのアセットビュー（nil 可——ソース
// ホストが不明なら同セグメントのルールは自然に非ヒット）。ソースホスト自身（スコアに意味がない）と 0 点の候補は除外する。
func ScoreReuse(cred *CredentialRecord, src *ReuseCandidate, cands []ReuseCandidate) []ReuseSuggestion {
	out := []ReuseSuggestion{}
	for i := range cands {
		cand := &cands[i]
		if cand.AssetID == cred.HostAssetID && cred.HostAssetID != 0 {
			continue // ソースホスト自身は横展開の対象にならない
		}
		score := 0
		reasons := []string{}
		for _, rule := range reuseRules {
			if reason := rule.check(cred, src, cand); reason != "" {
				score += rule.points
				reasons = append(reasons, fmt.Sprintf("%s(+%d)", reason, rule.points))
			}
		}
		if score <= 0 {
			continue
		}
		out = append(out, ReuseSuggestion{
			AssetID: cand.AssetID, IP: cand.IP, Score: score, Reasons: reasons,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].AssetID < out[j].AssetID
	})
	return out
}
