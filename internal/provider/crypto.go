package provider

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"io"
)

// loadEncryptionKey decodes the base64 HIFY_ENCRYPTION_KEY into the raw
// 32-byte AES-256 key, once at startup (wire.go), not per call.
func loadEncryptionKey(base64Key string) ([]byte, error) {
	key, err := base64.StdEncoding.DecodeString(base64Key)
	if err != nil {
		return nil, fmt.Errorf("provider: decode encryption key: %w", err)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("provider: encryption key must decode to 32 bytes, got %d", len(key))
	}
	return key, nil
}

// encryptAPIKey seals plaintext with AES-256-GCM, nonce prepended to the
// ciphertext so decryptAPIKey doesn't need it stored separately.
func encryptAPIKey(key []byte, plaintext string) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("provider: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("provider: new gcm: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("provider: generate nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

func decryptAPIKey(key, ciphertext []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("provider: new cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("provider: new gcm: %w", err)
	}
	nonceSize := gcm.NonceSize()
	if len(ciphertext) < nonceSize {
		return "", fmt.Errorf("provider: ciphertext shorter than nonce size")
	}
	nonce, ct := ciphertext[:nonceSize], ciphertext[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("provider: decrypt api key: %w", err)
	}
	return string(plaintext), nil
}
