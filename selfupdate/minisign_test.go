package selfupdate

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	"golang.org/x/crypto/blake2b"
)

// signLikeMinisig は一時的な鍵ペアで minisign の事前ハッシュ形式に従って署名し、公開鍵行と
// .minisig テキストを返す。検証側の鏡実装で、テスト専用。
func signLikeMinisig(t *testing.T, msg []byte) (pubB64, minisig string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("一時鍵の生成に失敗しました: %v", err)
	}
	var keynum [minisignKeynumLen]byte
	if _, err := rand.Read(keynum[:]); err != nil {
		t.Fatalf("keynum の生成に失敗しました: %v", err)
	}

	pubRaw := append([]byte(minisignAlgPubKey), keynum[:]...)
	pubRaw = append(pubRaw, pub...)
	pubB64 = base64.StdEncoding.EncodeToString(pubRaw)

	digest := blake2b.Sum512(msg)
	sig := ed25519.Sign(priv, digest[:])
	sigRaw := append([]byte(minisignAlgPrehashed), keynum[:]...)
	sigRaw = append(sigRaw, sig...)
	return pubB64, "untrusted comment: test\n" + base64.StdEncoding.EncodeToString(sigRaw) + "\n" +
		"trusted comment: test\n"
}

func TestVerifyMinisigRoundTrip(t *testing.T) {
	msg := []byte("deadbeef  artex-0.0.0-linux-amd64.zip\n")
	pub, sig := signLikeMinisig(t, msg)
	if err := verifyMinisig(pub, sig, msg); err != nil {
		t.Fatalf("自前で署名したものが検証を通るべきです: %v", err)
	}
}

// 実際の minisign ツールとの互換性ベクタ：.minisig は公式 minisign.exe 0.12 が本リポジトリの
// 組み込み公開鍵に対応する秘密鍵で署名したもの（SUMS の内容は固定）。検証が通らない場合は
// 形式解析が実際のリリース手順と乖離しており、CI が署名したものをユーザー側が全て拒否してしまう。
func TestVerifyMinisigRealMinisignVector(t *testing.T) {
	sums := []byte("abc123  artex-0.0.0-test.zip\n")
	const realSig = `untrusted comment: signature from minisign secret key
RUSXIi1nUNeBgQlNNluEYr0xUtbczexTc7tErAAcT9PewbUgc5yR/SqhkoptD4XymJGhEnTDMaY9FK5FSGsvPIae5oW1oRf+Igg=
trusted comment: timestamp:1789274763	file:SUMS	hashed
hPfyMYz7wNoDz1PWTzLZc9KGIo0zFAsvOjgqBxwWqkDSy7+ySAuiqac0pIdGDr+tHTOW+CokYXmZI8VPPHMsBQ==
`
	if err := verifyMinisig(embeddedPubKey, realSig, sums); err != nil {
		t.Fatalf("公式 minisign が署名したものは検証を通るべきです: %v", err)
	}
}

func TestVerifyMinisigRejectsTamperedMessage(t *testing.T) {
	msg := []byte("deadbeef  artex-0.0.0-linux-amd64.zip\n")
	pub, sig := signLikeMinisig(t, msg)

	tampered := []byte("aaaaaaab  artex-0.0.0-linux-amd64.zip\n") // 一覧のダイジェストを 1 つ改ざんする
	if err := verifyMinisig(pub, sig, tampered); err == nil {
		t.Fatal("メッセージが改ざんされていれば検証は失敗するべきです")
	}
}

func TestVerifyMinisigRejectsTamperedSignature(t *testing.T) {
	msg := []byte("x\n")
	pub, sig := signLikeMinisig(t, msg)
	// デコードし、署名の 1 バイトを反転して再エンコードし、転送の破損や手作業の変更を再現する。
	lines := strings.Split(sig, "\n")
	raw, _ := base64.StdEncoding.DecodeString(lines[1])
	raw[len(raw)-1] ^= 0x01
	lines[1] = base64.StdEncoding.EncodeToString(raw)
	if err := verifyMinisig(pub, strings.Join(lines, "\n"), msg); err == nil {
		t.Fatal("署名が改ざんされていれば検証は失敗するべきです")
	}
}

func TestVerifyMinisigRejectsWrongKey(t *testing.T) {
	msg := []byte("x\n")
	_, sig := signLikeMinisig(t, msg)
	otherPub, _ := signLikeMinisig(t, msg) // 別の鍵ペア。keynum も公開鍵も異なる
	if err := verifyMinisig(otherPub, sig, msg); err == nil {
		t.Fatal("別の鍵での検証は失敗するべきです")
	}
}

func TestVerifyMinisigRejectsMalformed(t *testing.T) {
	for _, raw := range []string{
		"",
		"not a signature at all",
		"untrusted comment: only comments\ntrusted comment: nothing here\n",
		base64.StdEncoding.EncodeToString([]byte("short")),
	} {
		if err := verifyMinisig(embeddedPubKey, raw, []byte("x")); err == nil {
			t.Errorf("不正な入力 %q は拒否されるべきです", raw)
		}
	}
}

// 署名ファイルの欠落は fail-closed であること：.minisig アセットがない場合は直ちに拒否し、ネットワーク要求を一切出さない。
func TestVerifySumsSignatureRejectsMissingAsset(t *testing.T) {
	rel := &Release{Assets: []Asset{{Name: sumsAsset}}}
	err := verifySumsSignature(t.Context(), nil, rel, []byte("x"), nil)
	if err == nil || !strings.Contains(err.Error(), sigAsset) {
		t.Fatalf("%s がない場合はアップグレードを拒否するべきです。結果: %v", sigAsset, err)
	}
}

// 緊急用の逃げ道：ARTEX_UPDATE_SKIP_SIGN=1 なら署名アセットも探さないが、prog で警告を出す。
func TestVerifySumsSignatureSkipEnv(t *testing.T) {
	t.Setenv(skipSignEnv, "1")
	rel := &Release{} // SHA256SUMS すらないので、スキップしなければ必ず失敗する
	warned := false
	err := verifySumsSignature(t.Context(), nil, rel, []byte("x"), func(_ Phase, _ int, msg string) {
		if strings.Contains(msg, "警告") {
			warned = true
		}
	})
	if err != nil {
		t.Fatalf("%s=1 なら署名検証をスキップするべきです: %v", skipSignEnv, err)
	}
	if !warned {
		t.Error("署名検証をスキップするときは警告を出すべきです")
	}
}
