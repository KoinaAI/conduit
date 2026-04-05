package observability

import (
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/KoinaAI/conduit/backend/internal/config"
)

// ConfigureDefaultLogger installs the process-wide slog default used by the
// gateway. Text output is the default for local readability; JSON is available
// for machine-ingested logs.
func ConfigureDefaultLogger(cfg config.Config, writer io.Writer) (*slog.Logger, error) {
	if writer == nil {
		writer = os.Stdout
		if strings.TrimSpace(cfg.LogFile) != "" {
			rotating, err := newRotatingWriter(cfg.LogFile, cfg.LogMaxSizeMB, cfg.LogMaxBackups)
			if err != nil {
				return nil, err
			}
			writer = rotating
		}
	}
	options := &slog.HandlerOptions{
		Level: parseLevel(cfg.LogLevel),
	}
	var handler slog.Handler
	if strings.EqualFold(strings.TrimSpace(cfg.LogFormat), "json") {
		handler = slog.NewJSONHandler(writer, options)
	} else {
		handler = slog.NewTextHandler(writer, options)
	}
	logger := slog.New(handler)
	slog.SetDefault(logger)
	return logger, nil
}

func parseLevel(value string) slog.Leveler {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug
	case "warn":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
