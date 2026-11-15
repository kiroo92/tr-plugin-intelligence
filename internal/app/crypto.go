package app

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
)

// crypto.go seals administrator credentials with AES-256-GCM. The master key
// stays in host plugin configuration; only ciphertext enters the plugin
// database. Each record carries its own identity in the additional data so a
// payload cannot be moved between configurations.

// masterKeyMinLength bounds the operator-supplied secret. The derived key stays
// 256 bits regardless, so this only rejects trivially short passphrases.
const masterKeyMinLength = 32

// cipher seals and opens credential payloads using a derived AES key.
type cipherBox struct {
	aead cipher.AEAD
}

// newCipherBox derives an AES-256-GCM box from the configured master secret.
// An empty or too-short secret returns errNoMasterKey so callers refuse work
// instead of falling back to plaintext storage.
func newCipherBox(masterKey string) (*cipherBox, error) {
	key := strings.TrimSpace(masterKey)
	if len(key) < masterKeyMinLength {
		return nil, errNoMasterKey
	}
	derived := sha256.Sum256([]byte("tr-plugin-intelligence/credential/" + key))
	block, err := aes.NewCipher(derived[:])
	if err != nil {
		return nil, errNoMasterKey
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, errNoMasterKey
	}
	return &cipherBox{aead: aead}, nil
}

// seal encrypts one credential bound to the supplied record identity. The
// nonce is random per call and prefixed to the ciphertext.
func (box *cipherBox) seal(plaintext, additional string) (string, error) {
	if box == nil {
		return "", errNoMasterKey
	}
	nonce := make([]byte, box.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", errors.New("无法生成加密随机数")
	}
	ciphertext := box.aead.Seal(nonce, nonce, []byte(plaintext), []byte(additional))
	return base64.RawStdEncoding.EncodeToString(ciphertext), nil
}

// open decrypts one credential and verifies its record identity. A missing box,
// malformed payload, or wrong identity returns an error so callers stop before
// any chargeable request.
func (box *cipherBox) open(payload, additional string) (string, error) {
	if box == nil {
		return "", errNoMasterKey
	}
	raw, err := base64.RawStdEncoding.DecodeString(payload)
	if err != nil || len(raw) <= box.aead.NonceSize() {
		return "", errors.New("凭据无法解密，请重新填写 API Key")
	}
	nonce, ciphertext := raw[:box.aead.NonceSize()], raw[box.aead.NonceSize():]
	plaintext, err := box.aead.Open(nil, nonce, ciphertext, []byte(additional))
	if err != nil {
		return "", errors.New("凭据无法解密，请检查主密钥或重新填写 API Key")
	}
	return string(plaintext), nil
}

// newKeyID produces the opaque identity used as additional data. It is stored
// in plaintext beside the ciphertext and carries no secret material.
func newKeyID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", errors.New("无法生成凭据标识")
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}
