package config

import "testing"

func TestValidateRejectsUnsafeAdminToken(t *testing.T) {
	t.Parallel()

	cases := []Config{
		{AdminToken: ""},
		{AdminToken: "dev-admin-token"},
		{AdminToken: "change-this-admin-token"},
	}

	for _, cfg := range cases {
		if err := cfg.Validate(); err == nil {
			t.Fatalf("expected admin token %q to be rejected", cfg.AdminToken)
		}
	}
}

func TestValidateAcceptsExplicitAdminToken(t *testing.T) {
	t.Parallel()

	cfg := Config{AdminToken: "super-secret-admin-token"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected admin token to be accepted: %v", err)
	}
}

func TestOriginChecks(t *testing.T) {
	t.Parallel()

	cfg := Config{
		GatewayAllowedOrigins:  []string{"*"},
		AdminAllowedOrigins:    []string{"https://admin.example"},
		RealtimeAllowedOrigins: []string{"https://console.example"},
	}

	if !cfg.AllowsGatewayOrigin("https://any.example") {
		t.Fatal("expected wildcard gateway origin to be allowed")
	}
	if !cfg.AllowsAdminOrigin("https://admin.example") {
		t.Fatal("expected admin origin to be allowed")
	}
	if cfg.AllowsAdminOrigin("https://evil.example") {
		t.Fatal("did not expect unrelated admin origin to be allowed")
	}
	if !cfg.AllowsRealtimeOrigin("https://console.example") {
		t.Fatal("expected realtime origin to be allowed")
	}
}
