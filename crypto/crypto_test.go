package crypto

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"strings"
	"testing"
)

// freshKeyEnv installs a valid 32-byte AES key + 2048-bit RSA key into the
// process environment for the duration of the test, then yields a ready-to-use
// CryptoService. Restores any prior global service after the test.
func freshKeyEnv(t *testing.T) *CryptoService {
	t.Helper()

	aesKey := make([]byte, 32)
	if _, err := rand.Read(aesKey); err != nil {
		t.Fatalf("rand for aes key: %v", err)
	}
	t.Setenv(EnvDataEncryptionKey, base64.StdEncoding.EncodeToString(aesKey))

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa keygen: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	t.Setenv(EnvRSAPrivateKey, strings.ReplaceAll(string(pemBytes), "\n", "\\n"))

	cs, err := NewCryptoService()
	if err != nil {
		t.Fatalf("NewCryptoService with valid env: %v", err)
	}

	prev := globalCryptoService
	SetGlobalCryptoService(cs)
	t.Cleanup(func() { SetGlobalCryptoService(prev) })

	return cs
}

// TestKeyLengthFatal locks in F002-T03 fix #1: DATA_ENCRYPTION_KEY that does not
// decode to exactly 32 bytes must produce an error. The previous implementation
// silently SHA256-hashed any input, hiding misconfiguration.
func TestKeyLengthFatal(t *testing.T) {
	t.Setenv(EnvRSAPrivateKey, "ignored-for-this-test")

	// Inputs are chosen so each parses unambiguously through ONE encoder:
	// base64 inputs include '+' or '/' to defeat the hex decoder; the hex
	// case is short enough that no base64 decoder would yield 32 bytes from it.
	short16 := bytes16(0x7F)         // 16 random-ish bytes → base64 has '/'
	long48 := bytes48()              // 48 distinct bytes → base64 has '+/'
	cases := []struct {
		name string
		val  string
		want string // substring expected in error message
	}{
		{"empty", "", "not set"},
		{"undecodable", "this-is-not-base64-or-hex!!!", "could not be decoded"},
		{"too-short-base64", base64.StdEncoding.EncodeToString(short16), "decoded to 16 bytes"},
		{"too-long-base64", base64.StdEncoding.EncodeToString(long48), "decoded to 48 bytes"},
		// "abcdef" parses successfully via base64 first (4 bytes); we just
		// care that the wrong length is reported, not which decoder won.
		{"short-input-wrong-length", "abcdef", "AES-256 requires exactly 32 bytes"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(EnvDataEncryptionKey, c.val)
			_, err := loadDataKeyFromEnv()
			if err == nil {
				t.Fatalf("expected error for %q, got nil", c.val)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not contain %q", err.Error(), c.want)
			}
		})
	}

	// Sanity: a properly encoded 32-byte key must succeed.
	t.Run("happy-path-32-bytes", func(t *testing.T) {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			t.Fatalf("rand: %v", err)
		}
		t.Setenv(EnvDataEncryptionKey, base64.StdEncoding.EncodeToString(raw))
		key, err := loadDataKeyFromEnv()
		if err != nil {
			t.Fatalf("unexpected error on valid 32-byte key: %v", err)
		}
		if len(key) != 32 {
			t.Fatalf("expected 32-byte key, got %d", len(key))
		}
	})
}

// TestScanError locks in F002-T03 fix #2: EncryptedString.Scan must return an
// error when DecryptFromStorage fails, not silently expose ciphertext as
// plaintext. Caller code (exchange API, agent decisions) must see the failure.
func TestScanError(t *testing.T) {
	freshKeyEnv(t)

	// A storage-prefixed value with corrupt ciphertext: passes
	// IsEncryptedStorageValue but fails DecryptFromStorage.
	corrupt := storagePrefix + "deadbeef:not-base64-cipher"

	var es EncryptedString
	err := es.Scan(corrupt)
	if err == nil {
		t.Fatalf("Scan with corrupt ciphertext: expected error, got nil; es=%q", string(es))
	}
	// Critical invariant: ciphertext must NOT be exposed as plaintext.
	if string(es) == corrupt {
		t.Fatalf("Scan leaked ciphertext as plaintext: %q", string(es))
	}
}

// TestValueError locks in F002-T03 fix #3: EncryptedString.Value must propagate
// encryption failures rather than silently writing plaintext to the DB. We
// trigger failure by clearing the global crypto service's data key after the
// service is constructed; EncryptForStorage then has nothing to encrypt with.
func TestValueError(t *testing.T) {
	cs := freshKeyEnv(t)

	// Sabotage the data key to force EncryptForStorage to fail.
	cs.dataKey = nil

	es := EncryptedString("super-secret-api-key")
	got, err := es.Value()
	if err == nil {
		t.Fatalf("Value with broken cipher: expected error, got nil; got=%v", got)
	}
	// Critical invariant: plaintext must NOT be returned to GORM.
	if s, ok := got.(string); ok && s == string(es) {
		t.Fatalf("Value leaked plaintext to driver: %q", s)
	}
	if got != nil {
		t.Fatalf("Value should return nil driver.Value on error, got %T(%v)", got, got)
	}
}

// TestNonceUniqueness — extra coverage that two encryptions of the same
// plaintext produce different ciphertexts (GCM nonce is randomized per call).
// SPEC F002 acceptance criterion #3.
func TestNonceUniqueness(t *testing.T) {
	cs := freshKeyEnv(t)
	plaintext := "nonce-uniqueness-canary"
	a, err := cs.EncryptForStorage(plaintext)
	if err != nil {
		t.Fatalf("encrypt #1: %v", err)
	}
	b, err := cs.EncryptForStorage(plaintext)
	if err != nil {
		t.Fatalf("encrypt #2: %v", err)
	}
	if a == b {
		t.Fatalf("nonce reuse: same ciphertext for two encryptions of %q", plaintext)
	}
	// Both must round-trip.
	for i, c := range []string{a, b} {
		got, err := cs.DecryptFromStorage(c)
		if err != nil {
			t.Fatalf("decrypt #%d: %v", i, err)
		}
		if got != plaintext {
			t.Fatalf("decrypt #%d: got %q want %q", i, got, plaintext)
		}
	}
}

// TestScanPlainStringPassthrough — when crypto service has no encrypted prefix,
// Scan should pass plaintext through. This ensures the F002-T03 fix didn't
// over-rotate and break unencrypted reads.
func TestScanPlainStringPassthrough(t *testing.T) {
	freshKeyEnv(t)
	var es EncryptedString
	if err := es.Scan("plain-text-value"); err != nil {
		t.Fatalf("Scan of plain string should not error: %v", err)
	}
	if string(es) != "plain-text-value" {
		t.Fatalf("Scan plaintext mismatch: %q", string(es))
	}
}

// bytes16 returns 16 bytes whose base64 representation contains characters
// outside the hex alphabet, ensuring it cannot be misparsed as hex.
func bytes16(seed byte) []byte {
	b := make([]byte, 16)
	for i := range b {
		b[i] = byte((int(seed) + i*37) & 0xFF)
	}
	return b
}

// bytes48 returns 48 bytes designed so that base64(bytes48()) contains '+' or
// '/', which makes it unambiguous (not a valid hex string).
func bytes48() []byte {
	b := make([]byte, 48)
	for i := range b {
		b[i] = byte(i*7 + 251) // mix that produces high-bit / odd patterns
	}
	return b
}

// helper to silence unused import warning in restricted builds.
var _ = errors.New
