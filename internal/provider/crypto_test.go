package provider

import (
	"bytes"
	"encoding/base64"
	"strings"
	"testing"
)

// Characterization tests for API Key 加解密 — 链路 6 的安全敏感环节。
// 改坏的后果是密钥泄露（弱加密/明文落库）或全部 provider 不可用
// （解不回来）。

func testKey(t *testing.T) []byte {
	t.Helper()
	return bytes.Repeat([]byte{0x42}, 32)
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := testKey(t)
	for _, plaintext := range []string{"", "sk-abc123", "含中文的密钥🔑", strings.Repeat("x", 4096)} {
		ct, err := encryptAPIKey(key, plaintext)
		if err != nil {
			t.Fatalf("encrypt: %v", err)
		}
		got, err := decryptAPIKey(key, ct)
		if err != nil {
			t.Fatalf("decrypt: %v", err)
		}
		if got != plaintext {
			t.Fatalf("round trip = %q, want %q", got, plaintext)
		}
	}
}

func TestEncryptIsNondeterministic(t *testing.T) {
	// GCM nonce 随机：同一明文两次加密必须产生不同密文，否则密文可被
	// 比对推断（也说明 nonce 被写死了）。
	key := testKey(t)
	a, _ := encryptAPIKey(key, "sk-same")
	b, _ := encryptAPIKey(key, "sk-same")
	if bytes.Equal(a, b) {
		t.Fatal("two encryptions of same plaintext produced identical ciphertext")
	}
}

func TestDecryptRejectsTamperedCiphertext(t *testing.T) {
	key := testKey(t)
	ct, _ := encryptAPIKey(key, "sk-abc")
	ct[len(ct)-1] ^= 0xFF
	if _, err := decryptAPIKey(key, ct); err == nil {
		t.Fatal("tampered ciphertext decrypted without error (GCM auth broken)")
	}
}

func TestDecryptRejectsWrongKey(t *testing.T) {
	ct, _ := encryptAPIKey(testKey(t), "sk-abc")
	otherKey := bytes.Repeat([]byte{0x24}, 32)
	if _, err := decryptAPIKey(otherKey, ct); err == nil {
		t.Fatal("ciphertext decrypted with wrong key")
	}
}

func TestDecryptRejectsShortCiphertext(t *testing.T) {
	if _, err := decryptAPIKey(testKey(t), []byte{0x01, 0x02}); err == nil {
		t.Fatal("ciphertext shorter than nonce accepted")
	}
}

func TestLoadEncryptionKey(t *testing.T) {
	valid := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 32))
	if _, err := loadEncryptionKey(valid); err != nil {
		t.Fatalf("valid 32-byte key rejected: %v", err)
	}
	if _, err := loadEncryptionKey("!!not-base64!!"); err == nil {
		t.Fatal("invalid base64 accepted")
	}
	short := base64.StdEncoding.EncodeToString([]byte("too-short"))
	if _, err := loadEncryptionKey(short); err == nil {
		t.Fatal("non-32-byte key accepted")
	}
}
