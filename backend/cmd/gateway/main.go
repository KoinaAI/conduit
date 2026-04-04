package main

import (
	"log/slog"
	"os"

	"github.com/KoinaAI/conduit/backend/internal/app"
	"github.com/KoinaAI/conduit/backend/internal/config"
	"github.com/KoinaAI/conduit/backend/internal/observability"
)

func main() {
	cfg := config.Load()
	observability.ConfigureDefaultLogger(cfg, nil)
	if err := app.Run(cfg); err != nil {
		slog.Error("gateway stopped with error", "error", err)
		os.Exit(1)
	}
}
