package crypto

import (
	"bytes"
	"testing"
)

func TestSecretBindingAndTamper(t *testing.T) {
	key := bytes.Repeat([]byte{0x42}, 32)
	box, err := NewBox(key)
	if err != nil {
		t.Fatal(err)
	}
	first, nonce, err := box.Seal([]byte("local-test-secret"), "tenant-a", "connection-a", "stripe", 1)
	if err != nil {
		t.Fatal(err)
	}
	second, secondNonce, err := box.Seal([]byte("local-test-secret"), "tenant-a", "connection-a", "stripe", 1)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(nonce, secondNonce) || bytes.Equal(first, second) {
		t.Fatal("nonce reuse or deterministic ciphertext")
	}
	got, err := box.Open(first, nonce, "tenant-a", "connection-a", "stripe", 1)
	if err != nil || string(got) != "local-test-secret" {
		t.Fatalf("decrypt: %v", err)
	}
	if _, err = box.Open(first, nonce, "tenant-b", "connection-a", "stripe", 1); err == nil {
		t.Fatal("tenant A ciphertext opened under tenant B")
	}
	first[0] ^= 1
	if _, err = box.Open(first, nonce, "tenant-a", "connection-a", "stripe", 1); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}
}
