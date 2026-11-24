package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// アプローチ A：同一入口の finding 自動マージ（FIXPLAN 要件一 / ROADMAP C2·C4）。
//
// RecordFindingTx は挿入前に (task_id, 正規化 vulnclass, 主 asset_id) で同一タスクの
// 未クローズかつ status IN ('confirmed','pending') の既存 finding を検索する。ヒット時は
// 行/ノードを新規作成せず、evidence を追記し、severity は最高を採用、summary は最新の
// 深掘り利用を優先（旧いものは evidence へ退避）、新 intent の yields 辺は既存ノードを
// 指し、トラフィックのスナップショットを既存 finding へ紐づける。重複判定と挿入は同じ
// タスク単位のエビデンスロック（LockTaskEvidenceTx）内にあり、自然に直列化されるため、
// 並行二重挿入の窓はない。
//
// pending もマージ対象とする根拠：負荷試験では 2 件目の報告が届く時点で 1 件目がまだ
// pending であることが多く、confirmed だけを対象にするとマージが 0 件になる。未検証の
// 結論もまず集約し、その後に検証パイプラインが判断する。C2 にはマージ後の増分再検証が
// ある（evidence.Store.OnMerged → server.RequestFindingCheck）。後に誤検知と裁定されても、
// false_positive 処理時にマージで入ったエビデンス段は evidence に残り監査できる。終了状態
// （fixed/false_positive/duplicate など）は除外し、集約を汚さない。
//
// マージ対象外のケース（いずれも従来の新規作成パスに戻る）：
//   - task なし（TaskID<=0）または主資産なし（AssetIDs が空）。マージキーが不完全;
//   - 候補の status が終了状態（fixed/false_positive/duplicate など）;
//   - 候補に探索ノードがない（node_id IS NULL）。マージは 1ノード↔1行 の契約を保つ。

// vulnClassAliases はよくある脆弱性カテゴリを粗い分類へ収束させる。未知の項目は
// そのまま小文字で保持する（判等には参加し、カテゴリを跨ぐマージはしない）。キーは
// 先に normalizeVulnClassKey と同じ折り畳み（小文字、空白/アンダースコア/ハイフンを
// 単一ハイフンに）を通す必要がある。
var vulnClassAliases = map[string]string{
	"sqli":                           "sqli",
	"sql-injection":                  "sqli",
	"sql-inj":                        "sqli",
	"sql":                            "sqli",
	"xss":                            "xss",
	"cross-site-scripting":           "xss",
	"ssrf":                           "ssrf",
	"server-side-request-forgery":    "ssrf",
	"ssti":                           "ssti",
	"template-injection":             "ssti",
	"server-side-template-injection": "ssti",
	"xxe":                            "xxe",
	"xml-external-entity":            "xxe",
	"cmdi":                           "cmdi",
	"rce":                            "cmdi",
	"command-injection":              "cmdi",
	"os-command-injection":           "cmdi",
	"remote-code-execution":          "cmdi",
	"code-execution":                 "cmdi",
	"upload":                         "upload",
	"file-upload":                    "upload",
	"arbitrary-file-upload":          "upload",
	"unrestricted-upload":            "upload",
	"auth-bypass":                    "auth-bypass",
	"authentication-bypass":          "auth-bypass",
	"broken-auth":                    "auth-bypass",
	"broken-authentication":          "auth-bypass",
	"logic":                          "logic",
	"business-logic":                 "logic",
	"deser":                          "deser",
	"deserialization":                "deser",
	"insecure-deserialization":       "deser",
	"info-leak":                      "info-leak",
	"information-disclosure":         "info-leak",
	"info-disclosure":                "info-leak",
	"sensitive-data-exposure":        "info-leak",
	"privesc":                        "privesc",
	"privilege-escalation":           "privesc",
	"priv-esc":                       "privesc",
	"other":                          "other",
}

// normalizeVulnClassKey は vulnclass を折り畳む：小文字化し、前後の空白を除去し、
// 空白/アンダースコア/ハイフン/スラッシュを単一ハイフンにする。別名表のキーも
// 曖昧一致段のキーワードも同じ折り畳み形式である。
func normalizeVulnClassKey(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	return strings.Join(strings.FieldsFunc(v, func(r rune) bool {
		return r == ' ' || r == '_' || r == '-' || r == '/'
	}), "-")
}

// vulnClassFuzzyKeywords は曖昧一致段のキーワード表。上から優先順に照合し、ヒット
// したら正規の粗い分類へ収束させる。保守的な方針：裸の "injection" は意図的に収録
// しない。SSTI などは "injection" を含むが SQLi ではない。"sql" 語根こそ sqli の
// 強いシグナルである。blind sqli は独立した sqli-blind ではなく sqli に寄せる：
// 既存の粗い分類と揃え、ブラインドは sqli の利用手法であって新しい分類ではない。
var vulnClassFuzzyKeywords = []struct {
	keys  []string
	class string
}{
	{[]string{"sql", "ブラインド"}, "sqli"},
	{[]string{"xss", "cross-site-script", "クロスサイトスクリプティング"}, "xss"},
	{[]string{"ssrf"}, "ssrf"},
	{[]string{"ssti", "template-injection", "テンプレートインジェクション"}, "ssti"},
	{[]string{"xxe"}, "xxe"},
	{[]string{"command-injection", "コマンドインジェクション", "rce", "code-execution", "リモートコード実行", "リモートコマンド実行", "コード実行", "コマンド実行"}, "cmdi"},
	{[]string{"file-upload", "ファイルアップロード", "アップロード"}, "upload"},
	{[]string{"file-inclusion", "lfi", "rfi", "ファイルインクルード"}, "lfi"},
	{[]string{"deserial", "デシリアライズ"}, "deser"},
	{[]string{"csrf"}, "csrf"},
	{[]string{"auth-bypass", "authentication-bypass", "認証バイパス", "認可バイパス"}, "auth-bypass"},
	{[]string{"brute", "ブルートフォース"}, "brute-force"},
	{[]string{"info-leak", "information-leak", "漏えい", "漏えい"}, "info-leak"},
	{[]string{"privesc", "privilege-escalation", "権限昇格"}, "privesc"},
	{[]string{"logic", "ロジック"}, "logic"},
}

// fuzzyVulnClass は折り畳み後の key にキーワード包含スキャンを行い、ヒット時は粗い分類を返す。
func fuzzyVulnClass(key string) (string, bool) {
	for _, kw := range vulnClassFuzzyKeywords {
		for _, k := range kw.keys {
			if strings.Contains(key, k) {
				return kw.class, true
			}
		}
	}
	return "", false
}

// parenContents は折り畳み後の key から各対の（半角/全角）括弧内の内容を抽出する。
// モデルが慣用する "Stored Cross-Site Scripting (XSS)" /
// "任意ファイルアップロード (CVE-2017-12615)" といった書き方に対応するため。
func parenContents(key string) []string {
	var out []string
	for _, pair := range [][2]rune{{'(', ')'}, {'（', '）'}} {
		rest := key
		for {
			i := strings.IndexRune(rest, pair[0])
			if i < 0 {
				break
			}
			rest = rest[i+1:]
			j := strings.IndexRune(rest, pair[1])
			if j < 0 {
				break
			}
			if inner := strings.TrimSpace(rest[:j]); inner != "" {
				out = append(out, inner)
			}
			rest = rest[j+1:]
		}
	}
	return out
}

// NormalizeVulnClass は脆弱性カテゴリを粗い分類へ正規化する。二段構え：
//  1. 別名表：大文字小文字/空白/ハイフンを折り畳んだ後に vulnClassAliases へ完全一致;
//  2. 曖昧一致段：未ヒット時はまず括弧内の内容、次に折り畳み後の全文を取り、それぞれ
//     別名表を再照会してからキーワード包含スキャンを行う（優先順位は vulnClassFuzzyKeywords）;
//  3. いずれも不中：折り畳み後の小文字をそのまま保持（判等には参加し、カテゴリを跨ぐマージはしない）。
func NormalizeVulnClass(v string) string {
	key := normalizeVulnClassKey(v)
	if canon, ok := vulnClassAliases[key]; ok {
		return canon
	}
	candidates := append(parenContents(key), key)
	for _, c := range candidates {
		if canon, ok := vulnClassAliases[c]; ok {
			return canon
		}
	}
	for _, c := range candidates {
		if canon, ok := fuzzyVulnClass(c); ok {
			return canon
		}
	}
	return key
}

// severityRank は severity を比較可能な値へ写像する。未知/空は 0（最小）。
func severityRank(s string) int {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case SeverityCritical:
		return 4
	case SeverityHigh:
		return 3
	case SeverityMedium:
		return 2
	case SeverityLow:
		return 1
	}
	return 0
}

// MaxSeverity は 2 つのうち高い severity を返す。同格またはどちらも未知なら a を返す。
func MaxSeverity(a, b string) string {
	if severityRank(b) > severityRank(a) {
		return b
	}
	return a
}

// appendMergeSegment は evidence の末尾にソースマーカー付きの段落を追加する。note が
// 非空なら段落マーカーへ付加する（現在は vulnclass の表現揺らぎ監査に使用）。
func appendMergeSegment(evidence, label, body string, now time.Time, intentID int64, worker, note string) string {
	marker := fmt.Sprintf("\n\n--- [%s] マージ日時 %s", label, now.UTC().Format(time.RFC3339))
	if intentID > 0 {
		marker += fmt.Sprintf(", intent #%d", intentID)
	}
	if worker = strings.TrimSpace(worker); worker != "" {
		marker += ", by " + worker
	}
	if note != "" {
		marker += note
	}
	return evidence + marker + " ---\n" + body
}

// mergeFindingFields はマージ後の severity/summary/evidence を計算する（純粋関数で
// テスト可能）：severity は最高を採用。新 summary が旧と異なれば「最新の深掘り利用」を
// 優先して置換し、旧いものは evidence へ退避。新 evidence はタイムスタンプと intent
// マーカー付きの段落として追加し、原文は保持する。今回報告の vulnclass 原文が既存行と
// 異なる場合（正規化後に等しい、モデルの表現揺らぎにすぎない）は、追加する段落マーカーに
// 原文を注記して監査しやすくする。
func mergeFindingFields(old DBFinding, in RecordFindingInput, now time.Time) (severity, summary, evidence string) {
	severity = MaxSeverity(old.Severity, in.Severity)
	summary = old.Summary
	evidence = old.Evidence
	drift := ""
	if iv, ov := strings.TrimSpace(in.VulnClass), strings.TrimSpace(old.VulnClass); iv != "" && ov != "" && iv != ov {
		drift = fmt.Sprintf(", 今回 vulnclass 原文 %q", iv)
	}
	appended := false
	if ns := strings.TrimSpace(in.Summary); ns != "" && ns != strings.TrimSpace(old.Summary) {
		if s := strings.TrimSpace(old.Summary); s != "" {
			evidence = appendMergeSegment(evidence, "元の要約", s, now, in.IntentID, in.Worker, drift)
			appended = true
		}
		summary = in.Summary
	}
	if e := strings.TrimSpace(in.Evidence); e != "" {
		evidence = appendMergeSegment(evidence, "新たな証拠", in.Evidence, now, in.IntentID, in.Worker, drift)
		appended = true
	}
	// 付けられる段落はないが表現揺らぎはある：監査用の段落を単独で残し、揺らぎの痕跡を失わない。
	if drift != "" && !appended {
		evidence = appendMergeSegment(evidence, "新たな証拠", "(vulnclass の表現揺らぎのみ、新たな証拠本文なし)", now, in.IntentID, in.Worker, drift)
	}
	return
}

// findMergeCandidateTx はトランザクション内（呼び出し側はタスク単位のエビデンス
// ロックを保持済みであること）でマージ可能な既存 finding を検索する：同一 task、
// status IN ('confirmed','pending')（終了状態は除外。根拠はファイル冒頭のコメント）、
// 正規化 vulnclass が同一、主資産が一致（候補の asset_ids->0 = 入力 AssetIDs の
// 先頭）、かつ探索ノードがあるもの。複数行ヒット時は最新の 1 件を取る。
// 主資産の一致であり包含ではない：候補 [ep,svc] と入力 [svc] は同一入口ではない
// （svc は候補の副資産にすぎない）ため、包含で判等すると service に紐づく別入口の
// 脆弱性を誤って併合してしまう。[ep,svc] と [ep] は主資産が同じなので正しく
// マージされる。asset_ids が空、または入力の主資産が空なら ->0 は NULL となり、
// 自然にマージ対象外となる。
func findMergeCandidateTx(tx *sql.Tx, in RecordFindingInput) (*DBFinding, error) {
	if in.TaskID <= 0 || len(in.AssetIDs) == 0 {
		return nil, nil
	}
	want := NormalizeVulnClass(in.VulnClass)
	rows, err := tx.Query(`SELECT id, node_id, vulnclass, severity, summary, evidence
FROM findings
WHERE task_id=$1 AND status IN ($2, $3) AND node_id IS NOT NULL
  AND asset_ids->0 = to_jsonb($4::bigint)
ORDER BY id DESC
FOR UPDATE`, in.TaskID, FindingConfirmed, FindingPending, in.AssetIDs[0])
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var f DBFinding
		if err := rows.Scan(&f.ID, &f.NodeID, &f.VulnClass, &f.Severity, &f.Summary, &f.Evidence); err != nil {
			return nil, err
		}
		if NormalizeVulnClass(f.VulnClass) == want {
			return &f, rows.Err()
		}
	}
	return nil, rows.Err()
}

// mergeFindingTx はマージを実行する：既存 finding 行とノードの payload を更新し、
// 系譜辺とトラフィックのスナップショットを追加し、既存レコードを指す
// RecordedFinding（Merged=true）を返す。
func mergeFindingTx(tx *sql.Tx, cand *DBFinding, in RecordFindingInput, prepared []PreparedTrafficEvidence) (*RecordedFinding, error) {
	severity, summary, evidence := mergeFindingFields(*cand, in, time.Now())
	if _, err := tx.Exec(`UPDATE findings SET severity=$2, summary=$3, evidence=$4 WHERE id=$1`,
		cand.ID, severity, summary, evidence); err != nil {
		return nil, err
	}
	// ノードの payload を同期（setFindingCol と同じ jsonb_set）。タスク別の
	// ファインディングタブ（ノード payload を読む）と findings 表の一致を保つ。
	if _, err := tx.Exec(`UPDATE exploration_nodes
SET payload = jsonb_set(jsonb_set(jsonb_set(payload, '{severity}', to_jsonb($2::text)),
	'{summary}', to_jsonb($3::text)), '{evidence,poc}', to_jsonb($4::text))
WHERE id=$1 AND kind='finding'`, *cand.NodeID, severity, summary, evidence); err != nil {
		return nil, err
	}
	// 系譜：新 intent の yields 辺は既存ノードを指す（「同一入口が繰り返し深掘り
	// された」履歴を保つ）。対象ノードはこの exploration に属する必要がある。辺の
	// 主キーにより自然に冪等。
	if in.IntentID > 0 {
		if _, err := tx.Exec(`INSERT INTO exploration_edges(exploration_id,src_id,rel,dst_id)
SELECT $1,$2,$3,$4 WHERE EXISTS(SELECT 1 FROM exploration_nodes WHERE id=$4 AND exploration_id=$1)
ON CONFLICT DO NOTHING`, in.ExplorationID, in.IntentID, RelYields, *cand.NodeID); err != nil {
			return nil, err
		}
	}
	if err := AddFindingTrafficTx(tx, cand.ID, prepared); err != nil {
		return nil, err
	}
	// テキストエビデンス/summary の変化も既存レポートを陳腐化させる（トラフィック
	// 紐づけと同じバージョン列を共用）。
	if err := bumpEvidenceVersionTx(tx, cand.ID); err != nil {
		return nil, err
	}
	traffic, err := FindingTrafficTx(tx, cand.ID)
	if err != nil {
		return nil, err
	}
	return &RecordedFinding{
		FindingID:           cand.ID,
		NodeID:              *cand.NodeID,
		Traffic:             traffic,
		Merged:              true,
		MergedIntoFindingID: cand.ID,
		MergedIntoNodeID:    *cand.NodeID,
	}, nil
}
