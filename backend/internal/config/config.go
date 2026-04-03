package config

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	BindAddress               string
	StatePath                 string
	AdminToken                string
	GatewaySecretLookupPepper string
	GatewayAllowedOrigins     []string
	AdminAllowedOrigins       []string
	RealtimeAllowedOrigins    []string
	EnableRealtime            bool
	RequestHistory            int
	BootstrapGatewayKey       string
	ProbeIntervalSeconds      int
}

func Load() Config {
	return Config{
		BindAddress:               getenv("GATEWAY_BIND", ":8080"),
		StatePath:                 getenv("GATEWAY_STATE_PATH", "./data/gateway.db"),
		AdminToken:                strings.TrimSpace(os.Getenv("GATEWAY_ADMIN_TOKEN")),
		GatewaySecretLookupPepper: strings.TrimSpace(os.Getenv("GATEWAY_SECRET_LOOKUP_PEPPER")),
		GatewayAllowedOrigins:     getenvCSV("GATEWAY_ALLOWED_ORIGINS", "*"),
		AdminAllowedOrigins:       getenvCSV("GATEWAY_ADMIN_ALLOWED_ORIGINS", ""),
		RealtimeAllowedOrigins:    getenvCSV("GATEWAY_REALTIME_ALLOWED_ORIGINS", ""),
		EnableRealtime:            getenvBool("GATEWAY_ENABLE_REALTIME", true),
		RequestHistory:            getenvInt("GATEWAY_REQUEST_HISTORY", 200),
		BootstrapGatewayKey:       getenv("GATEWAY_BOOTSTRAP_GATEWAY_KEY", ""),
		ProbeIntervalSeconds:      getenvInt("GATEWAY_PROBE_INTERVAL_SECONDS", 180),
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
