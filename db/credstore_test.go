package db

// credstore_test.go:認証情報の永続化層のテスト。暗号化/復号の roundtrip は PG に依存しない。
// CRUD は既存の PG-gated skip 方式(DB がなければ skip)に従う。

import (
	"context"
	"strings"
	"testing"
)

// TestCredentialSecretCrypto は PG に依存しない：AES-GCM の roundtrip、sessions とのドメイン分離、
// 誤った鍵での失敗、未初期化鍵での平文保存拒否、改ざん時の認証失敗を検証する。
func TestCredentialSecretCrypto(t *testing.T) {
	master := []byte("test-jwt-key-material-32bytes!!!")
	key := CredentialSecretKey(master)
	if len(key) != 32 {
		t.Fatalf("派生鍵の長さ = %d", len(key))
	}
	// ドメイン分離：sessions の派生鍵と異なる(同じ master、異なるドメインラベル)。
	if string(key) == string(SessionSecretKey(master)) {
		t.Fatal("credentials と sessions の派生鍵が同じで、ドメイン分離が機能していません")
	}

	store := &CredentialStore{key: key}
	enc, err := store.encrypt("P@ssw0rd!")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if !strings.HasPrefix(enc, secretPrefix) {
		t.Fatalf("暗号文にバージョン接頭辞がありません: %q", enc[:16])
	}
	if strings.Contains(enc, "P@ssw0rd") {
		t.Fatal("暗号文に平文の断片を含めてはなりません")
	}
	dec, err := store.decrypt(enc)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if dec != "P@ssw0rd!" {
		t.Fatalf("roundtrip 不一致: %q", dec)
	}
	// 2 回の暗号化で暗号文が異なる(ランダム nonce)。
	enc2, _ := store.encrypt("P@ssw0rd!")
	if enc == enc2 {
		t.Fatal("nonce がランダム化されていません: 2 回の暗号文が同一です")
	}
	// 誤った鍵(sessions の鍵を含む)での復号は必ず失敗する。
	wrong := &CredentialStore{key: CredentialSecretKey([]byte("another-master-key-32bytes!!!!"))}
	if _, err := wrong.decrypt(enc); err == nil {
		t.Fatal("誤った鍵での復号は失敗すべきです")
	}
	cross := &CredentialStore{key: SessionSecretKey(master)}
	if _, err := cross.decrypt(enc); err == nil {
		t.Fatal("sessions の鍵で credentials の暗号文を復号すると失敗すべきです（ドメイン分離）")
	}
	// 未初期化鍵:暗号化を拒否し(平文を保存しない)、復号も拒否する。
	noKey := &CredentialStore{}
	if _, err := noKey.encrypt("x"); err == nil {
		t.Fatal("鍵が未初期化なら暗号化を拒否すべきです")
	}
	if _, err := noKey.decrypt(enc); err == nil {
		t.Fatal("鍵が未初期化なら復号を拒否すべきです")
	}
	// バージョン接頭辞なし:平文としての読み取りを拒否する。
	if _, err := store.decrypt("P@ssw0rd!"); err == nil {
		t.Fatal("不明な形式は平文としての読み取りを拒否すべきです")
	}
	// 暗号文の改ざん：GCM 認証に失敗する。
	tampered := enc[:len(enc)-4] + "AAAA"
	if _, err := store.decrypt(tampered); err == nil {
		t.Fatal("改ざんした暗号文は認証に失敗すべきです")
	}
}

// TestMaskSecret はマスク処理:前後 2 文字ずつ残し、短すぎる場合は全体をマスクする。
func TestMaskSecret(t *testing.T) {
	cases := map[string]string{
		"P@ssw0rd!": "P@****d!",
		"abcdef":    "ab****ef",
		"abcde":     "ab****de",
		"abcd":      "****",
		"a":         "****",
		"":          "****",
	}
	for in, want := range cases {
		if got := MaskSecret(in); got != want {
			t.Errorf("MaskSecret(%q) = %q、期待は %q", in, got, want)
		}
	}
}

// TestValidCredType は種別の全集合を検証する。
func TestValidCredType(t *testing.T) {
	for _, ty := range []string{"password", "hash_nt", "hash_lm", "hash_sha1", "ticket", "ssh_key", "token"} {
		if !ValidCredType(ty) {
			t.Errorf("%s は有効であるべきです", ty)
		}
	}
	for _, ty := range []string{"", "ntlm", "PASSWORD", "key"} {
		if ValidCredType(ty) {
			t.Errorf("%q は有効であってはなりません", ty)
		}
	}
}

// TestCredentialStoreCRUD は live PG で実行する(既存の skip 方式：DB がなければ skip)。
func TestCredentialStoreCRUD(t *testing.T) {
	d, err := Open(testDSN(t))
	if err != nil {
		t.Skipf("postgres unavailable (%v) — skipping", err)
	}
	defer d.Close()
	store := NewCredentialStore(d, []byte("test-jwt-key-material-32bytes!!!"))
	ctx := context.Background()

	// 検証:不正な種別/空の secret/task 欠落は即エラーとなり、DB へ書き込まない。
	if _, err := store.Create(ctx, &CredentialRecord{TaskID: 1, CredType: "ntlm", Secret: "x", Source: "t"}); err == nil {
		t.Fatal("不正な cred_type はエラーになるべきです")
	}
	if _, err := store.Create(ctx, &CredentialRecord{TaskID: 1, CredType: "password", Source: "t"}); err == nil {
		t.Fatal("secret が空ならエラーになるべきです")
	}
	if _, err := store.Create(ctx, &CredentialRecord{CredType: "password", Secret: "x", Source: "t"}); err == nil {
		t.Fatal("task_id が欠けていればエラーになるべきです")
	}

	rec := &CredentialRecord{
		TaskID: 999999001, Username: "administrator", CredType: "hash_nt",
		Secret: "aad3b435b51404eeaad3b435b51404ee", Domain: "CORP",
		Source: "10.0.0.5 mimikatz lsass dump",
	}
	id, err := store.Create(ctx, rec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(context.Background(), id) })

	// DB 内は平文であってはならない。
	var raw string
	if err := d.QueryRow(`SELECT secret FROM credentials WHERE id=$1`, id).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, secretPrefix) || strings.Contains(raw, "aad3b435") {
		t.Fatalf("保存された secret は gcm1 の暗号文であるべきです: %q", raw[:24])
	}

	got, err := store.Get(ctx, id)
	if err != nil || got == nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Secret != rec.Secret || got.Domain != "CORP" || got.Verified {
		t.Fatalf("got = %+v", got)
	}

	// ListByTask は自タスクの分だけ返す。
	other := &CredentialRecord{TaskID: 999999002, CredType: "password", Secret: "x12345", Source: "t"}
	oid, err := store.Create(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Delete(context.Background(), oid) })
	list, err := store.ListByTask(ctx, 999999001)
	if err != nil || len(list) != 1 || list[0].ID != id {
		t.Fatalf("ListByTask = %v, %v", list, err)
	}

	// SetVerified。
	if err := store.SetVerified(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	got, _ = store.Get(ctx, id)
	if !got.Verified {
		t.Fatal("SetVerified が反映されていません")
	}

	// Delete + Get miss。
	if err := store.Delete(ctx, id); err != nil {
		t.Fatal(err)
	}
	got, err = store.Get(ctx, id)
	if err != nil || got != nil {
		t.Fatalf("削除後の Get は (nil,nil) であるべき、実際は %v, %v", got, err)
	}
}
