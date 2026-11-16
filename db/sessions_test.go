package db

// sessions_test.go は sessions の永続化層テスト。暗号化/復号の roundtrip は PG に依存しない;
// CRUD は既存の PG-gated skip 方式（DB がなければスキップ）。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// TestSessionSecretCrypto は PG に依存しない：AES-GCM の roundtrip、誤った鍵での失敗、
// 未初期化の鍵による平文保存の拒否、未知形式の平文読み取り拒否を確認する。
func TestSessionSecretCrypto(t *testing.T) {
	master := []byte("test-jwt-key-material-32bytes!!!")
	key := SessionSecretKey(master)
	if len(key) != 32 {
		t.Fatalf("派生鍵の長さ = %d", len(key))
	}
	// ドメイン分離:派生鍵は master 自身やその直接ハッシュ以外の利用可能な形と一致してはならない。
	other := SessionSecretKey([]byte("another-master-key-32bytes!!!!"))
	if string(key) == string(other) {
		t.Fatal("異なる master から同じ鍵が派生しました")
	}

	store := &SessionStore{key: key}
	secret := json.RawMessage(`{"url":"http://example.local/shell.php","password":"cmd","lang":"php"}`)
	enc, err := store.encrypt(secret)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if !strings.HasPrefix(enc, secretPrefix) {
		t.Fatalf("暗号文にバージョン接頭辞がありません: %q", enc[:16])
	}
	if strings.Contains(enc, "shell.php") || strings.Contains(enc, "cmd") {
		t.Fatal("暗号文に平文の断片を含めてはなりません")
	}
	dec, err := store.decrypt(enc)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if string(dec) != string(secret) {
		t.Fatalf("roundtrip 不一致: %q", dec)
	}
	// 2 回の暗号化で暗号文が異なる（nonce はランダム）。
	enc2, _ := store.encrypt(secret)
	if enc == enc2 {
		t.Fatal("nonce がランダム化されていません：2 回の暗号文が同一です")
	}
	// 誤った鍵での復号は必ず失敗する（誠実に、ゴミを返さない）。
	wrong := &SessionStore{key: other}
	if _, err := wrong.decrypt(enc); err == nil {
		t.Fatal("誤った鍵での復号は失敗すべきです")
	}
	// 未初期化の鍵:暗号化（平文を保存しない）も復号も拒否する。
	noKey := &SessionStore{}
	if _, err := noKey.encrypt(secret); err == nil {
		t.Fatal("鍵が未初期化なら暗号化を拒否すべきです")
	}
	if _, err := noKey.decrypt(enc); err == nil {
		t.Fatal("鍵が未初期化なら復号を拒否すべきです")
	}
	// バージョン接頭辞のない履歴/異常データ:平文としての読み取りを拒否する。
	if _, err := store.decrypt(`{"url":"x"}`); err == nil {
		t.Fatal("不明な形式は平文としての読み取りを拒否すべきです")
	}
	// 暗号文の改ざん：GCM 認証に失敗する。
	tampered := enc[:len(enc)-4] + "AAAA"
	if _, err := store.decrypt(tampered); err == nil {
		t.Fatal("改ざんした暗号文は認証に失敗すべきです")
	}
}

// TestSessionStoreCRUD は実際の PG を使う（既存の skip 方式：DB がなければスキップ）。
func TestSessionStoreCRUD(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	store := NewSessionStore(d, []byte("test-jwt-key-material-32bytes!!!"))
	ctx := context.Background()

	rec := &SessionRecord{
		Kind: "http_php", URL: "http://target.local/s.php",
		Secret:          json.RawMessage(`{"url":"http://target.local/s.php","password":"cmd","lang":"php","variant":"eval","funcs":["exec"]}`),
		Lang:            "php",
		CreatedByTask:   0, // NULL を許容する列
		CreatedByIntent: 0,
	}
	id, err := store.Create(ctx, rec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(context.Background(), id) })

	// DB 内は平文であってはならない。
	var raw string
	if err := d.QueryRow(`SELECT secret FROM sessions WHERE id=$1`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, secretPrefix) || strings.Contains(raw, "target.local") {
		t.Fatalf("保存された secret は gcm1 の暗号文であるべきです: %q", raw[:24])
	}

	got, err := store.Get(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Kind != "http_php" || got.Status != SessionAlive || got.Lang != "php" {
		t.Fatalf("got = %+v", got)
	}
	if !strings.Contains(string(got.Secret), `"password":"cmd"`) {
		t.Fatalf("secret の復号に失敗しました: %q", got.Secret)
	}

	// List と status による絞り込み。
	alive, err := store.List(ctx, SessionAlive)
	if err != nil || len(alive) == 0 {
		t.Fatalf("List alive: %v", err)
	}
	found := false
	for _, r := range alive {
		if r.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatal("List(alive) に新しいセッションが含まれていません")
	}

	// UpdateStatus + Touch + SetHostAsset。
	if err := store.UpdateStatus(ctx, id, SessionDead); err != nil {
		t.Fatalf("UpdateStatus: %v", err)
	}
	if err := store.UpdateStatus(ctx, id, "bogus"); err == nil {
		t.Fatal("不正なステータスはエラーになるべきです")
	}
	store.Touch(ctx, id)
	store.SetHostAsset(ctx, id, 12345)
	got, _ = store.Get(ctx, id)
	if got.Status != SessionDead || got.HostAssetID != 12345 {
		t.Fatalf("ステータス/アセットの書き戻しに失敗しました: %+v", got)
	}
	dead, err := store.List(ctx, SessionDead)
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, r := range dead {
		if r.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatal("List(dead) に新しいセッションが含まれていません")
	}

	// Delete + Get miss。
	if err := store.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	got, err = store.Get(ctx, id)
	if err != nil || got != nil {
		t.Fatalf("削除後の Get は (nil,nil) であるべき、実際は %v, %v", got, err)
	}
}
