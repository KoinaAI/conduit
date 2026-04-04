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

func TestValidateRejectsNegativeProbeInterval(t *testing.T) {
	t.Parallel()

	cfg := Config{
		AdminToken:           "super-secret-admin-token",
		ProbeIntervalSeconds: -1,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected negative probe interval to be rejected")
	}
}

func TestSecretLookupPepperUsesExplicitValueOnly(t *testing.T) {
	t.Parallel()

	cfg := Config{AdminToken: "admin-token"}
	if got := cfg.SecretLookupPepper(); got != "" {
		t.Fatalf("expected empty lookup pepper without explicit config, got %q", got)
	}
	cfg.GatewaySecretLookupPepper = "lookup-pepper"
	if got := cfg.SecretLookupPepper(); got != "lookup-pepper" {
		t.Fatalf("expected explicit lookup pepper, got %q", got)
	}
}

func TestValidateRejectsUnknownLogFormat(t *testing.T) {
	t.Parallel()

	cfg := Config{
		AdminToken: "super-secret-admin-token",
		LogFormat:  "yaml",
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected invalid log format to be rejected")
	}
}

func TestValidateRejectsUnknownLogLevel(t *testing.T) {
	t.Parallel()

	cfg := Config{
		AdminToken: "super-secret-admin-token",
		LogLevel:   "trace",
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected invalid log level to be rejected")
	}
}
