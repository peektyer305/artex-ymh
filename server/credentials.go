// credentials.go は認証情報を一級エンティティとして扱う 3 つの host ツールを組み込む(イントラネット侵攻フェーズ 2,
// INTRANET-PIVOT-DESIGN.md §4.4):add_credential/list_credentials/
// credential_reuse_plan。フェーズ 1a のセッションツールと同じ方式:通常のツール呼び出しで、guard 承認を自動で通過し、
// activity に自動で記録される;tools テーブルに seed し、デフォルトで worker に紐付ける。
package server

import (
	"context"
	"encoding/json"
	"log"

	"github.com/Autumn-27/artex/agent"
	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// initCredentials は認証情報の永続化アクセス層を構築する(secret は AES-GCM、鍵は jwtKey から派生し、
// sessions とは別のドメインラベルを使う)。失敗はログのみで、起動を妨げない。
func (s *Server) initCredentials() {
	if s.m.pg == nil {
		return
	}
	s.credStore = db.NewCredentialStore(s.m.pg, s.jwtKey)
}

// credentialTools は 3 つの認証情報ツールを返す。credStore が nil(DB 未準備)のときは提供しない。
func (s *Server) credentialTools() []actool.CoreTool {
	if s.credStore == nil {
		return nil
	}
	return []actool.CoreTool{
		s.addCredentialTool(),
		s.listCredentialsTool(),
		s.credentialReusePlanTool(),
	}
}

// addCredentialTool は対象から回収した認証情報をプラットフォーム管理の一級エンティティとして登録する(暗号化して永続化)。
func (s *Server) addCredentialTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "add_credential",
		Description: "【認証情報の登録】対象から読み取ったパスワード/hash/鍵/ticket をプラットフォーム管理の認証情報として登録する(secret は AES-GCM で暗号化して永続化)。" +
			"回収した認証情報はすべて登録すべき:再利用スコアリング(credential_reuse_plan)がこれを基に横展開可能な対象を推測する。" +
			"ソース(source)には取得元を明確に書く(例: \"sqli dump users テーブル\" / \"/etc/shadow\" / \"mimikatz lsass dump\")、" +
			"スコアリングは source 内の ssh/rdp/smb などのサービスキーワードをマッチングに含める。実際にログインできた後は verify=true で再登録するか、プラットフォームに verified を付けさせる。",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"username":      strParam("ユーザー名(空でも可、ticket/token などユーザー名がない場合は空)"),
				"cred_type":     strParam("認証情報の種類：password / hash_nt / hash_lm / hash_sha1 / ticket / ssh_key / token"),
				"secret":        strParam("認証情報の内容(パスワード平文/hash/秘密鍵/ticket 文字列。暗号化して保存し、平文では永続化しない)"),
				"domain":        strParam("ドメイン(任意、CORP や corp.local など。ドメイン認証情報は再利用スコアリングで大きく加点される)"),
				"source":        strParam("ソースの説明(必須):どのホストから/どうやって取得したか。例: \"10.0.0.5 /etc/shadow\"、\"sqli dump users テーブル\""),
				"host_asset_id": map[string]any{"type": "integer", "description": "認証情報の取得元ホストのアセット id(任意。同一セグメントのスコアリングに使う)"},
			},
			"required": []any{"cred_type", "secret", "source"},
		},
		ReadOnly:    func(json.RawMessage) bool { return false },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, in json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			var a struct {
				Username    string `json:"username"`
				CredType    string `json:"cred_type"`
				Secret      string `json:"secret"`
				Domain      string `json:"domain"`
				Source      string `json:"source"`
				HostAssetID int64  `json:"host_asset_id"`
			}
			_ = json.Unmarshal(in, &a)
			if !db.ValidCredType(a.CredType) {
				return actool.Errorf("不正な cred_type " + a.CredType + "(password/hash_nt/hash_lm/hash_sha1/ticket/ssh_key/token)"), nil
			}
			if a.Secret == "" {
				return actool.Errorf("secret は必須です"), nil
			}
			if a.Source == "" {
				return actool.Errorf("source は必須です(再利用スコアリングと追跡の両方が依存します)"), nil
			}
			ri := agent.RunInfoFrom(ctx)
			if ri.TaskID <= 0 {
				return actool.Errorf("認証情報はタスクコンテキスト内で登録する必要があります(タスク実行外では task に紐付きません)"), nil
			}
			rec := &db.CredentialRecord{
				TaskID: ri.TaskID, HostAssetID: a.HostAssetID, Username: a.Username,
				CredType: a.CredType, Secret: a.Secret, Domain: a.Domain, Source: a.Source,
			}
			id, err := s.credStore.Create(ctx, rec)
			if err != nil {
				return actool.Errorf("認証情報の永続化に失敗しました:" + err.Error()), nil
			}
			return jsonResult(map[string]any{"credential_id": id})
		},
	})
}

// listCredentialsTool は本タスクの認証情報のマスク済み一覧を返す(secret は前後各 2 文字だけ表示)。
func (s *Server) listCredentialsTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "list_credentials",
		Description: "本タスクに登録済みの認証情報をすべて一覧する(マスク表示：secret は前後 2 文字だけを返す)。" +
			"横展開の前に既存の認証情報を確認し、重複した回収を避ける。特定の対象を攻撃するときは credential_reuse_plan で再利用の提案を得る。",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		ReadOnly:    func(json.RawMessage) bool { return true },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, _ json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			ri := agent.RunInfoFrom(ctx)
			if ri.TaskID <= 0 {
				return actool.Errorf("list_credentials にはタスクコンテキストが必要です"), nil
			}
			creds, err := s.credStore.ListByTask(ctx, ri.TaskID)
			if err != nil {
				return actool.Errorf("認証情報の読み込みに失敗しました:" + err.Error()), nil
			}
			items := []map[string]any{}
			for _, c := range creds {
				items = append(items, map[string]any{
					"id": c.ID, "username": c.Username, "cred_type": c.CredType,
					"secret_masked": db.MaskSecret(c.Secret),
					"domain":        c.Domain, "source": c.Source, "verified": c.Verified,
					"host_asset_id": c.HostAssetID, "created_at": c.CreatedAt,
				})
			}
			return jsonResult(map[string]any{"credentials": items, "count": len(items)})
		},
	})
}

// credentialReusePlanTool は本タスクの認証情報 × 本タスクのアセットでサーバ側の再利用スコアリングを実行し、上位 10 件を返す。
func (s *Server) credentialReusePlanTool() actool.CoreTool {
	return actool.Build(actool.Spec{
		Name: "credential_reuse_plan",
		Description: "【認証情報の再利用スコアリング】本タスクに登録済みの認証情報 × 本タスクで発見済みのホストアセットに対して再利用スコアリングを実行し(純粋なルール:同一セグメント/ドメイン認証情報/サービス重複/PtH/検証済み)、" +
			"スコア上位 10 件の横展開提案(認証情報 × 対象アセット + 各加点の理由)を返す。提案は成功率の事前見積りにすぎず、" +
			"実際の横展開(ssh/wmi/psexec/パスワードスプレー)は痕跡の大きい操作なので、guard の手動確認を経て RoE の範囲内で行う必要がある。",
		Schema: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
		ReadOnly:    func(json.RawMessage) bool { return true },
		Permissions: sessionPerm,
		Run: func(ctx context.Context, _ json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			ri := agent.RunInfoFrom(ctx)
			if ri.TaskID <= 0 {
				return actool.Errorf("credential_reuse_plan にはタスクコンテキストが必要です"), nil
			}
			creds, err := s.credStore.ListByTask(ctx, ri.TaskID)
			if err != nil {
				return actool.Errorf("認証情報の読み込みに失敗しました:" + err.Error()), nil
			}
			if len(creds) == 0 {
				return actool.Text("本タスクにはまだ認証情報が登録されていません(先に add_credential で登録してください)。再利用の提案はありません"), nil
			}
			assets, err := s.m.pg.Assets().QueryByTask(ri.TaskID, "ip", 2000, 0)
			if err != nil {
				return actool.Errorf("アセットの読み込みに失敗しました:" + err.Error()), nil
			}
			cands, byID := reuseCandidates(assets)
			type row struct {
				CredID   int64    `json:"cred_id"`
				Username string   `json:"username"`
				CredType string   `json:"cred_type"`
				AssetID  int64    `json:"asset_id"`
				IP       string   `json:"ip"`
				Score    int      `json:"score"`
				Reasons  []string `json:"reasons"`
			}
			flat := []row{}
			for _, c := range creds {
				var src *db.ReuseCandidate
				if c.HostAssetID != 0 {
					src = byID[c.HostAssetID]
				}
				for _, sug := range db.ScoreReuse(c, src, cands) {
					flat = append(flat, row{c.ID, c.Username, c.CredType, sug.AssetID, sug.IP, sug.Score, sug.Reasons})
				}
			}
			// 全体の並び替え:スコア降順、同スコアは cred_id/asset_id 昇順(決定的)。
			for i := 0; i < len(flat); i++ {
				for j := i + 1; j < len(flat); j++ {
					if flat[j].Score > flat[i].Score ||
						(flat[j].Score == flat[i].Score && (flat[j].CredID < flat[i].CredID ||
							(flat[j].CredID == flat[i].CredID && flat[j].AssetID < flat[i].AssetID))) {
						flat[i], flat[j] = flat[j], flat[i]
					}
				}
			}
			if len(flat) > 10 {
				flat = flat[:10]
			}
			return jsonResult(map[string]any{
				"suggestions": flat, "credential_count": len(creds), "asset_count": len(cands),
			})
		},
	})
}

// reuseCandidates は ip アセットをスコアリング候補に射影する(サービス名は open_ports から取得)。
func reuseCandidates(assets []*db.Asset) ([]db.ReuseCandidate, map[int64]*db.ReuseCandidate) {
	out := []db.ReuseCandidate{}
	byID := map[int64]*db.ReuseCandidate{}
	for _, a := range assets {
		if a == nil || a.IP == "" {
			continue
		}
		services := []string{}
		for _, p := range a.OpenPorts {
			if svc, ok := p["service"].(string); ok && svc != "" {
				services = append(services, svc)
			}
		}
		out = append(out, db.ReuseCandidate{
			AssetID: a.ID, IP: a.IP, CSegment: a.CSegment, Services: services,
		})
		byID[a.ID] = &out[len(out)-1]
	}
	return out, byID
}

// seedCredentialTools は 3 つの認証情報ツールを tools テーブルに seed し、デフォルトで worker に紐付ける
// (seedSessionTools と同じ SeedTool の初回挿入セマンティクス:既存 DB のユーザー編集は上書きしない)。
func (s *Server) seedCredentialTools() {
	workerAgents, _ := json.Marshal([]string{"worker"})
	for _, t := range s.credentialTools() {
		schema, _ := json.Marshal(t.InputSchema())
		if err := s.m.PG().SeedTool(t.Name(), t.Description(), schema, workerAgents); err != nil {
			log.Printf("[tools] seed %s に失敗しました: %v", t.Name(), err)
		}
	}
}
