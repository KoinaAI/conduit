package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/KoinaAI/conduit/backend/internal/app"
	"github.com/KoinaAI/conduit/backend/internal/config"
	"github.com/KoinaAI/conduit/backend/internal/observability"
)

func main() {
	cfg := config.Load()
	if _, err := observability.ConfigureDefaultLogger(cfg, nil); err != nil {
		slog.Error("gateway logger initialization failed", "error", err)
		os.Exit(1)
	}
	shutdownTracing, err := observability.ConfigureTracing(context.Background())
	if err != nil {
		slog.Error("gateway tracing initialization failed", "error", err)
		os.Exit(1)
	}
	defer func() {
		if err := shutdownTracing(context.Background()); err != nil {
			slog.Error("gateway tracing shutdown failed", "error", err)
		}
	}()
	if err := app.Run(cfg); err != nil {
		slog.Error("gateway stopped with error", "error", err)
		os.Exit(1)
	}
}
