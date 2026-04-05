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

func TestValidateRejectsInvalidLogFileRotationConfig(t *testing.T) {
	t.Parallel()

	cfg := Config{
		AdminToken:    "super-secret-admin-token",
		LogFile:       "/tmp/conduit.log",
		LogMaxSizeMB:  0,
		LogMaxBackups: 5,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected invalid log rotation config to be rejected")
	}
}

func TestValidateRejectsInvalidBackupConfig(t *testing.T) {
	t.Parallel()

	cfg := Config{
		AdminToken:            "super-secret-admin-token",
		BackupDirectory:       "/tmp/conduit-backups",
		BackupIntervalSeconds: 0,
		BackupRetention:       5,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected invalid backup config to be rejected")
	}
}

func TestValidateAcceptsLogFileAndBackupConfig(t *testing.T) {
	t.Parallel()

	cfg := Config{
		AdminToken:            "super-secret-admin-token",
		LogFile:               "/tmp/conduit.log",
		LogMaxSizeMB:          20,
		LogMaxBackups:         5,
		BackupDirectory:       "/tmp/conduit-backups",
		BackupIntervalSeconds: 3600,
		BackupRetention:       10,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected log file and backup config to be accepted: %v", err)
	}
}

func TestValidateRejectsInvalidPricingSyncConfig(t *testing.T) {
	t.Parallel()

	cfg := Config{
		AdminToken:                 "super-secret-admin-token",
		PricingSyncEnabled:         true,
		PricingCatalogURL:          "https://models.dev/api.json",
		PricingSyncIntervalSeconds: 0,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected invalid pricing sync config to be rejected")
	}
}

func TestValidateAcceptsPricingSyncConfig(t *testing.T) {
	t.Parallel()

	cfg := Config{
		AdminToken:                 "super-secret-admin-token",
		PricingSyncEnabled:         true,
		PricingCatalogURL:          "https://models.dev/api.json",
		PricingSyncIntervalSeconds: 86400,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected pricing sync config to be accepted: %v", err)
	}
}

func TestValidateRejectsNegativeRedisDB(t *testing.T) {
	t.Parallel()

	cfg := Config{
		AdminToken: "super-secret-admin-token",
		RedisDB:    -1,
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected negative redis db to be rejected")
	}
}

func TestValidateRejectsUnsupportedDatabaseURLScheme(t *testing.T) {
	t.Parallel()

	cfg := Config{
		AdminToken:  "super-secret-admin-token",
		DatabaseURL: "sqlite:///tmp/gateway.db",
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected unsupported database url scheme to be rejected")
	}
}

func TestValidateAcceptsPostgresDatabaseURL(t *testing.T) {
	t.Parallel()

	cfg := Config{
		AdminToken:  "super-secret-admin-token",
		DatabaseURL: "postgres://conduit:secret@db.example/conduit?sslmode=disable",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected postgres database url to be accepted: %v", err)
	}
	if got := cfg.StoreLocator(); got != cfg.DatabaseURL {
		t.Fatalf("expected store locator to prefer database url, got %q", got)
	}
}

func TestValidateRejectsWeakBootstrapGatewayKey(t *testing.T) {
	t.Parallel()

	cfg := Config{
		AdminToken:          "super-secret-admin-token",
		BootstrapGatewayKey: "too-short",
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected weak bootstrap gateway key to be rejected")
	}
}

func TestValidateAcceptsStrongBootstrapGatewayKey(t *testing.T) {
	t.Parallel()

	cfg := Config{
		AdminToken:          "super-secret-admin-token",
		BootstrapGatewayKey: "bootstrap-secret-123",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected bootstrap gateway key to be accepted: %v", err)
	}
}
