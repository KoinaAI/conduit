package model

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/bcrypt"
)

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
	hashed, err := bcrypt.GenerateFromPassword([]byte(strings.TrimSpace(secret)), bcrypt.DefaultCost)
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

// SecretPreview formats a short non-sensitive preview for the admin console.
func SecretPreview(secret string) string {
	trimmed := strings.TrimSpace(secret)
	if len(trimmed) <= 8 {
		return trimmed
	}
	return fmt.Sprintf("%s...%s", trimmed[:6], trimmed[len(trimmed)-4:])
}
