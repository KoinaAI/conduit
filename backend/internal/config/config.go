package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	BindAddress                string
	StatePath                  string
	DatabaseURL                string
	AdminToken                 string
	GatewaySecretLookupPepper  string
	LogFormat                  string
	LogLevel                   string
	LogFile                    string
	LogMaxSizeMB               int
	LogMaxBackups              int
	GatewayAllowedOrigins      []string
	AdminAllowedOrigins        []string
	RealtimeAllowedOrigins     []string
	EnableRealtime             bool
	RequestHistory             int
	BootstrapGatewayKey        string
	ProbeIntervalSeconds       int
	BackupDirectory            string
	BackupIntervalSeconds      int
	BackupRetention            int
	PricingSyncEnabled         bool
	PricingCatalogURL          string
	PricingSyncIntervalSeconds int
	RedisAddr                  string
	RedisPassword              string
	RedisDB                    int
	RedisKeyPrefix             string
}

func Load() Config {
	return Config{
		BindAddress:                getenv("GATEWAY_BIND", ":8080"),
		StatePath:                  getenv("GATEWAY_STATE_PATH", "./data/gateway.db"),
		DatabaseURL:                strings.TrimSpace(os.Getenv("GATEWAY_DATABASE_URL")),
		AdminToken:                 strings.TrimSpace(os.Getenv("GATEWAY_ADMIN_TOKEN")),
		GatewaySecretLookupPepper:  strings.TrimSpace(os.Getenv("GATEWAY_SECRET_LOOKUP_PEPPER")),
		LogFormat:                  strings.TrimSpace(getenv("GATEWAY_LOG_FORMAT", "text")),
		LogLevel:                   strings.TrimSpace(getenv("GATEWAY_LOG_LEVEL", "info")),
		LogFile:                    strings.TrimSpace(os.Getenv("GATEWAY_LOG_FILE")),
		LogMaxSizeMB:               getenvInt("GATEWAY_LOG_MAX_SIZE_MB", 20),
		LogMaxBackups:              getenvInt("GATEWAY_LOG_MAX_BACKUPS", 5),
		GatewayAllowedOrigins:      getenvCSV("GATEWAY_ALLOWED_ORIGINS", "*"),
		AdminAllowedOrigins:        getenvCSV("GATEWAY_ADMIN_ALLOWED_ORIGINS", ""),
		RealtimeAllowedOrigins:     getenvCSV("GATEWAY_REALTIME_ALLOWED_ORIGINS", ""),
		EnableRealtime:             getenvBool("GATEWAY_ENABLE_REALTIME", true),
		RequestHistory:             getenvInt("GATEWAY_REQUEST_HISTORY", 10000),
		BootstrapGatewayKey:        getenv("GATEWAY_BOOTSTRAP_GATEWAY_KEY", ""),
		ProbeIntervalSeconds:       getenvInt("GATEWAY_PROBE_INTERVAL_SECONDS", 180),
		BackupDirectory:            strings.TrimSpace(os.Getenv("GATEWAY_BACKUP_DIR")),
		BackupIntervalSeconds:      getenvInt("GATEWAY_BACKUP_INTERVAL_SECONDS", 21600),
		BackupRetention:            getenvInt("GATEWAY_BACKUP_RETENTION", 10),
		PricingSyncEnabled:         getenvBool("GATEWAY_PRICING_SYNC_ENABLED", false),
		PricingCatalogURL:          strings.TrimSpace(getenv("GATEWAY_PRICING_CATALOG_URL", "https://models.dev/api.json")),
		PricingSyncIntervalSeconds: getenvInt("GATEWAY_PRICING_SYNC_INTERVAL_SECONDS", 86400),
		RedisAddr:                  strings.TrimSpace(os.Getenv("GATEWAY_REDIS_ADDR")),
		RedisPassword:              os.Getenv("GATEWAY_REDIS_PASSWORD"),
		RedisDB:                    getenvInt("GATEWAY_REDIS_DB", 0),
		RedisKeyPrefix:             strings.TrimSpace(getenv("GATEWAY_REDIS_KEY_PREFIX", "conduit")),
	}
}

// Validate rejects unsafe runtime defaults that would expose the admin plane.
func (c Config) Validate() error {
	switch token := strings.TrimSpace(c.AdminToken); token {
	case "", "dev-admin-token", "change-this-admin-token":
		return errors.New("GATEWAY_ADMIN_TOKEN must be explicitly configured with a non-default value")
	}
	if c.ProbeIntervalSeconds < 0 {
		return errors.New("GATEWAY_PROBE_INTERVAL_SECONDS must be greater than or equal to 0")
	}
	if c.LogFile != "" {
		if c.LogMaxSizeMB <= 0 {
			return errors.New("GATEWAY_LOG_MAX_SIZE_MB must be greater than 0 when GATEWAY_LOG_FILE is set")
		}
		if c.LogMaxBackups <= 0 {
			return errors.New("GATEWAY_LOG_MAX_BACKUPS must be greater than 0 when GATEWAY_LOG_FILE is set")
		}
	}
	if c.BackupIntervalSeconds < 0 {
		return errors.New("GATEWAY_BACKUP_INTERVAL_SECONDS must be greater than or equal to 0")
	}
	if c.PricingSyncIntervalSeconds < 0 {
		return errors.New("GATEWAY_PRICING_SYNC_INTERVAL_SECONDS must be greater than or equal to 0")
	}
	if c.BackupDirectory != "" {
		if c.BackupIntervalSeconds <= 0 {
			return errors.New("GATEWAY_BACKUP_INTERVAL_SECONDS must be greater than 0 when GATEWAY_BACKUP_DIR is set")
		}
		if c.BackupRetention <= 0 {
			return errors.New("GATEWAY_BACKUP_RETENTION must be greater than 0 when GATEWAY_BACKUP_DIR is set")
		}
	}
	if c.PricingSyncEnabled {
		if strings.TrimSpace(c.PricingCatalogURL) == "" {
			return errors.New("GATEWAY_PRICING_CATALOG_URL is required when GATEWAY_PRICING_SYNC_ENABLED is set")
		}
		if c.PricingSyncIntervalSeconds <= 0 {
			return errors.New("GATEWAY_PRICING_SYNC_INTERVAL_SECONDS must be greater than 0 when GATEWAY_PRICING_SYNC_ENABLED is set")
		}
	}
	if c.RedisDB < 0 {
		return errors.New("GATEWAY_REDIS_DB must be greater than or equal to 0")
	}
	if c.DatabaseURL != "" {
		switch {
		case strings.HasPrefix(strings.ToLower(c.DatabaseURL), "postgres://"),
			strings.HasPrefix(strings.ToLower(c.DatabaseURL), "postgresql://"):
		default:
			return errors.New("GATEWAY_DATABASE_URL must start with postgres:// or postgresql://")
		}
	}
	switch strings.ToLower(strings.TrimSpace(c.LogFormat)) {
	case "", "text", "json":
	default:
		return errors.New("GATEWAY_LOG_FORMAT must be text or json")
	}
	switch strings.ToLower(strings.TrimSpace(c.LogLevel)) {
	case "", "debug", "info", "warn", "error":
	default:
		return errors.New("GATEWAY_LOG_LEVEL must be one of debug, info, warn, error")
	}
	return nil
}

func (c Config) AllowsGatewayOrigin(origin string) bool {
	return originAllowed(c.GatewayAllowedOrigins, origin)
}

func (c Config) AllowsAdminOrigin(origin string) bool {
	return originAllowed(c.AdminAllowedOrigins, origin)
}

func (c Config) AllowsRealtimeOrigin(origin string) bool {
	return originAllowed(c.RealtimeAllowedOrigins, origin)
}

func (c Config) SecretLookupPepper() string {
	return strings.TrimSpace(c.GatewaySecretLookupPepper)
}

func (c Config) StoreLocator() string {
	if strings.TrimSpace(c.DatabaseURL) != "" {
		return strings.TrimSpace(c.DatabaseURL)
	}
	return strings.TrimSpace(c.StatePath)
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func getenvBool(key string, fallback bool) bool {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	switch value {
	case "1", "true", "TRUE", "True", "yes", "YES", "on", "ON":
		return true
	case "0", "false", "FALSE", "False", "no", "NO", "off", "OFF":
		return false
	default:
		return fallback
	}
}

func getenvInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return n
}

func getenvCSV(key, fallback string) []string {
	value := os.Getenv(key)
	if value == "" {
		value = fallback
	}
	if strings.TrimSpace(value) == "" {
		return nil
	}

	items := strings.Split(value, ",")
	origins := make([]string, 0, len(items))
	for _, item := range items {
		normalized := normalizeOrigin(item)
		if normalized == "" {
			continue
		}
		origins = append(origins, normalized)
	}
	if len(origins) == 0 {
		return nil
	}
	return origins
}

func originAllowed(allowed []string, origin string) bool {
	normalizedOrigin := normalizeOrigin(origin)
	if normalizedOrigin == "" {
		return false
	}
	for _, candidate := range allowed {
		if candidate == "*" || candidate == normalizedOrigin {
			return true
		}
	}
	return false
}

func normalizeOrigin(origin string) string {
	trimmed := strings.TrimSpace(origin)
	if trimmed == "" {
		return ""
	}
	if trimmed == "*" {
		return "*"
	}
	return strings.ToLower(trimmed)
}
