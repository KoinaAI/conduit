package model

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

const maxGatewaySecretBytes = 72

// NewGatewaySecret returns a high-entropy secret suitable for public gateway use.
func NewGatewaySecret() (string, error) {
	var raw [24]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return "uag-" + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// HashGatewaySecret hashes a gateway secret for storage.
func HashGatewaySecret(secret string) (string, error) {
	trimmed := strings.TrimSpace(secret)
	if trimmed == "" {
		return "", errors.New("gateway secret is required")
	}
	if len([]byte(trimmed)) > maxGatewaySecretBytes {
		return "", fmt.Errorf("gateway secret must be %d bytes or fewer", maxGatewaySecretBytes)
	}
	hashed, err := bcrypt.GenerateFromPassword([]byte(trimmed), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hashed), nil
}

// VerifyGatewaySecret verifies a stored hash against a presented secret.
func VerifyGatewaySecret(hashedSecret, presentedSecret string) bool {
	if strings.TrimSpace(hashedSecret) == "" || strings.TrimSpace(presentedSecret) == "" {
		return false
	}
	return bcrypt.CompareHashAndPassword([]byte(hashedSecret), []byte(strings.TrimSpace(presentedSecret))) == nil
}

// GatewaySecretLookupHash returns a deterministic keyed lookup digest for the
// secret. It is used only to narrow candidate keys before the expensive bcrypt
// check.
func GatewaySecretLookupHash(secret, pepper string) string {
	trimmed := strings.TrimSpace(secret)
	if trimmed == "" {
		return ""
	}
	if strings.TrimSpace(pepper) == "" {
		return LegacyGatewaySecretLookupHash(trimmed)
	}
	mac := hmac.New(sha256.New, []byte(strings.TrimSpace(pepper)))
	_, _ = mac.Write([]byte(trimmed))
	return hex.EncodeToString(mac.Sum(nil))
}

// LegacyGatewaySecretLookupHash preserves the original unhasalted SHA-256
// lookup digest for backwards-compatible key matching during upgrades.
func LegacyGatewaySecretLookupHash(secret string) string {
	trimmed := strings.TrimSpace(secret)
	if trimmed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(trimmed))
	return hex.EncodeToString(sum[:])
}

// SecretPreview formats a short non-sensitive preview for the admin console.
func SecretPreview(secret string) string {
	trimmed := strings.TrimSpace(secret)
	switch {
	case len(trimmed) == 0:
		return ""
	case len(trimmed) <= 4:
		return strings.Repeat("*", len(trimmed))
	case len(trimmed) <= 8:
		return fmt.Sprintf("%s...%s", trimmed[:1], trimmed[len(trimmed)-1:])
	}
	return fmt.Sprintf("%s...%s", trimmed[:6], trimmed[len(trimmed)-4:])
}
