package model

import "testing"

func TestGatewaySecretLookupHashUsesPepper(t *testing.T) {
	t.Parallel()

	secret := "uag-test-secret"
	withPepper := GatewaySecretLookupHash(secret, "pepper-a")
	withOtherPepper := GatewaySecretLookupHash(secret, "pepper-b")
	withoutPepper := GatewaySecretLookupHash(secret, "")

	if withPepper == "" {
		t.Fatal("expected peppered lookup hash to be populated")
	}
	if withPepper == withOtherPepper {
		t.Fatal("expected different peppers to produce different lookup hashes")
	}
	if withoutPepper == "" {
		t.Fatal("expected empty pepper to still produce a lookup hash")
	}
	if withPepper == withoutPepper {
		t.Fatal("expected peppered lookup hash to differ from empty-pepper lookup hash")
	}
}
