package selfupdate

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"golang.org/x/crypto/blake2b"
)

// sigAsset は release.yml が minisign で SHA256SUMS に対して生成する署名ファイル。
const sigAsset = sumsAsset + ".minisig"

// embeddedPubKey は ARTEX Release の minisign 公開鍵（base64、minisign -P の出力に対応）。
// 秘密鍵はリポジトリに置かず、GitHub Secrets の MINISIGN_KEY にのみ保存する；鍵を替えるときはここも secret と同期して入れ替える。
const embeddedPubKey = "RWSXIi1nUNeBgfwSraBVDfhBz6DC3SE0sWAK1z5CaLMaqAY7NAcerhIa"

// skipSignEnv は緊急用の逃げ道：1 にすると署名検証をスキップする（旧 Release との互換のための
// 緊急時の用途に限り、警告を出す）。デフォルトは fail-closed：署名がない場合も検証に通らない場合もインストールを拒否する。
const skipSignEnv = "ARTEX_UPDATE_SKIP_SIGN"

// minisign のファイル形式（https://jedisct1.github.io/minisign/ と minisign のソースを参照）：
//
//	公開鍵: base64( "Ed" || keynum(8) || pubkey(32) )                   42 バイト
//	署名: base64( "ED" || keynum(8) || ed25519_sig(64) )               74 バイト
//
// アルゴリズムのマーカーは大文字小文字が異なる：公開鍵は常に "Ed"；署名の "ED"（大文字の D）は
// 事前ハッシュモードで、署名対象は Blake2b-512(message)——minisign ≥0.9 はこの形式しか出力しない。
// 小文字の "Ed" は古いバージョンが元のメッセージを直接署名する形式で、本実装では未対応。
const (
	minisignAlgPubKey    = "Ed"
	minisignAlgPrehashed = "ED"
	minisignKeynumLen    = 8
)

// parsePubKey は minisign の公開鍵行（base64）を解析し、keynum と Ed25519 公開鍵を返す。
func parsePubKey(b64 string) ([minisignKeynumLen]byte, ed25519.PublicKey, error) {
	var keynum [minisignKeynumLen]byte
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(b64))
	if err != nil {
		return keynum, nil, fmt.Errorf("公開鍵の base64 デコードに失敗しました: %w", err)
	}
	if len(raw) != 2+minisignKeynumLen+ed25519.PublicKeySize || string(raw[:2]) != minisignAlgPubKey {
		return keynum, nil, fmt.Errorf("公開鍵の形式を認識できません（minisign Ed25519 公開鍵を期待しています）")
	}
	copy(keynum[:], raw[2:2+minisignKeynumLen])
	return keynum, ed25519.PublicKey(raw[2+minisignKeynumLen:]), nil
}

// parseMinisig は .minisig テキストから主署名を取り出す。最初の base64 行のみを扱う
// （trusted comment とそのグローバル署名は検証に参加しない——更新経路はコメント内容を使わない）。
func parseMinisig(raw string) ([minisignKeynumLen]byte, []byte, error) {
	var keynum [minisignKeynumLen]byte
	for line := range strings.Lines(raw) {
		line = strings.TrimSpace(line)
		if line == "" || strings.Contains(line, ":") { // コメント行は "untrusted comment: …" の形
			continue
		}
		dec, err := base64.StdEncoding.DecodeString(line)
		if err != nil {
			return keynum, nil, fmt.Errorf("署名の base64 デコードに失敗しました: %w", err)
		}
		if len(dec) != 2+minisignKeynumLen+ed25519.SignatureSize || string(dec[:2]) != minisignAlgPrehashed {
			return keynum, nil, fmt.Errorf("署名の形式を認識できません（事前ハッシュ Ed25519 署名を期待しています）")
		}
		copy(keynum[:], dec[2:2+minisignKeynumLen])
		return keynum, dec[2+minisignKeynumLen:], nil
	}
	return keynum, nil, fmt.Errorf("%s に署名データがありません", sigAsset)
}

// verifyMinisig は pubB64 を使い、sigRaw が msg の有効な minisign 署名かを検証する。
func verifyMinisig(pubB64, sigRaw string, msg []byte) error {
	wantKeynum, pub, err := parsePubKey(pubB64)
	if err != nil {
		return err
	}
	gotKeynum, sig, err := parseMinisig(sigRaw)
	if err != nil {
		return err
	}
	if !bytes.Equal(gotKeynum[:], wantKeynum[:]) {
		return fmt.Errorf("署名の keynum が組み込み公開鍵と一致しません（本プロジェクトのリリース鍵で署名されていません）")
	}
	digest := blake2b.Sum512(msg)
	if !ed25519.Verify(pub, digest[:], sig) {
		return fmt.Errorf("minisign の署名検証に失敗しました：%s が改ざんされているか、署名者が組み込み公開鍵と一致しません", sumsAsset)
	}
	return nil
}

// verifySumsSignature は sigAsset をダウンロードし、sums（SHA256SUMS の生バイト列）の署名を検証する。
// 署名検証と SHA256 照合は直列に重ねる：署名が一覧の出所の信頼性を、ハッシュが zip のバイト列の完全性を保証する。
func verifySumsSignature(ctx context.Context, c *http.Client, rel *Release, sums []byte, prog Progress) error {
	if os.Getenv(skipSignEnv) == "1" {
		if prog != nil {
			prog(PhaseVerify, -1, fmt.Sprintf("警告：%s=1 のため、リリース署名の検証をスキップしました（緊急の互換用途に限ります）", skipSignEnv))
		}
		return nil
	}

	asset, ok := rel.FindAsset(sigAsset)
	if !ok {
		return fmt.Errorf("この Release には %s がないため出所を検証できず、アップグレードを拒否します（緊急時は %s=1 でスキップ可能）", sigAsset, skipSignEnv)
	}
	body, err := get(ctx, c, asset.URL)
	if err != nil {
		return fmt.Errorf("%s のダウンロードに失敗しました: %w", sigAsset, err)
	}
	defer body.Close()

	raw, err := io.ReadAll(io.LimitReader(body, 1<<16)) // 署名ファイルは通常数百バイトしかない
	if err != nil {
		return fmt.Errorf("%s の読み込みに失敗しました: %w", sigAsset, err)
	}
	return verifyMinisig(embeddedPubKey, string(raw), sums)
}
