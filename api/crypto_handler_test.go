package api

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"nofx/crypto"

	"github.com/gin-gonic/gin"
)

// newTestCryptoHandler spins up a CryptoHandler backed by an ephemeral
// in-memory key pair. Used by the handler tests below; isolates each test
// from process-level env / globals.
func newTestCryptoHandler(t *testing.T) *CryptoHandler {
	t.Helper()

	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa keygen: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(rsaKey)
	if err != nil {
		t.Fatalf("marshal pkcs8: %v", err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})

	aes := make([]byte, 32)
	if _, err := rand.Read(aes); err != nil {
		t.Fatalf("rand aes: %v", err)
	}

	t.Setenv(crypto.EnvDataEncryptionKey, base64.StdEncoding.EncodeToString(aes))
	t.Setenv(crypto.EnvRSAPrivateKey, strings.ReplaceAll(string(pemBytes), "\n", "\\n"))

	cs, err := crypto.NewCryptoService()
	if err != nil {
		t.Fatalf("NewCryptoService: %v", err)
	}

	return NewCryptoHandler(cs)
}

func performJSON(h gin.HandlerFunc, body []byte) *httptest.ResponseRecorder {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.POST("/decrypt", h)

	req := httptest.NewRequest(http.MethodPost, "/decrypt", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	router.ServeHTTP(rr, req)
	return rr
}

// TestHandleDecryptSensitiveData_InvalidJSON locks in that malformed JSON
// returns 400 with INVALID_REQUEST (not the same error code as decrypt
// failure — F002 Contract distinguishes the two).
func TestHandleDecryptSensitiveData_InvalidJSON(t *testing.T) {
	h := newTestCryptoHandler(t)
	rr := performJSON(h.HandleDecryptSensitiveData, []byte("{not json"))

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v body=%s", err, rr.Body.String())
	}
	if resp["error"] != "INVALID_REQUEST" {
		t.Fatalf("want error=INVALID_REQUEST, got %q", resp["error"])
	}
}

// TestHandleDecryptSensitiveData_DecryptFails locks in F002-T05 item 1:
// when DecryptSensitiveData returns an error (e.g. corrupted ciphertext,
// missing TS, replay window violation), the handler must respond with
// 400 + CRYPTO_DECRYPT_FAIL — not 500 + generic message.
func TestHandleDecryptSensitiveData_DecryptFails(t *testing.T) {
	h := newTestCryptoHandler(t)

	// Well-formed payload but ciphertext is junk — DecryptPayload will fail.
	body, err := json.Marshal(map[string]any{
		"wrappedKey": base64.RawURLEncoding.EncodeToString([]byte("garbage")),
		"iv":         base64.RawURLEncoding.EncodeToString([]byte("not-an-iv")),
		"ciphertext": base64.RawURLEncoding.EncodeToString([]byte("nope")),
		"ts":         0, // also exercises T05 item 2: TS=0 rejection bubbles up
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	rr := performJSON(h.HandleDecryptSensitiveData, body)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d (body=%s)", rr.Code, rr.Body.String())
	}
	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not JSON: %v body=%s", err, rr.Body.String())
	}
	if resp["error"] != "CRYPTO_DECRYPT_FAIL" {
		t.Fatalf("want error=CRYPTO_DECRYPT_FAIL, got %q (body=%s)", resp["error"], rr.Body.String())
	}
	// The handler must never expose plaintext or the underlying error detail.
	if strings.Contains(rr.Body.String(), "plaintext") {
		t.Fatalf("response leaks plaintext key: %s", rr.Body.String())
	}
}
