package observability

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KoinaAI/conduit/backend/internal/config"
)

func TestConfigureDefaultLoggerSupportsJSON(t *testing.T) {
	t.Parallel()

	var buffer bytes.Buffer
	logger, err := ConfigureDefaultLogger(config.Config{
		AdminToken: "admin-token",
		LogFormat:  "json",
		LogLevel:   "warn",
	}, &buffer)
	if err != nil {
		t.Fatalf("configure logger: %v", err)
	}
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
	logger, err := ConfigureDefaultLogger(config.Config{
		AdminToken: "admin-token",
	}, &buffer)
	if err != nil {
		t.Fatalf("configure logger: %v", err)
	}
	logger.Info("gateway started", "component", "test")

	output := buffer.String()
	if !strings.Contains(output, "msg=\"gateway started\"") {
		t.Fatalf("expected text slog output, got %q", output)
	}
	if !strings.Contains(output, "component=test") {
		t.Fatalf("expected structured slog field, got %q", output)
	}
}

func TestRotatingWriterRotatesAndRetainsBackups(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "conduit.log")
	writer, err := newRotatingWriter(path, 1, 2)
	if err != nil {
		t.Fatalf("new rotating writer: %v", err)
	}
	writer.maxBytes = 32

	for index := 0; index < 6; index++ {
		if _, err := writer.Write([]byte("0123456789abcdef\n")); err != nil {
			t.Fatalf("write log chunk %d: %v", index, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close rotating writer: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected active log file to exist: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected first rotated log file to exist: %v", err)
	}
	if _, err := os.Stat(path + ".2"); err != nil {
		t.Fatalf("expected second rotated log file to exist: %v", err)
	}
	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("expected rotation retention to cap at 2 backups, err=%v", err)
	}
}
