package db

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

func TestNormalizeVulnClass(t *testing.T) {
	cases := map[string]string{
		"SQLi":                           "sqli",
		"sql-injection":                  "sqli",
		"SQL Injection":                  "sqli",
		" sql_injection ":                "sqli",
		"XSS":                            "xss",
		"Cross-Site Scripting":           "xss",
		"SSRF":                           "ssrf",
		"SSTI":                           "ssti",
		"Server-Side Template Injection": "ssti",
		"XXE":                            "xxe",
		"RCE":                            "cmdi",
		"cmdi":                           "cmdi",
		"Command Injection":              "cmdi",
		"remote code execution":          "cmdi",
		"File Upload":                    "upload",
		"Auth Bypass":                    "auth-bypass",
		"business_logic":                 "logic",
		"Deserialization":                "deser",
		"Insecure Deserialization":       "deser",
		"Information Disclosure":         "info-leak",
		"privilege escalation":           "privesc",
		"other":                          "other",
		// 曖昧なケース：実戦で揺らぐ文字列の組（正規化後に等しければ相互マージできる）
		"Stored Cross-Site Scripting (XSS)": "xss",
		"XSS (Stored)":                      "xss",
		// blind sqli は sqli に寄せる（既存の粗い分類と一致。ブラインドは利用手法であり新しい分類ではない）
		"SQL Injection (Blind)": "sqli",
		"Blind SQL Injection":   "sqli",
		"SQL 盲注":                "sqli",
		// 中国語 / CVE 番号付き / 括弧内の略語
		"任意文件上传 (CVE-2017-12615)": "upload",
		"任意文件上传":                  "upload",
		"远程代码执行":                  "cmdi",
		"远程命令执行":                  "cmdi",
		"跨站脚本":                    "xss",
		"本地文件包含 (LFI)":            "lfi",
		"敏感信息泄露":                  "info-leak",
		"业务逻辑漏洞":                  "logic",
		"密码爆破":                    "brute-force",
		// 保守的："injection" を含んでも sqli とは限らない。SSTI は template-injection キーワードで収束させる
		"Template Injection":                      "ssti",
		"Jinja2 模板注入":                             "ssti",
		"Insecure Deserialization (Java)":         "deser",
		"Vertical Privilege Escalation (PrivEsc)": "privesc",
		"OS Command Injection (RCE)":              "cmdi",
		"Authentication Bypass (MFA)":             "auth-bypass",
		// 未知の項目：そのまま小文字化し、折り畳み後も保持する
		"csrf":             "csrf",
		"Weird  New_Class": "weird-new-class",
		"":                 "",
	}
	for in, want := range cases {
		if got := NormalizeVulnClass(in); got != want {
			t.Errorf("NormalizeVulnClass(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMaxSeverity(t *testing.T) {
	cases := []struct {
		a, b, want string
	}{
		{"low", "medium", "medium"},
		{"critical", "high", "critical"},
		{"high", "high", "high"},
		{"medium", "low", "medium"},
		{"", "low", "low"},
		{"weird", "", "weird"},        // どちらも未知：a を返す
		{"medium", "weird", "medium"}, // 未知は 0 として扱う
	}
	for _, c := range cases {
		if got := MaxSeverity(c.a, c.b); got != c.want {
			t.Errorf("MaxSeverity(%q,%q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

func TestMergeFindingFields(t *testing.T) {
	now := time.Date(2026, 9, 13, 12, 0, 0, 0, time.UTC)
	old := DBFinding{Severity: "medium", Summary: "旧要約", Evidence: "旧エビデンス本文"}

	// summary が異なる：置換し、旧要約を evidence へアーカイブ。severity は高い方、新エビデンスはマーカー付きで追記
	sev, sum, ev := mergeFindingFields(old, RecordFindingInput{
		Severity: "high", Summary: "より深い悪用の要約", Evidence: "新 PoC", IntentID: 77, Worker: "w2",
	}, now)
	if sev != "high" || sum != "より深い悪用の要約" {
		t.Fatalf("severity/summary: got (%q,%q)", sev, sum)
	}
	for _, want := range []string{"旧エビデンス本文", "[原摘要]", "旧要約", "[新证据]", "新 PoC", "intent #77", "by w2", "2026-09-13T12:00:00Z"} {
		if !strings.Contains(ev, want) {
			t.Errorf("merged evidence missing %q:\n%s", want, ev)
		}
	}
	if !strings.HasPrefix(ev, "旧エビデンス本文") {
		t.Errorf("元の evidence は先頭に保持されなければなりません:\n%s", ev)
	}

	// summary が同一：置換もアーカイブもしない。severity が同格なら旧値を保持。空の evidence は追記しない
	sev, sum, ev = mergeFindingFields(old, RecordFindingInput{Severity: "low", Summary: "旧要約"}, now)
	if sev != "medium" || sum != "旧要約" || ev != "旧エビデンス本文" {
		t.Fatalf("no-op merge: got (%q,%q,%q)", sev, sum, ev)
	}

	// 新しい summary が空：旧 summary を保持し、evidence は変更しない
	_, sum, ev = mergeFindingFields(old, RecordFindingInput{Severity: "medium", Evidence: "  "}, now)
	if sum != "旧要約" || ev != "旧エビデンス本文" {
		t.Fatalf("empty summary/evidence merge: got (%q,%q)", sum, ev)
	}

	// 旧 summary が空で新 summary が非空：置換するが「原摘要」のアーカイブ段は作らない
	emptyOld := DBFinding{Severity: "low", Summary: "", Evidence: "E"}
	_, sum, ev = mergeFindingFields(emptyOld, RecordFindingInput{Severity: "low", Summary: "S", Evidence: "E2", IntentID: 0}, now)
	if sum != "S" || strings.Contains(ev, "[原摘要]") || !strings.Contains(ev, "E2") {
		t.Fatalf("empty old summary merge: got (%q,%q)", sum, ev)
	}

	// vulnclass の表現揺らぎ（正規化後に等しい）：追記段に今回の原文を注記し、モデルの揺らぎを監査できるようにする
	oldX := DBFinding{VulnClass: "XSS (Stored)", Severity: "medium", Summary: "S", Evidence: "E"}
	_, _, ev = mergeFindingFields(oldX, RecordFindingInput{
		VulnClass: "Stored Cross-Site Scripting (XSS)", Severity: "medium", Summary: "S", Evidence: "新エビデンス",
	}, now)
	if !strings.Contains(ev, `本次 vulnclass 原文 "Stored Cross-Site Scripting (XSS)"`) {
		t.Errorf("drift annotation missing:\n%s", ev)
	}

	// vulnclass の原文が同一：注記しない
	_, _, ev = mergeFindingFields(oldX, RecordFindingInput{
		VulnClass: "XSS (Stored)", Severity: "medium", Summary: "S", Evidence: "新エビデンス",
	}, now)
	if strings.Contains(ev, "vulnclass 原文") {
		t.Errorf("no drift expected:\n%s", ev)
	}

	// 新しいエビデンスがなく summary も不変だが揺らぎはある：監査用の段を単独で残し、痕跡を失わない
	_, _, ev = mergeFindingFields(oldX, RecordFindingInput{
		VulnClass: "Stored Cross-Site Scripting (XSS)", Severity: "medium", Summary: "S",
	}, now)
	if !strings.Contains(ev, "vulnclass 原文") || !strings.Contains(ev, "仅 vulnclass 措辞漂移") {
		t.Errorf("drift-only segment missing:\n%s", ev)
	}
}

// --- PG 依存パス（本機に ARTEX_PG_DSN がない場合は自動 skip。PG のある環境で実測が必要） ---

// TestRecordFindingTxMerge はアプローチ A のマージをエンドツーエンドで検証する：同一入口の
// confirmed/pending ファインディングにヒットした場合は行/ノードを新規作成せず、evidence を追記、
// severity は高い方を採用、summary は置換してアーカイブし、系譜辺は既存ノードを指す。
// 異種/主資産違い/終了状態/資産なしはいずれもマージしない。PG が必要（testDSN 未設定なら skip）。
func TestRecordFindingTxMerge(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()

	tk, err := d.CreateTask("マージテスト", "ゴール", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer d.DeleteTask(tk.ID)
	es := d.Exploration(tk.ExplorationID)

	mkAsset := func(domain string) int64 {
		t.Helper()
		var id int64
		if err := d.QueryRow(`INSERT INTO assets(type,domain,root_domain,task_ids,last_seen)
			VALUES ('root_domain',$1,$1,ARRAY[$2]::bigint[],now()) RETURNING id`, domain, tk.ID).Scan(&id); err != nil {
			t.Fatal(err)
		}
		return id
	}
	assetA := mkAsset("merge-a.invalid")
	assetB := mkAsset("merge-b.invalid")
	defer func() { _, _ = d.Exec(`DELETE FROM assets WHERE id=ANY($1::bigint[])`, []int64{assetA, assetB}) }()

	mkFinding := func(vulnclass, status string, assets []int64) (fid, nodeID int64) {
		t.Helper()
		var err error
		nodeID, err = es.AddNode(KindFinding, map[string]any{
			"vulnclass": vulnclass, "severity": "medium", "summary": "旧要約",
			"evidence": map[string]any{"by": "w1", "poc": "旧エビデンス"},
		}, 9, "confirmed", "w1", nil)
		if err != nil {
			t.Fatal(err)
		}
		fid, err = d.AddFinding(tk.ID, nodeID, vulnclass, "", "medium", "旧要約", "旧エビデンス", "w1", assets)
		if err != nil {
			t.Fatal(err)
		}
		if status != FindingPending {
			if _, err = d.SetFindingStatus(fid, status); err != nil {
				t.Fatal(err)
			}
		}
		return
	}
	record := func(in RecordFindingInput) *RecordedFinding {
		t.Helper()
		in.TaskID, in.ExplorationID = tk.ID, tk.ExplorationID
		var out *RecordedFinding
		err := d.WithEvidenceTx(t.Context(), func(tx *sql.Tx) error {
			var e error
			out, e = RecordFindingTx(tx, in, nil)
			return e
		})
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	intentID, err := es.AddNode(KindIntent, map[string]any{"summary": "深掘り悪用"}, 5, "open", "planner", nil)
	if err != nil {
		t.Fatal(err)
	}

	// 1) ヒット：同一タスク、confirmed、"SQL-Injection"≈"sqli"、主資産が同一 → マージ
	fid, nodeID := mkFinding("SQL-Injection", FindingConfirmed, []int64{assetA})
	out := record(RecordFindingInput{
		VulnClass: "sqli", Severity: "critical", Summary: "認証情報奪取後の深掘り悪用",
		Evidence: "新 PoC 出力", Worker: "w2", IntentID: intentID, AssetIDs: []int64{assetA},
	})
	if !out.Merged || out.FindingID != fid || out.NodeID != nodeID {
		t.Fatalf("want merge into (%d,%d), got %+v", fid, nodeID, out)
	}
	if out.MergedIntoFindingID != fid || out.MergedIntoNodeID != nodeID {
		t.Fatalf("merged_into fields: %+v", out)
	}
	f, err := d.GetFinding(fid)
	if err != nil || f == nil {
		t.Fatal(err)
	}
	if f.Severity != "critical" || f.Summary != "認証情報奪取後の深掘り悪用" {
		t.Fatalf("merged fields: severity=%q summary=%q", f.Severity, f.Summary)
	}
	for _, want := range []string{"旧エビデンス", "[原摘要]", "旧要約", "[新证据]", "新 PoC 出力", "intent #"} {
		if !strings.Contains(f.Evidence, want) {
			t.Errorf("merged evidence missing %q:\n%s", want, f.Evidence)
		}
	}
	// 系譜辺 intent --yields--> 既存ノード
	var edges int
	if err := d.QueryRow(`SELECT count(*) FROM exploration_edges
		WHERE exploration_id=$1 AND src_id=$2 AND rel=$3 AND dst_id=$4`,
		tk.ExplorationID, intentID, RelYields, nodeID).Scan(&edges); err != nil || edges != 1 {
		t.Fatalf("yields edge: count=%d err=%v", edges, err)
	}
	// ノードの payload を同期
	var payload string
	if err := d.QueryRow(`SELECT payload::text FROM exploration_nodes WHERE id=$1`, nodeID).Scan(&payload); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(payload, "認証情報奪取後の深掘り悪用") || !strings.Contains(payload, "critical") {
		t.Errorf("node payload not synced: %s", payload)
	}
	// 同一 intent で再報告：辺は冪等、引き続きマージされる
	out2 := record(RecordFindingInput{
		VulnClass: "SQLi", Severity: "high", Summary: "認証情報奪取後の深掘り悪用",
		Evidence: "さらに別のエビデンス", Worker: "w2", IntentID: intentID, AssetIDs: []int64{assetA},
	})
	if !out2.Merged || out2.FindingID != fid {
		t.Fatalf("re-merge: %+v", out2)
	}
	if err := d.QueryRow(`SELECT count(*) FROM exploration_edges
		WHERE exploration_id=$1 AND src_id=$2 AND rel=$3 AND dst_id=$4`,
		tk.ExplorationID, intentID, RelYields, nodeID).Scan(&edges); err != nil || edges != 1 {
		t.Fatalf("edge idempotency: count=%d err=%v", edges, err)
	}

	// 2) pending のプレースホルダもマージ対象（負荷試験で 0 件になった根因の修正）：主資産が同じで
	// より新しい pending 行がヒットし（ORDER BY id DESC）、エビデンスを集約する。status は pending のまま
	// 検証パイプライン（C2 の増分再検証）の判断に委ね、マージ時点で真偽を先に決めない。
	pfid, _ := mkFinding("sqli", FindingPending, []int64{assetA})
	out3 := record(RecordFindingInput{
		VulnClass: "SQL Injection (Blind)", Severity: "low", Summary: "別の 1 件",
		Evidence: "pending マージ時エビデンス", AssetIDs: []int64{assetA},
	})
	if !out3.Merged || out3.FindingID != pfid {
		t.Fatalf("pending must merge into %d: %+v", pfid, out3)
	}
	pf, err := d.GetFinding(pfid)
	if err != nil || pf == nil {
		t.Fatal(err)
	}
	if pf.Status != FindingPending {
		t.Fatalf("merge must not change status, got %q", pf.Status)
	}
	if !strings.Contains(pf.Evidence, "pending マージ時エビデンス") ||
		!strings.Contains(pf.Evidence, `本次 vulnclass 原文 "SQL Injection (Blind)"`) {
		t.Errorf("pending merged evidence missing segment/drift note:\n%s", pf.Evidence)
	}

	// 3) 正規化後に別カテゴリ → 新規作成
	mkFinding("xss", FindingConfirmed, []int64{assetA})
	out4 := record(RecordFindingInput{
		VulnClass: "ssrf", Severity: "low", Summary: "ssrf", AssetIDs: []int64{assetA},
	})
	if out4.Merged {
		t.Fatalf("different class must not merge: %+v", out4)
	}
	defer func() { _, _ = d.DeleteFinding(out4.FindingID) }()

	// 4) 終了状態は除外し主資産も異なる：assetB 上に false_positive の sqli が 1 件あるが
	// 終了状態はマージ対象外。assetB 上に confirmed/pending の sqli はない → 新規作成
	fpfid, _ := mkFinding("sqli", FindingFalsePositive, []int64{assetB})
	out5 := record(RecordFindingInput{
		VulnClass: "sqli", Severity: "low", Summary: "別のアセット", AssetIDs: []int64{assetB},
	})
	if out5.Merged || out5.FindingID == fpfid {
		t.Fatalf("terminal state must not merge (fp fid=%d): %+v", fpfid, out5)
	}
	defer func() { _, _ = d.DeleteFinding(out5.FindingID) }()

	// 5) 資産なし → マージキーが不完全なので新規作成
	out6 := record(RecordFindingInput{VulnClass: "sqli", Severity: "low", Summary: "アセットなし"})
	if out6.Merged {
		t.Fatalf("asset-less report must not merge: %+v", out6)
	}
	defer func() { _, _ = d.DeleteFinding(out6.FindingID) }()

	// 6) マージキーは主資産の一致（asset_ids->0）であり、包含ではない：
	// (a) 候補 [ep,svc] 対 入力 [svc] —— svc は候補の副資産にすぎず主資産が異なるのでマージしない。
	// (b) 候補 [ep,svc] 対 入力 [ep] —— 主資産が同一なのでマージする。
	epAsset := mkAsset("merge-ep.invalid")
	svcAsset := mkAsset("merge-svc.invalid")
	defer func() { _, _ = d.Exec(`DELETE FROM assets WHERE id=ANY($1::bigint[])`, []int64{epAsset, svcAsset}) }()

	mfid, _ := mkFinding("csrf", FindingConfirmed, []int64{epAsset, svcAsset})
	out7 := record(RecordFindingInput{
		VulnClass: "csrf", Severity: "low", Summary: "service に紐づく別の 1 件",
		AssetIDs: []int64{svcAsset},
	})
	if out7.Merged {
		t.Fatalf("secondary-asset containment must not merge (candidate fid=%d): %+v", mfid, out7)
	}
	defer func() { _, _ = d.DeleteFinding(out7.FindingID) }()

	out8 := record(RecordFindingInput{
		VulnClass: "CSRF", Severity: "medium", Summary: "同一入口での深掘り",
		AssetIDs: []int64{epAsset},
	})
	if !out8.Merged || out8.FindingID != mfid {
		t.Fatalf("same primary asset must merge into %d: %+v", mfid, out8)
	}
}
