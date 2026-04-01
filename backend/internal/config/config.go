package config

import (
	"os"
	"strconv"
)

type Config struct {
	BindAddress          string
	StatePath            string
	AdminToken           string
	EnableRealtime       bool
	RequestHistory       int
	BootstrapGatewayKey  string
	ProbeIntervalSeconds int
}

func Load() Config {
	return Config{
		BindAddress:          getenv("GATEWAY_BIND", ":8080"),
		StatePath:            getenv("GATEWAY_STATE_PATH", "./data/gateway.db"),
		AdminToken:           getenv("GATEWAY_ADMIN_TOKEN", "dev-admin-token"),
		EnableRealtime:       getenvBool("GATEWAY_ENABLE_REALTIME", true),
		RequestHistory:       getenvInt("GATEWAY_REQUEST_HISTORY", 200),
		BootstrapGatewayKey:  getenv("GATEWAY_BOOTSTRAP_GATEWAY_KEY", ""),
		ProbeIntervalSeconds: getenvInt("GATEWAY_PROBE_INTERVAL_SECONDS", 180),
	}
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
