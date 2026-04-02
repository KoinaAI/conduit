package model

import (
	"strings"
	"testing"
)

func TestGatewaySecretLookupHashUsesPepper(t *testing.T) {
	t.Parallel()

	secret := "uag-test-secret"
	withPepper := GatewaySecretLookupHash(secret, "pepper-a")
	withOtherPepper := GatewaySecretLookupHash(secret, "pepper-b")
	legacy := LegacyGatewaySecretLookupHash(secret)

	if withPepper == "" {
		t.Fatal("expected peppered lookup hash to be populated")
	}
	if withPepper == withOtherPepper {
		t.Fatal("expected different peppers to produce different lookup hashes")
	}
	if withPepper == legacy {
		t.Fatal("expected peppered lookup hash to differ from legacy lookup hash")
	}
	if GatewaySecretLookupHash(secret, "") != legacy {
		t.Fatal("expected empty pepper to preserve legacy lookup hash for compatibility")
	}
}

func TestHashGatewaySecretRejectsTooLongSecrets(t *testing.T) {
	t.Parallel()

	if _, err := HashGatewaySecret(strings.Repeat("a", 73)); err == nil {
		t.Fatal("expected secret longer than 72 bytes to be rejected")
	}
}

func TestSecretPreviewMasksShortSecrets(t *testing.T) {
	t.Parallel()

	cases := []string{"abc", "shortkey"}
	for _, secret := range cases {
		preview := SecretPreview(secret)
		if preview == secret {
			t.Fatalf("expected preview to mask short secret %q", secret)
		}
	}
}
