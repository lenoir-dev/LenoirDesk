package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
)

type Box struct{ aead cipher.AEAD }

func NewBox(key []byte) (*Box, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Box{aead: aead}, nil
}
func aad(tenantID, connectionID, provider string, version int) []byte {
	return []byte(fmt.Sprintf("%s|%s|%s|%d", tenantID, connectionID, provider, version))
}
func (b *Box) Seal(secret []byte, tenantID, connectionID, provider string, version int) (ciphertext, nonce []byte, err error) {
	nonce = make([]byte, b.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return b.aead.Seal(nil, nonce, secret, aad(tenantID, connectionID, provider, version)), nonce, nil
}
func (b *Box) Open(ciphertext, nonce []byte, tenantID, connectionID, provider string, version int) ([]byte, error) {
	if len(nonce) != b.aead.NonceSize() {
		return nil, fmt.Errorf("invalid secret nonce")
	}
	return b.aead.Open(nil, nonce, ciphertext, aad(tenantID, connectionID, provider, version))
}

// SealPurpose encrypts short internal secrets under a domain-separated context.
func (b *Box) SealPurpose(secret []byte, purpose string) (ciphertext, nonce []byte, err error) {
	nonce = make([]byte, b.aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return nil, nil, err
	}
	return b.aead.Seal(nil, nonce, secret, []byte("relaydesk-internal|"+purpose)), nonce, nil
}

func (b *Box) OpenPurpose(ciphertext, nonce []byte, purpose string) ([]byte, error) {
	if len(nonce) != b.aead.NonceSize() {
		return nil, fmt.Errorf("invalid internal secret nonce")
	}
	return b.aead.Open(nil, nonce, ciphertext, []byte("relaydesk-internal|"+purpose))
}
