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
	withEmptyPepper := GatewaySecretLookupHash(secret, "")

	if withPepper == "" {
		t.Fatal("expected peppered lookup hash to be populated")
	}
	if withPepper == withOtherPepper {
		t.Fatal("expected different peppers to produce different lookup hashes")
	}
	if withEmptyPepper == "" {
		t.Fatal("expected empty pepper lookup hash to be populated")
	}
	if withEmptyPepper == withPepper {
		t.Fatal("expected peppered lookup hash to differ from empty-pepper hash")
	}
	if GatewaySecretLookupHash(secret, "") != withEmptyPepper {
		t.Fatal("expected empty pepper lookup hash to stay deterministic")
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
