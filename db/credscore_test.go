package db

// credscore_test.go:再利用スコアリング純関数の単体テスト（ルールのヒット/並び替え/絞り込み/同点時の決定性）。

import (
	"strings"
	"testing"
)

func TestScoreReuse(t *testing.T) {
	cred := &CredentialRecord{
		HostAssetID: 1, Username: "administrator", CredType: CredHashNT,
		Domain: "CORP", Verified: true, Source: "10.0.0.5 mimikatz lsass dump smb",
	}
	src := &ReuseCandidate{AssetID: 1, IP: "10.0.0.5", CSegment: "10.0.0.0/24"}
	cands := []ReuseCandidate{
		{AssetID: 1, IP: "10.0.0.5", CSegment: "10.0.0.0/24", Services: []string{"SMB"}},      // ソースホスト自身なので絞り込まれる
		{AssetID: 2, IP: "10.0.0.9", CSegment: "10.0.0.0/24", Services: []string{"SMB"}},      // 40+35+20+15+25=135
		{AssetID: 3, IP: "172.16.0.9", CSegment: "172.16.0.0/24", Services: []string{"HTTP"}}, // 35+15+25=75
		{AssetID: 4, IP: "192.168.1.9", CSegment: "192.168.1.0/24"},                           // 35+15+25=75
	}
	sugs := ScoreReuse(cred, src, cands)
	if len(sugs) != 3 {
		t.Fatalf("期待は 3 件の提案（ソースホストは除外）、実際は %d: %+v", len(sugs), sugs)
	}
	if sugs[0].AssetID != 2 || sugs[0].Score != 135 {
		t.Fatalf("top1 は asset 2 / 135 点のはず: %+v", sugs[0])
	}
	// 同点は asset_id 昇順（決定性）。
	if sugs[1].AssetID != 3 || sugs[2].AssetID != 4 || sugs[1].Score != 75 {
		t.Fatalf("同点時の並び順が誤り: %+v", sugs[1:])
	}
	// 各加点には可読な reason がある。
	joined := strings.Join(sugs[0].Reasons, " ")
	for _, kw := range []string{"同网段", "域凭据", "重叠", "PtH", "已实测验证"} {
		if !strings.Contains(joined, kw) {
			t.Errorf("top1 の reason に %q がありません: %s", kw, joined)
		}
	}
}

func TestScoreReuseNoDomainUnverified(t *testing.T) {
	// ローカルの平文パスワード、未検証:同セグメントとサービス重複の点だけが残る。
	cred := &CredentialRecord{
		HostAssetID: 1, Username: "root", CredType: CredPassword,
		Source: "10.0.0.5 /etc/shadow ssh ログイン",
	}
	src := &ReuseCandidate{AssetID: 1, IP: "10.0.0.5", CSegment: "10.0.0.0/24"}
	cands := []ReuseCandidate{
		{AssetID: 2, IP: "10.0.0.9", CSegment: "10.0.0.0/24", Services: []string{"ssh"}}, // 40+20=60
		{AssetID: 3, IP: "172.16.0.9", CSegment: "172.16.0.0/24"},                        // 0 点なので絞り込まれる
	}
	sugs := ScoreReuse(cred, src, cands)
	if len(sugs) != 1 || sugs[0].AssetID != 2 || sugs[0].Score != 60 {
		t.Fatalf("ローカルパスワードのスコアが誤り: %+v", sugs)
	}
}

func TestScoreReuseNilSource(t *testing.T) {
	// ソースホスト不明（host_asset_id 未設定）:同セグメントのルールは非ヒットで、他は通常どおり。
	cred := &CredentialRecord{CredType: CredHashNT, Source: "sqli dump users テーブル"}
	cands := []ReuseCandidate{
		{AssetID: 9, IP: "10.0.0.9", CSegment: "10.0.0.0/24"},
	}
	sugs := ScoreReuse(cred, nil, cands)
	if len(sugs) != 1 || sugs[0].Score != 15 {
		t.Fatalf("ソースホスト不明時のスコアが誤り（PtH +15 のみのはず）: %+v", sugs)
	}
}
