package server

// credplan_test.go:スコアリング済みアセットの投影を再利用する(純関数、PG に依存しない)。

import (
	"testing"

	"github.com/Autumn-27/artex/db"
)

func TestReuseCandidates(t *testing.T) {
	assets := []*db.Asset{
		{ID: 1, Type: "ip", IP: "10.0.0.5", CSegment: "10.0.0.0/24",
			OpenPorts: []map[string]any{
				{"port": 445, "service": "SMB"},
				{"port": 135}, // service 名がないためスキップ
			}},
		{ID: 2, Type: "ip", IP: "", CSegment: "10.0.0.0/24"}, // IP がないため除外
		nil, // 防御的：nil 行
	}
	cands, byID := reuseCandidates(assets)
	if len(cands) != 1 || cands[0].AssetID != 1 || cands[0].IP != "10.0.0.5" {
		t.Fatalf("投影エラー: %+v", cands)
	}
	if len(cands[0].Services) != 1 || cands[0].Services[0] != "SMB" {
		t.Fatalf("サービス名の投影エラー: %+v", cands[0].Services)
	}
	if byID[1] == nil || byID[2] != nil {
		t.Fatalf("byID 索引エラー: %+v", byID)
	}
}
