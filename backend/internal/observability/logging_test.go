package observability

import (
	"bytes"
	"strings"
	"testing"

	"github.com/KoinaAI/conduit/backend/internal/config"
)

func TestConfigureDefaultLoggerSupportsJSON(t *testing.T) {
	t.Parallel()

	var buffer bytes.Buffer
	logger := ConfigureDefaultLogger(config.Config{
		AdminToken: "admin-token",
		LogFormat:  "json",
		LogLevel:   "warn",
	}, &buffer)
	logger.Warn("gateway started", "component", "test")

	output := buffer.String()
	if !strings.Contains(output, `"msg":"gateway started"`) {
		t.Fatalf("expected JSON slog output, got %q", output)
	}
	if !strings.Contains(output, `"component":"test"`) {
		t.Fatalf("expected structured slog field, got %q", output)
	}
}

func TestConfigureDefaultLoggerDefaultsToText(t *testing.T) {
	t.Parallel()

	var buffer bytes.Buffer
	logger := ConfigureDefaultLogger(config.Config{
		AdminToken: "admin-token",
	}, &buffer)
	logger.Info("gateway started", "component", "test")

	output := buffer.String()
	if !strings.Contains(output, "msg=\"gateway started\"") {
		t.Fatalf("expected text slog output, got %q", output)
	}
	if !strings.Contains(output, "component=test") {
		t.Fatalf("expected structured slog field, got %q", output)
	}
}
