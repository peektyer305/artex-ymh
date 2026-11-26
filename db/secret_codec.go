package db

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

const secretPrefix = "gcm1:"

// The caller supplies a domain-separated key; this codec never derives keys.
func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func encodeSecret(key, plain []byte) (string, error) {
	if len(key) != 32 {
		return "", errors.New("暗号化鍵が初期化されていません")
	}
	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	return secretPrefix + base64.StdEncoding.EncodeToString(gcm.Seal(nonce, nonce, plain, nil)), nil
}

func decodeSecret(key []byte, stored string) ([]byte, error) {
	if stored == "" {
		return nil, nil
	}
	if len(key) != 32 {
		return nil, errors.New("復号鍵が初期化されていません")
	}
	if !strings.HasPrefix(stored, secretPrefix) {
		return nil, errors.New("保存された秘密情報の形式が不正です")
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, secretPrefix))
	if err != nil {
		return nil, fmt.Errorf("秘密情報のBase64が不正です: %w", err)
	}
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize() {
		return nil, errors.New("秘密情報の暗号文が短すぎます")
	}
	plain, err := gcm.Open(nil, raw[:gcm.NonceSize()], raw[gcm.NonceSize():], nil)
	if err != nil {
		return nil, fmt.Errorf("秘密情報を復号できません: %w", err)
	}
	return plain, nil
}

func hasPrefix(value, prefix string) bool { return strings.HasPrefix(value, prefix) }
