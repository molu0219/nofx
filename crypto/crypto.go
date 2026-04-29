package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"database/sql/driver"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

const (
	storagePrefix    = "ENC:v1:"
	storageDelimiter = ":"
)

// Environment variable names
const (
	EnvDataEncryptionKey = "DATA_ENCRYPTION_KEY" // AES data encryption key (Base64)
	EnvRSAPrivateKey     = "RSA_PRIVATE_KEY"     // RSA private key (PEM format, use \n for newlines)
)

type EncryptedPayload struct {
	WrappedKey string `json:"wrappedKey"`
	IV         string `json:"iv"`
	Ciphertext string `json:"ciphertext"`
	AAD        string `json:"aad,omitempty"`
	KID        string `json:"kid,omitempty"`
	TS         int64  `json:"ts,omitempty"`
}

type AADData struct {
	UserID    string `json:"userId"`
	SessionID string `json:"sessionId"`
	TS        int64  `json:"ts"`
	Purpose   string `json:"purpose"`
}

type CryptoService struct {
	privateKey *rsa.PrivateKey
	publicKey  *rsa.PublicKey
	dataKey    []byte
}

// NewCryptoService creates crypto service (loads keys from environment variables)
func NewCryptoService() (*CryptoService, error) {
	// 1. Load RSA private key
	privateKey, err := loadRSAPrivateKeyFromEnv()
	if err != nil {
		return nil, fmt.Errorf("failed to load RSA private key: %w", err)
	}

	// 2. Load AES data encryption key
	dataKey, err := loadDataKeyFromEnv()
	if err != nil {
		return nil, fmt.Errorf("failed to load data encryption key: %w", err)
	}

	return &CryptoService{
		privateKey: privateKey,
		publicKey:  &privateKey.PublicKey,
		dataKey:    dataKey,
	}, nil
}

// loadRSAPrivateKeyFromEnv loads RSA private key from environment variable
func loadRSAPrivateKeyFromEnv() (*rsa.PrivateKey, error) {
	keyPEM := os.Getenv(EnvRSAPrivateKey)
	if keyPEM == "" {
		return nil, fmt.Errorf("environment variable %s not set, please configure RSA private key in .env", EnvRSAPrivateKey)
	}

	// Handle newlines in environment variable (\n -> actual newline)
	keyPEM = strings.ReplaceAll(keyPEM, "\\n", "\n")

	return ParseRSAPrivateKeyFromPEM([]byte(keyPEM))
}

// loadDataKeyFromEnv loads the AES-256 data encryption key from the environment.
// DATA_ENCRYPTION_KEY must decode (base64 / base64-raw / hex) to exactly 32 bytes;
// no silent fallback (previously SHA256-hashed any input, which made misconfigured
// keys invisible until decryption failed downstream).
func loadDataKeyFromEnv() ([]byte, error) {
	keyStr := strings.TrimSpace(os.Getenv(EnvDataEncryptionKey))
	if keyStr == "" {
		return nil, fmt.Errorf("environment variable %s not set, please configure data encryption key in .env", EnvDataEncryptionKey)
	}

	key, ok := decodePossibleKey(keyStr)
	if !ok {
		return nil, fmt.Errorf("%s could not be decoded as base64 or hex; expected a 32-byte AES-256 key (generate with `openssl rand -base64 32`)", EnvDataEncryptionKey)
	}
	if len(key) != 32 {
		return nil, fmt.Errorf("%s decoded to %d bytes; AES-256 requires exactly 32 bytes (generate with `openssl rand -base64 32`)", EnvDataEncryptionKey, len(key))
	}
	return key, nil
}

// ParseRSAPrivateKeyFromPEM parses RSA private key from PEM format
func ParseRSAPrivateKeyFromPEM(pemBytes []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, errors.New("invalid PEM format")
	}

	switch block.Type {
	case "RSA PRIVATE KEY":
		return x509.ParsePKCS1PrivateKey(block.Bytes)
	case "PRIVATE KEY":
		key, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, err
		}
		rsaKey, ok := key.(*rsa.PrivateKey)
		if !ok {
			return nil, errors.New("not an RSA key")
		}
		return rsaKey, nil
	default:
		return nil, errors.New("unsupported key type: " + block.Type)
	}
}

// decodePossibleKey returns the bytes produced by the first encoding that
// successfully parses `value` (base64-std → base64-raw → hex). It does NOT
// validate length — that is the caller's job, so callers can produce a
// length-specific error message ("decoded to N bytes; want 32"). Returning
// (nil, false) means the input could not be parsed by any supported encoding.
func decodePossibleKey(value string) ([]byte, bool) {
	decoders := []func(string) ([]byte, error){
		base64.StdEncoding.DecodeString,
		base64.RawStdEncoding.DecodeString,
		func(s string) ([]byte, error) { return hex.DecodeString(s) },
	}

	for _, decoder := range decoders {
		if decoded, err := decoder(value); err == nil && len(decoded) > 0 {
			return decoded, true
		}
	}

	return nil, false
}

func (cs *CryptoService) HasDataKey() bool {
	return len(cs.dataKey) > 0
}

func (cs *CryptoService) GetPublicKeyPEM() string {
	publicKeyDER, err := x509.MarshalPKIXPublicKey(cs.publicKey)
	if err != nil {
		return ""
	}

	publicKeyPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: publicKeyDER,
	})

	return string(publicKeyPEM)
}

func (cs *CryptoService) EncryptForStorage(plaintext string, aadParts ...string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	if !cs.HasDataKey() {
		return "", errors.New("data encryption key not configured")
	}
	if isEncryptedStorageValue(plaintext) {
		return plaintext, nil
	}

	block, err := aes.NewCipher(cs.dataKey)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}

	aad := composeAAD(aadParts)
	ciphertext := gcm.Seal(nil, nonce, []byte(plaintext), aad)

	return storagePrefix +
		base64.StdEncoding.EncodeToString(nonce) + storageDelimiter +
		base64.StdEncoding.EncodeToString(ciphertext), nil
}

func (cs *CryptoService) DecryptFromStorage(value string, aadParts ...string) (string, error) {
	if value == "" {
		return "", nil
	}
	if !cs.HasDataKey() {
		return "", errors.New("data encryption key not configured")
	}
	if !isEncryptedStorageValue(value) {
		return "", errors.New("data not encrypted")
	}

	payload := strings.TrimPrefix(value, storagePrefix)
	parts := strings.SplitN(payload, storageDelimiter, 2)
	if len(parts) != 2 {
		return "", errors.New("invalid encrypted data format")
	}

	nonce, err := base64.StdEncoding.DecodeString(parts[0])
	if err != nil {
		return "", fmt.Errorf("failed to decode nonce: %w", err)
	}

	ciphertext, err := base64.StdEncoding.DecodeString(parts[1])
	if err != nil {
		return "", fmt.Errorf("failed to decode ciphertext: %w", err)
	}

	block, err := aes.NewCipher(cs.dataKey)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	if len(nonce) != gcm.NonceSize() {
		return "", fmt.Errorf("invalid nonce length: expected %d, got %d", gcm.NonceSize(), len(nonce))
	}

	aad := composeAAD(aadParts)
	plaintext, err := gcm.Open(nil, nonce, ciphertext, aad)
	if err != nil {
		return "", fmt.Errorf("decryption failed: %w", err)
	}

	return string(plaintext), nil
}

func (cs *CryptoService) IsEncryptedStorageValue(value string) bool {
	return isEncryptedStorageValue(value)
}

func composeAAD(parts []string) []byte {
	if len(parts) == 0 {
		return nil
	}
	return []byte(strings.Join(parts, "|"))
}

func isEncryptedStorageValue(value string) bool {
	return strings.HasPrefix(value, storagePrefix)
}

func (cs *CryptoService) DecryptPayload(payload *EncryptedPayload) ([]byte, error) {
	// 1. Validate timestamp (prevent replay attacks)
	if payload.TS != 0 {
		elapsed := time.Since(time.Unix(payload.TS, 0))
		if elapsed > 5*time.Minute || elapsed < -1*time.Minute {
			return nil, errors.New("timestamp invalid or expired")
		}
	}

	// 2. Decode base64url
	wrappedKey, err := base64.RawURLEncoding.DecodeString(payload.WrappedKey)
	if err != nil {
		return nil, fmt.Errorf("failed to decode wrapped key: %w", err)
	}

	iv, err := base64.RawURLEncoding.DecodeString(payload.IV)
	if err != nil {
		return nil, fmt.Errorf("failed to decode IV: %w", err)
	}

	ciphertext, err := base64.RawURLEncoding.DecodeString(payload.Ciphertext)
	if err != nil {
		return nil, fmt.Errorf("failed to decode ciphertext: %w", err)
	}

	var aad []byte
	if payload.AAD != "" {
		aad, err = base64.RawURLEncoding.DecodeString(payload.AAD)
		if err != nil {
			return nil, fmt.Errorf("failed to decode AAD: %w", err)
		}

		var aadData AADData
		if err := json.Unmarshal(aad, &aadData); err == nil {
			// Additional validation logic can be added here
		}
	}

	// 3. Decrypt AES key using RSA-OAEP
	aesKey, err := rsa.DecryptOAEP(sha256.New(), rand.Reader, cs.privateKey, wrappedKey, nil)
	if err != nil {
		return nil, fmt.Errorf("RSA decryption failed: %w", err)
	}

	// 4. Decrypt data using AES-GCM
	block, err := aes.NewCipher(aesKey)
	if err != nil {
		return nil, fmt.Errorf("failed to create AES cipher: %w", err)
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("failed to create GCM: %w", err)
	}

	if len(iv) != gcm.NonceSize() {
		return nil, fmt.Errorf("invalid IV length: expected %d, got %d", gcm.NonceSize(), len(iv))
	}

	plaintext, err := gcm.Open(nil, iv, ciphertext, aad)
	if err != nil {
		return nil, fmt.Errorf("decryption verification failed: %w", err)
	}

	return plaintext, nil
}

func (cs *CryptoService) DecryptSensitiveData(payload *EncryptedPayload) (string, error) {
	plaintext, err := cs.DecryptPayload(payload)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

// GenerateKeyPair generates RSA key pair (for key generation during initialization)
// Returns PEM format private key and public key
func GenerateKeyPair() (privateKeyPEM, publicKeyPEM string, err error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", "", err
	}

	// Encode private key
	privPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
	})

	// Encode public key
	publicKeyDER, err := x509.MarshalPKIXPublicKey(&privateKey.PublicKey)
	if err != nil {
		return "", "", err
	}

	pubPEM := pem.EncodeToMemory(&pem.Block{
		Type:  "PUBLIC KEY",
		Bytes: publicKeyDER,
	})

	return string(privPEM), string(pubPEM), nil
}

// GenerateDataKey generates AES data encryption key
// Returns Base64 encoded 32-byte key
func GenerateDataKey() (string, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(key), nil
}

// ============================================================================
// EncryptedString - GORM custom type for automatic encryption/decryption
// ============================================================================

// Global crypto service for EncryptedString
var globalCryptoService *CryptoService

// SetGlobalCryptoService sets the global crypto service for EncryptedString
func SetGlobalCryptoService(cs *CryptoService) {
	globalCryptoService = cs
}

// EncryptedString is a custom type that automatically encrypts on save and decrypts on load
// Usage: Use EncryptedString instead of string for sensitive fields in GORM models
type EncryptedString string

// Scan implements sql.Scanner - called when reading from database
// Automatically decrypts the value
func (es *EncryptedString) Scan(value interface{}) error {
	if value == nil {
		*es = ""
		return nil
	}

	var str string
	switch v := value.(type) {
	case string:
		str = v
	case []byte:
		str = string(v)
	default:
		*es = ""
		return nil
	}

	// Decrypt if crypto service is set.
	// Decryption failure must propagate: silently keeping the ciphertext
	// (ENC:v1:...) as if it were plaintext lets it reach exchange API
	// calls, agent decisions, and trader keys downstream.
	if globalCryptoService != nil && str != "" && globalCryptoService.IsEncryptedStorageValue(str) {
		decrypted, err := globalCryptoService.DecryptFromStorage(str)
		if err != nil {
			return fmt.Errorf("decrypt EncryptedString from storage: %w", err)
		}
		*es = EncryptedString(decrypted)
	} else {
		*es = EncryptedString(str)
	}
	return nil
}

// Value implements driver.Valuer - called when writing to database
// Automatically encrypts the value
func (es EncryptedString) Value() (driver.Value, error) {
	if es == "" {
		return "", nil
	}

	// Encrypt if crypto service is set.
	// Encryption failure must propagate: silently writing plaintext to the
	// database violates the F002 invariant that secrets never land
	// unencrypted (DECISION #2). Fail the write rather than leak the secret.
	if globalCryptoService != nil {
		encrypted, err := globalCryptoService.EncryptForStorage(string(es))
		if err != nil {
			return nil, fmt.Errorf("encrypt EncryptedString for storage: %w", err)
		}
		return encrypted, nil
	}
	return string(es), nil
}

// String returns the plaintext string value
func (es EncryptedString) String() string {
	return string(es)
}
