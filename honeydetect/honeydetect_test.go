package honeydetect

import (
	"os"
	"path/filepath"
	"testing"
)

// writeSigs は一時ディレクトリにシグネチャファイルを書き出し、パスを返す。
func writeSigs(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "honeypot.json")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const testSigs = `{"signatures":[
  {"name":"cowrie_default_ssh_banner","product":"Cowrie","confidence":0.99,"source":"t","match":{"banner_substr":"OpenSSH_6.0p1 Debian-4+deb7u2"}},
  {"name":"hfish_default_web_xjs","product":"HFish","confidence":0.85,"source":"t","match":{"port":8080,"http_body_substr":"x.js"}},
  {"name":"dionaea_default_cert_subject","product":"Dionaea","confidence":0.95,"source":"t","match":{"cert_subject_substr":"Nepenthes Development Team"}},
  {"name":"empty_match_never_hits","product":"X","confidence":1.0,"source":"t","match":{}}
]}`

func TestEvaluateHitsHighestConfidence(t *testing.T) {
	e := NewEngine(writeSigs(t, testSigs))
	score, ev := e.Evaluate(ServiceView{Port: 22, Banner: "SSH-2.0-OpenSSH_6.0p1 Debian-4+deb7u2"})
	if score != 0.99 || len(ev) != 1 {
		t.Fatalf("cowrie banner: score=%v ev=%v", score, ev)
	}
}

func TestEvaluateAllConditionsMustMatch(t *testing.T) {
	e := NewEngine(writeSigs(t, testSigs))
	// ポートが違うため body が命中してもカウントしない
	if score, _ := e.Evaluate(ServiceView{Port: 80, Body: "see x.js"}); score != 0 {
		t.Fatalf("port mismatch should not hit, score=%v", score)
	}
	// ポートと body の両方が命中したときだけカウントする
	if score, _ := e.Evaluate(ServiceView{Port: 8080, Body: "see x.js"}); score != 0.85 {
		t.Fatalf("port+body hit: score=%v", score)
	}
}

func TestEvaluateCertAndCaseFold(t *testing.T) {
	e := NewEngine(writeSigs(t, testSigs))
	score, _ := e.Evaluate(ServiceView{CertSubject: "C=DE, CN=nepenthes development team, O=dionaea.carnivore.it"})
	if score != 0.95 {
		t.Fatalf("cert subject case-fold hit: score=%v", score)
	}
}

func TestEvaluateNoMatch(t *testing.T) {
	e := NewEngine(writeSigs(t, testSigs))
	score, ev := e.Evaluate(ServiceView{Port: 22, Banner: "SSH-2.0-OpenSSH_8.9p1 Ubuntu-3ubuntu0.6"})
	if score != 0 || ev != nil {
		t.Fatalf("no match: score=%v ev=%v", score, ev)
	}
}

func TestMissingOrCorruptFileDegradesQuietly(t *testing.T) {
	e := NewEngine(filepath.Join(t.TempDir(), "nonexistent.json"))
	if score, _ := e.Evaluate(ServiceView{Banner: "OpenSSH_6.0p1 Debian-4+deb7u2"}); score != 0 {
		t.Fatalf("missing file should score 0, got %v", score)
	}
	e2 := NewEngine(writeSigs(t, `{not json`))
	if score, _ := e2.Evaluate(ServiceView{Banner: "OpenSSH_6.0p1 Debian-4+deb7u2"}); score != 0 {
		t.Fatalf("corrupt file should score 0, got %v", score)
	}
}

func TestHotReloadByMtime(t *testing.T) {
	p := writeSigs(t, `{"signatures":[]}`)
	e := NewEngine(p)
	if score, _ := e.Evaluate(ServiceView{Banner: "OpenSSH_6.0p1 Debian-4+deb7u2"}); score != 0 {
		t.Fatalf("empty sigs should score 0, got %v", score)
	}
	// ファイルを書き直す（mtime の変化を観測できるようスリープする）。再起動なしで
	// ホットリロードされるはず。
	if err := os.Chtimes(p, mustStat(t, p).ModTime(), mustStat(t, p).ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(testSigs), 0o644); err != nil {
		t.Fatal(err)
	}
	if score, _ := e.Evaluate(ServiceView{Banner: "OpenSSH_6.0p1 Debian-4+deb7u2"}); score != 0.99 {
		t.Fatalf("hot reload should pick up new sigs, score=%v", score)
	}
}

func mustStat(t *testing.T, p string) os.FileInfo {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi
}

// TestBundledSignaturesLoad リポジトリの埋め込みシグネチャライブラリは解析
// 可能で、各エントリにソースと条件があること。
func TestBundledSignaturesLoad(t *testing.T) {
	sigs := parseSignatures(embeddedSignatures)
	if len(sigs) == 0 {
		t.Fatal("埋め込みシグネチャライブラリが空か解析に失敗しました")
	}
	for _, s := range sigs {
		if s.Name == "" || s.Product == "" || s.Source == "" {
			t.Fatalf("シグネチャに name/product/source がありません: %+v", s)
		}
		if s.Confidence < 0.85 || s.Confidence > 0.99 {
			t.Fatalf("シグネチャ %s の信頼度 %.2f が 0.85-0.99 の範囲外です", s.Name, s.Confidence)
		}
		if s.Match.empty() {
			t.Fatalf("シグネチャ %s にマッチ条件がありません", s.Name)
		}
	}
	// 埋め込みライブラリはシグネチャファイル欠落時のフォールバックとして返る
	// はず（2 条を抽出して実際の命中を検証）。
	e := NewEngine(filepath.Join(t.TempDir(), "nonexistent.json"))
	score, _ := e.Evaluate(ServiceView{Port: 22, Banner: "SSH-2.0-OpenSSH_6.0p1 Debian-4+deb7u2"})
	if score != 0.99 {
		t.Fatalf("埋め込み Cowrie シグネチャが命中しませんでした, score=%v", score)
	}
	score, _ = e.Evaluate(ServiceView{CertIssuer: "C=DE, CN=Nepenthes Development Team, O=dionaea.carnivore.it, OU=anv"})
	if score != 0.9 {
		t.Fatalf("埋め込み Dionaea の issuer シグネチャが命中しませんでした, score=%v", score)
	}
}
