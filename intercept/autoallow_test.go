package intercept

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/Autumn-27/artex/db"
)

func openTestDB(t *testing.T) *db.DB {
	t.Helper()
	dsn, _, err := db.DSN()
	if err != nil {
		t.Skipf("no database config (%v) — skipping", err)
	}
	d, err := db.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// resetAutoAllow disables the switch before and after the test so a failure
// never leaves the shared dev DB auto-approving asks.
func resetAutoAllow(t *testing.T, ic *Interceptor) {
	t.Helper()
	if err := ic.SetAutoAllow(false, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ic.SetAutoAllow(false, 0) })
}

func TestAutoAllowHoursClamp(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{0, 2}, {1, 2}, {2, 2}, {3, 4}, {4, 4}, {5, 8}, {8, 8}, {100, 8},
	} {
		if got := autoAllowHours(tc.in); got != tc.want {
			t.Errorf("autoAllowHours(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestAutoAllowSetAndStatus(t *testing.T) {
	d := openTestDB(t)
	ic := New(d)
	resetAutoAllow(t, ic)

	if st := ic.AutoAllowStatus(); st.Enabled || st.ExpiresAt != 0 || st.RemainingSeconds != 0 {
		t.Fatalf("default status should be disabled: %+v", st)
	}

	if err := ic.SetAutoAllow(true, 4); err != nil {
		t.Fatal(err)
	}
	st := ic.AutoAllowStatus()
	if !st.Enabled {
		t.Fatal("auto-allow should be enabled")
	}
	wantExp := time.Now().Add(4 * time.Hour).Unix()
	if st.ExpiresAt < wantExp-60 || st.ExpiresAt > wantExp {
		t.Fatalf("expires_at %d, want ~%d", st.ExpiresAt, wantExp)
	}
	if st.RemainingSeconds <= 4*3600-60 || st.RemainingSeconds > 4*3600 {
		t.Fatalf("remaining_seconds %d, want ~%d", st.RemainingSeconds, 4*3600)
	}

	// hours は {2,4,8} にクランプされる：99 → 8h
	if err := ic.SetAutoAllow(true, 99); err != nil {
		t.Fatal(err)
	}
	if st := ic.AutoAllowStatus(); st.ExpiresAt < time.Now().Add(8*time.Hour).Unix()-60 {
		t.Fatalf("hours=99 should clamp to 8h, got expires_at %d", st.ExpiresAt)
	}

	// enabled=false のときは hours を無視し、失効時刻をクリアする
	if err := ic.SetAutoAllow(false, 8); err != nil {
		t.Fatal(err)
	}
	if st := ic.AutoAllowStatus(); st.Enabled || st.ExpiresAt != 0 {
		t.Fatalf("disable should clear state: %+v", st)
	}
}

func TestAutoAllowExpiryAutoDisables(t *testing.T) {
	d := openTestDB(t)
	ic := New(d)
	resetAutoAllow(t, ic)

	// 期限切れのオン状態を書き込み、失効時刻を過ぎた状況を再現する。
	raw, _ := json.Marshal(autoAllowSetting{Enabled: true, ExpiresAt: time.Now().Unix() - 10})
	if err := d.SetSetting(SettingAutoAllow, string(raw)); err != nil {
		t.Fatal(err)
	}
	if st := ic.AutoAllowStatus(); st.Enabled {
		t.Fatal("expired auto-allow should report disabled")
	}
	// 遅延失効チェックにより settings がオフに書き戻されるはず。
	v, ok, err := d.GetSetting(SettingAutoAllow)
	if err != nil || !ok {
		t.Fatalf("setting should be rewritten: ok=%v err=%v", ok, err)
	}
	var s autoAllowSetting
	if err := json.Unmarshal([]byte(v), &s); err != nil || s.Enabled {
		t.Fatalf("expired switch should auto-disable, got %q", v)
	}
}

func TestHandleAskAutoAllowPassThrough(t *testing.T) {
	d := openTestDB(t)
	ic := New(d)
	resetAutoAllow(t, ic)
	if err := ic.SetAutoAllow(true, 2); err != nil {
		t.Fatal(err)
	}

	marker := fmt.Sprintf("autoallow-test-%d", time.Now().UnixNano())
	input := []byte(fmt.Sprintf(`{"command":%q}`, marker))
	// auto-allow が効かないと無限待機になるため、ctx のタイムアウトで打ち切り、超えたらテスト失敗とする。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	ok := ic.HandleAsk(ctx, 0, Decision{Action: "ask", Message: "承認が必要"}, "Bash", input)
	if !ok {
		t.Fatal("auto-allow enabled: ask should be approved without human decision")
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("auto-allow should approve immediately, not wait for a decision")
	}

	// 履歴には allowed + decision_source = auto_allow として記録され、pending レコードは残らないこと。
	var status, source string
	err := d.QueryRow(`SELECT status, decision_source FROM intercept_pending
		WHERE tool_name='Bash' AND tool_input::text LIKE '%' || $1 || '%'`, marker).Scan(&status, &source)
	if err != nil {
		t.Fatalf("auto-allow approval must be written to history: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM intercept_pending WHERE tool_input::text LIKE '%' || $1 || '%'`, marker)
	})
	if status != "allowed" || source != "auto_allow" {
		t.Fatalf("history = (%s, %s), want (allowed, auto_allow)", status, source)
	}
	var pending int
	if err := d.QueryRow(`SELECT count(*) FROM intercept_pending
		WHERE status='pending' AND tool_input::text LIKE '%' || $1 || '%'`, marker).Scan(&pending); err != nil {
		t.Fatal(err)
	}
	if pending != 0 {
		t.Fatal("auto-allow must not create pending records")
	}
}

func TestHandleAskAutoAllowOffUsesPendingFlow(t *testing.T) {
	d := openTestDB(t)
	ic := New(d)
	resetAutoAllow(t, ic)

	marker := fmt.Sprintf("autoallow-off-%d", time.Now().UnixNano())
	input := []byte(fmt.Sprintf(`{"command":%q}`, marker))
	dec := Decision{Action: "ask", Message: "承認が必要", TimeoutEnabled: true, TimeoutSeconds: 1, TimeoutAction: "deny"}
	start := time.Now()
	// オフ状態では従来の pending フローを通る：pending レコードを作成し、タイムアウトポリシーで処理する。
	if ic.HandleAsk(context.Background(), 0, dec, "Bash", input) {
		t.Fatal("auto-allow disabled: ask should not be auto-approved")
	}
	if time.Since(start) < 900*time.Millisecond {
		t.Fatal("auto-allow disabled: ask should wait for the human/timeout path")
	}
	var status, source string
	err := d.QueryRow(`SELECT status, decision_source FROM intercept_pending
		WHERE tool_name='Bash' AND tool_input::text LIKE '%' || $1 || '%'`, marker).Scan(&status, &source)
	if err != nil {
		t.Fatalf("pending flow should create a record: %v", err)
	}
	t.Cleanup(func() {
		_, _ = d.Exec(`DELETE FROM intercept_pending WHERE tool_input::text LIKE '%' || $1 || '%'`, marker)
	})
	if status != "timeout" {
		t.Fatalf("status = %s, want timeout (ask タイムアウトはポリシーに従い拒否)", status)
	}
	if source == "auto_allow" {
		t.Fatal("manual flow must not be labelled auto_allow")
	}
}
