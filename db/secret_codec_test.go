package db

import (
	"bytes"
	"testing"
)

func TestSecretCodecDomainsAndCompatibility(t *testing.T) {
	master := []byte("master-key-for-codec-tests")
	sessionKey, credentialKey := SessionSecretKey(master), CredentialSecretKey(master)
	session := NewSessionStore(nil, master)
	stored, err := session.encrypt([]byte(`{"password":"example"}`))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := decodeSecret(sessionKey, stored)
	if err != nil || !bytes.Equal(plain, []byte(`{"password":"example"}`)) {
		t.Fatalf("%s %v", plain, err)
	}
	if _, err := decodeSecret(credentialKey, stored); err == nil {
		t.Fatal("accepted another domain's ciphertext")
	}
	for _, bad := range []string{"plain", "gcm1:!", "gcm1:YQ=="} {
		if _, err := decodeSecret(sessionKey, bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	if _, err := encodeSecret(nil, []byte("x")); err == nil {
		t.Fatal("accepted empty key")
	}
}
