package agent

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/harness"
)

// 高速化用の設定：しきい値を絞り、テストで何十ステップも与えずに済ませる。
func testStuckConfig() StuckConfig {
	return StuckConfig{
		Window: 10, SigPrefix: 200,
		SameSigTrigger: 3, FailTrigger: 4, FailSimilarity: 0.8,
		Cooldown: 2, MaxFires: 3,
	}
}

func TestNormalizeStuckOutput(t *testing.T) {
	base := normalizeStuckOutput("malloc(): memory corruption\nAborted (core dumped)")
	if got := normalizeStuckOutput("malloc(): memory corruption\nAborted (core dumped)"); got != base {
		t.Fatalf("同一テキストの正規化が一致しない: %q vs %q", got, base)
	}
	// タイムスタンプだけが違い構造が同じ出力は同じに正規化される（プレースホルダ <ts> が位置情報を残す）。
	ts1 := normalizeStuckOutput("[2026-07-01T13:45:02Z] malloc(): memory corruption\nAborted (core dumped)")
	ts2 := normalizeStuckOutput("[2026/09/13 08:01:59] malloc(): memory corruption\nAborted (core dumped)")
	if ts1 != ts2 {
		t.Errorf("タイムスタンプ違いの正規化は一致すべき:\n a=%q\n b=%q", ts1, ts2)
	}
	if strings.ContainsAny(ts1, "0123456789") {
		t.Errorf("タイムスタンプの正規化後に数字が残ってはならない: %q", ts1)
	}
	// 数値の変種は正規化後に # で置換され、異なる数値同士も同じに正規化される
	a := normalizeStuckOutput("request id 88123 failed after 3 retries")
	b := normalizeStuckOutput("request id 41007 failed after 9 retries")
	if a != b {
		t.Errorf("数値は正規化されるべき: %q vs %q", a, b)
	}
	if strings.ContainsAny(a, "0123456789") {
		t.Errorf("正規化後に数字が残ってはならない: %q", a)
	}
}

func TestNormalizeStuckOutputPathsAndRandom(t *testing.T) {
	a := normalizeStuckOutput("open /tmp/sess_a1b2c3d4/upload_9912.php: permission denied")
	b := normalizeStuckOutput("open /tmp/sess_zz99ww88/upload_1234.php: permission denied")
	if a != b {
		t.Errorf("パス+ランダム文字列は正規化されるべき:\n a=%q\n b=%q", a, b)
	}
	c := normalizeStuckOutput(`token=4f8b2c1d9e0a4f8b2c1d9e0a4f8b2c1d status=403 forbidden`)
	d := normalizeStuckOutput(`token=aa00bb11cc22dd33ee44ff5566778899 status=403 forbidden`)
	if c != d {
		t.Errorf("長い hex トークンは正規化されるべき:\n c=%q\n d=%q", c, d)
	}
}

func TestStuckSameSignatureTriggers(t *testing.T) {
	d := NewStuckDetector(testStuckConfig())
	out := "HTTP/1.1 403 Forbidden\r\nContent-Length: 291\r\n\r\n<html>blocked by waf</html>"
	for i := 1; i <= 2; i++ {
		if ev := d.Observe("Bash", out, false); ev != nil {
			t.Fatalf("第 %d ステップでは発火してはならない(しきい値 3)", i)
		}
	}
	ev := d.Observe("Bash", out, false)
	if ev == nil {
		t.Fatal("同一シグネチャが 3 回連続したら発火すべき")
	}
	if ev.Kind != "same_signature" || ev.Count != 3 || ev.N != 1 {
		t.Errorf("イベントのフィールドが一致しない: %+v", ev)
	}
	msg, ok := d.Drain()
	if !ok || !strings.Contains(msg, "【停滞检测】") {
		t.Errorf("発火後は注入待ちメッセージがあるべき: %q, %v", msg, ok)
	}
	if _, ok := d.Drain(); ok {
		t.Error("注入待ちメッセージは 1 件だけであるべき")
	}
}

func TestStuckDifferentSignaturesNoTrigger(t *testing.T) {
	d := NewStuckDetector(testStuckConfig())
	outs := []string{
		"HTTP/1.1 200 OK\r\n\r\nlogin page",
		"HTTP/1.1 302 Found\r\nLocation: /home",
		"HTTP/1.1 404 Not Found\r\n\r\nno such route",
		"HTTP/1.1 500 Internal Server Error",
		"HTTP/1.1 200 OK\r\n\r\nadmin panel",
		"HTTP/1.1 401 Unauthorized\r\nWWW-Authenticate: Basic",
	}
	for i, out := range outs {
		if ev := d.Observe("WebFetch", out, false); ev != nil {
			t.Fatalf("第 %d ステップ、シグネチャが違えば発火しない: %+v", i+1, ev)
		}
	}
}

func TestStuckSignatureToleratesVolatileFields(t *testing.T) {
	// R4 のシナリオ：同一のクラッシュ署名で、毎回 PID/アドレスだけが違う —— 同質と認識されるべき。
	d := NewStuckDetector(testStuckConfig())
	for i := 1; i <= 3; i++ {
		out := fmt.Sprintf("malloc(): memory corruption (pid %d)\n[2026-07-01 13:%02d:11] Aborted (core dumped)", 4000+i, i)
		ev := d.Observe("Bash", out, true)
		if i < 3 && ev != nil {
			t.Fatalf("第 %d ステップでは発火してはならない", i)
		}
		if i == 3 && ev == nil {
			t.Fatal("同一のクラッシュシグネチャが 3 回続いたら発火すべき")
		}
	}
}

func TestStuckCooldown(t *testing.T) {
	cfg := testStuckConfig()
	d := NewStuckDetector(cfg)
	out := "same response over and over"
	steps := cfg.SameSigTrigger + cfg.Cooldown + 1 // 発火 + 冷却期間 + 冷却明けの 1 ステップ目
	var fires []int
	for i := 1; i <= steps; i++ {
		if ev := d.Observe("Bash", out, false); ev != nil {
			fires = append(fires, i)
		}
	}
	if len(fires) != 2 {
		t.Fatalf("2 回発火すべき(第 %d ステップ + クールダウン %d ステップ後), 実際は %v", cfg.SameSigTrigger, cfg.Cooldown, fires)
	}
	if fires[0] != cfg.SameSigTrigger {
		t.Errorf("最初の発火は第 %d ステップであるべき, 実際は第 %d ステップ", cfg.SameSigTrigger, fires[0])
	}
	if fires[1] != cfg.SameSigTrigger+cfg.Cooldown+1 {
		t.Errorf("クールダウン %d ステップ内は発火しない, 2 回目は第 %d ステップであるべき, 実際は %v", cfg.Cooldown, cfg.SameSigTrigger+cfg.Cooldown+1, fires)
	}
}

func TestStuckMaxFires(t *testing.T) {
	cfg := testStuckConfig()
	d := NewStuckDetector(cfg)
	out := "identical failure output"
	fires := 0
	// 十分なステップ数を与える：上限がなければ Cooldown+1 ステップごとに再発火する
	for i := 0; i < 100; i++ {
		if ev := d.Observe("Bash", out, false); ev != nil {
			fires++
			if ev.N != fires {
				t.Errorf("イベント番号は増加すべき: got %d, want %d", ev.N, fires)
			}
		}
	}
	if fires != cfg.MaxFires {
		t.Errorf("発火回数は上限 %d で打ち切られるべき, 実際は %d", cfg.MaxFires, fires)
	}
	if d.Fires() != cfg.MaxFires {
		t.Errorf("Fires() = %d, want %d", d.Fires(), cfg.MaxFires)
	}
}

func TestStuckConsecutiveFailuresSimilar(t *testing.T) {
	cfg := testStuckConfig()
	cfg.SameSigTrigger = 100 // 同一署名の経路を無効化し、同ツール連続失敗＋類似度の経路だけを見る
	d := NewStuckDetector(cfg)
	outs := []string{
		"Error: access denied for user 'root'@'10.0.0.11' (using password: YES)",
		"Error: access denied for user 'admin'@'10.0.0.12' (using password: YES)",
		"Error: access denied for user 'test'@'10.0.0.13' (using password: YES)",
		"Error: access denied for user 'backup'@'10.0.0.14' (using password: YES)",
	}
	for i, out := range outs {
		ev := d.Observe("Bash", out, true)
		if i < len(outs)-1 && ev != nil {
			t.Fatalf("第 %d ステップでは発火してはならない", i+1)
		}
		if i == len(outs)-1 {
			if ev == nil {
				t.Fatal("同じツールで類似した失敗が 4 回連続したら発火すべき")
			}
			if ev.Kind != "same_tool_failure" {
				t.Errorf("Kind = %q, want same_tool_failure", ev.Kind)
			}
		}
	}
}

func TestStuckConsecutiveFailuresDissimilarNoTrigger(t *testing.T) {
	cfg := testStuckConfig()
	cfg.SameSigTrigger = 100 // 失敗経路だけを見る
	d := NewStuckDetector(cfg)
	outs := []string{
		"Error: access denied for user 'root'@'10.0.0.11'",
		"segfault at 0x7f2a1b3c ip 00007f2a sp 00007fff error 4 in libc",
		"connection timed out after 30000 milliseconds",
		"panic: runtime error: index out of range [5] with length 2",
		"dns resolve failed: no such host",
	}
	for i, out := range outs {
		if ev := d.Observe("Bash", out, true); ev != nil {
			t.Fatalf("第 %d ステップ、出力が類似していなければ発火しない: %+v", i+1, ev)
		}
	}
}

func TestStuckSuccessResetsFailureStreak(t *testing.T) {
	cfg := testStuckConfig()
	cfg.SameSigTrigger = 100
	d := NewStuckDetector(cfg)
	fail := "Error: access denied for user 'root'@'10.0.0.11' (using password: YES)"
	for i := 0; i < cfg.FailTrigger-1; i++ {
		d.Observe("Bash", fail, true)
	}
	d.Observe("Bash", "total 48\ndrwxr-xr-x 2 root root 4096 bin", false) // 1 回の成功で連敗を断ち切る
	for i := 0; i < cfg.FailTrigger-1; i++ {
		if ev := d.Observe("Bash", fail, true); ev != nil {
			t.Fatalf("連続失敗が途切れて再カウント, 第 %d ステップでは発火してはならない", i+1)
		}
	}
	if ev := d.Observe("Bash", fail, true); ev == nil {
		t.Fatal("再びしきい値に達したら発火すべき")
	}
}

func TestStuckInterventionMessage(t *testing.T) {
	msg := StuckIntervention(StuckEvent{Tool: "Bash", Kind: "same_signature", Count: 12, Signature: "abcd1234", N: 1})
	for _, want := range []string{"【停滞检测】", "信息梯度为零", "换维度", "榨干", "请求转向", "12", "abcd1234"} {
		if !strings.Contains(msg, want) {
			t.Errorf("介入メッセージに %q がない:\n%s", want, msg)
		}
	}
}

func TestStuckPivotHintAppended(t *testing.T) {
	d := NewStuckDetector(testStuckConfig())
	d.PivotHint = "類似シナリオで選べる方向転換ルート(テスト):\n- [web] エンコード/位置/Content-Type を変える"
	out := "HTTP/1.1 403 Forbidden\r\n\r\n<html>blocked</html>"
	for i := 0; i < 3; i++ {
		d.Observe("Bash", out, false)
	}
	msg, ok := d.Drain()
	if !ok {
		t.Fatal("発火後は注入待ちメッセージがあるべき")
	}
	if !strings.Contains(msg, "【停滞检测】") || !strings.Contains(msg, "[web] エンコード/位置/Content-Type を変える") {
		t.Errorf("PivotHint は汎用の介入メッセージの後に追加されるべき:\n%s", msg)
	}
	// PivotHint が空なら元のまま（汎用の文言のみ）。
	d2 := NewStuckDetector(testStuckConfig())
	for i := 0; i < 3; i++ {
		d2.Observe("Bash", out, false)
	}
	msg2, _ := d2.Drain()
	if strings.Contains(msg2, "選べる方向転換") {
		t.Errorf("PivotHint が空なら方向転換の提案段を出してはならない:\n%s", msg2)
	}
}

// ---------- wrapup 締めくくりセルフチェック段（chains L1 §4）----------

func TestWrapupSelfCheckWorker(t *testing.T) {
	s := wrapupSettlement("worker", []string{"Bash"})
	for _, want := range []string{"结束前对照自检", "record_fact", "被墙", "此路不通", "换什么维度"} {
		if !strings.Contains(s.Prompt, want) {
			t.Errorf("worker の締めくくり文にセルフチェック内容 %q がない:\n%s", want, s.Prompt)
		}
	}
	// 既存の本文は完全に残す必要がある（セルフチェック段は追加であり置換ではない）
	if !strings.HasPrefix(s.Prompt, settleWrapUpPrompt) {
		t.Error("セルフチェック段は既存の締めくくり文の後に追加されるべきで, 既存の本文は変えてはならない")
	}
}

func TestWrapupSelfCheckNotForOtherAgents(t *testing.T) {
	for _, key := range []string{"planner", "mainagent", "custom-x"} {
		if strings.Contains(wrapupSettlement(key, nil).Prompt, "结束前对照自检") {
			t.Errorf("%s の締めくくり文に worker のセルフチェック段を連結してはならない", key)
		}
	}
}

func TestWrapupSelfCheckTaskTimeoutVariant(t *testing.T) {
	s := wrapupSettlementForTask("worker", []string{"Bash"}, true)
	if s.PromptByReason == nil {
		t.Fatal("clamped のときは PromptByReason があるべき")
	}
	if !strings.Contains(s.PromptByReason[harness.ReasonTimeout], "结束前对照自检") {
		t.Error("タスクタイムアウト時の締めくくり文にもセルフチェック段が必要")
	}
	if !strings.Contains(s.PromptByReason[harness.ReasonMaxTurns], "结束前对照自检") {
		t.Error("per-run の締めくくり文にもセルフチェック段が必要")
	}
}
