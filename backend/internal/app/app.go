package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/admin"
	"github.com/KoinaAI/conduit/backend/internal/config"
	"github.com/KoinaAI/conduit/backend/internal/gateway"
	"github.com/KoinaAI/conduit/backend/internal/integration"
	"github.com/KoinaAI/conduit/backend/internal/metrics"
	"github.com/KoinaAI/conduit/backend/internal/model"
	"github.com/KoinaAI/conduit/backend/internal/scheduler"
	"github.com/KoinaAI/conduit/backend/internal/store"
)

type App struct {
	cfg              config.Config
	store            *store.FileStore
	admin            *admin.Handlers
	integration      *integration.Service
	gateway          *gateway.Service
	scheduler        *scheduler.Service
	backgroundCancel context.CancelFunc
	backgroundWG     sync.WaitGroup
	startedAt        time.Time
}

func New(cfg config.Config) (*App, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.GatewaySecretBcryptCost > 0 {
		model.SetGatewaySecretBcryptCost(cfg.GatewaySecretBcryptCost)
	}
	store, err := store.Open(cfg.StoreLocator(), store.WithRequestHistoryLimit(cfg.RequestHistory))
	if err != nil {
		return nil, err
	}
	integrationService := integration.NewService()
	gatewayService := gateway.NewService(cfg, store)
	if err := ensureBootstrapGatewayKey(cfg, store); err != nil {
		_ = store.Close()
		return nil, err
	}
	adminHandlers := admin.New(cfg, store, integrationService, gatewayService)

	return &App{
		cfg:         cfg,
		store:       store,
		admin:       adminHandlers,
		integration: integrationService,
		gateway:     gatewayService,
		scheduler: scheduler.NewWithCheckinInterval(
			adminHandlers,
			time.Duration(cfg.ProbeIntervalSeconds)*time.Second,
			pricingSyncInterval(cfg),
			time.Duration(cfg.CheckinIntervalSeconds)*time.Second,
		),
		startedAt: time.Now().UTC(),
	}, nil
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", a.Healthz)
	mux.Handle("GET /metrics", metrics.Handler(a))

	adminMux := http.NewServeMux()
	admin.RegisterRoutes(adminMux, a.admin)
	mux.Handle("/api/admin/", withCORS(
		a.admin.Middleware(adminMux),
		a.cfg,
		a.cfg.AdminAllowedOrigins,
		"Authorization, Content-Type, X-Admin-Token",
		"GET, POST, PUT, DELETE, OPTIONS",
		true,
	))

	mux.Handle("/v1/models", withCORS(
		methodHandler(http.MethodGet, http.HandlerFunc(a.gateway.HandleModels)),
		a.cfg,
		a.cfg.GatewayAllowedOrigins,
		gatewayAllowedHeaders(),
		"GET, OPTIONS",
		false,
	))
	mux.Handle("/v1/chat/completions", withCORS(
		methodHandler(http.MethodPost, http.HandlerFunc(a.gateway.ProxyHTTP(model.ProtocolOpenAIChat))),
		a.cfg,
		a.cfg.GatewayAllowedOrigins,
		gatewayAllowedHeaders(),
		"POST, OPTIONS",
		false,
	))
	mux.Handle("/v1/responses", withCORS(
		methodHandler(http.MethodPost, http.HandlerFunc(a.gateway.ProxyHTTP(model.ProtocolOpenAIResponses))),
		a.cfg,
		a.cfg.GatewayAllowedOrigins,
		gatewayAllowedHeaders(),
		"POST, OPTIONS",
		false,
	))
	mux.Handle("/v1/messages", withCORS(
		methodHandler(http.MethodPost, http.HandlerFunc(a.gateway.ProxyHTTP(model.ProtocolAnthropic))),
		a.cfg,
		a.cfg.GatewayAllowedOrigins,
		gatewayAllowedHeaders(),
		"POST, OPTIONS",
		false,
	))
	mux.Handle("/v1beta/models/", withCORS(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			switch protocol, ok := matchGeminiProtocol(r.URL.Path); {
			case ok && protocol == model.ProtocolGeminiStream:
				a.gateway.ProxyHTTP(model.ProtocolGeminiStream)(w, r)
				return
			case ok && protocol == model.ProtocolGeminiGenerate:
				a.gateway.ProxyHTTP(model.ProtocolGeminiGenerate)(w, r)
				return
			}
			http.NotFound(w, r)
		}),
		a.cfg,
		a.cfg.GatewayAllowedOrigins,
		gatewayAllowedHeaders(),
		"POST, OPTIONS",
		false,
	))
	if a.cfg.EnableRealtime {
		mux.Handle("/v1/realtime", withCORS(
			methodHandler(http.MethodGet, http.HandlerFunc(a.gateway.ProxyRealtime)),
			a.cfg,
			a.cfg.RealtimeAllowedOrigins,
			gatewayAllowedHeaders(),
			"GET, OPTIONS",
			false,
		))
	}

	return mux
}

// UptimeSeconds satisfies metrics.Source.
func (a *App) UptimeSeconds() float64 {
	return time.Since(a.startedAt).Seconds()
}

// HealthCounts satisfies metrics.Source.
func (a *App) HealthCounts(now time.Time) store.HealthCounts {
	return a.store.HealthCounts(now)
}

// ProviderUsage satisfies metrics.Source.
func (a *App) ProviderUsage(limit int) []gateway.ProviderRuntimeStatus {
	return a.gateway.ProviderUsage(limit)
}

// CircuitStatuses satisfies metrics.Source.
func (a *App) CircuitStatuses() []gateway.EndpointCircuitStatus {
	return a.gateway.CircuitStatuses()
}

// ActiveSessionsCount satisfies metrics.Source. Returns the count of live
// sessions currently tracked, with a generous discovery window so the gauge
// reflects the same view operators see in /api/admin/stats.
func (a *App) ActiveSessionsCount(_ time.Time) int {
	return len(a.gateway.ActiveSessions(0, 0))
}

// StickyBindingsCount satisfies metrics.Source.
func (a *App) StickyBindingsCount() int {
	return len(a.gateway.StickyBindings())
}

// Healthz reports process uptime plus database connectivity for external probes.
func (a *App) Healthz(w http.ResponseWriter, _ *http.Request) {
	status := http.StatusOK
	dbStatus := "ok"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := a.store.Ping(ctx); err != nil {
		status = http.StatusServiceUnavailable
		dbStatus = err.Error()
	}
	now := time.Now().UTC()
	counts := a.store.HealthCounts(now)
	healthStatus := "ok"
	if status != http.StatusOK {
		healthStatus = "degraded"
	}
	writeJSON(w, status, map[string]any{
		"status":         healthStatus,
		"server_time":    now,
		"uptime_seconds": int64(now.Sub(a.startedAt).Seconds()),
		"db_status":      dbStatus,
		"db_backend":     a.store.Backend(),
		"counts": map[string]any{
			"providers":             counts.Providers,
			"routes":                counts.Routes,
			"gateway_keys_total":    counts.GatewayKeysTotal,
			"gateway_keys_active":   counts.GatewayKeysActive,
			"integrations":          counts.Integrations,
			"pricing_profiles":      counts.PricingProfiles,
			"request_history_items": counts.RequestHistoryItems,
		},
	})
}

func (a *App) StartBackground(ctx context.Context) {
	backgroundCtx, cancel := context.WithCancel(ctx)
	a.backgroundCancel = cancel

	a.backgroundWG.Add(1)
	go func() {
		defer a.backgroundWG.Done()
		a.scheduler.Start(backgroundCtx)
	}()
	if strings.TrimSpace(a.cfg.BackupDirectory) != "" && a.cfg.BackupIntervalSeconds > 0 && a.store.SupportsBackup() {
		a.backgroundWG.Add(1)
		go func() {
			defer a.backgroundWG.Done()
			a.runBackupLoop(backgroundCtx)
		}()
	}
}

func (a *App) Close() error {
	if a == nil {
		return nil
	}
	if a.backgroundCancel != nil {
		a.backgroundCancel()
		a.backgroundCancel = nil
	}
	if a.scheduler != nil {
		a.scheduler.Stop()
	}
	a.backgroundWG.Wait()

	var errs []error
	if a.integration != nil {
		errs = append(errs, a.integration.Close())
	}
	if a.gateway != nil {
		errs = append(errs, a.gateway.Close())
	}
	if a.store != nil {
		errs = append(errs, a.store.Close())
	}
	return errors.Join(errs...)
}

func (a *App) runBackupLoop(ctx context.Context) {
	ticker := time.NewTicker(time.Duration(a.cfg.BackupIntervalSeconds) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			backupPath, err := a.store.Backup(a.cfg.BackupDirectory, a.cfg.BackupRetention)
			if err != nil {
				slog.Error("gateway backup failed", "error", err, "backup_dir", a.cfg.BackupDirectory)
				continue
			}
			slog.Info("gateway backup completed", "path", backupPath)
		}
	}
}

func newHTTPServer(addr string, handler http.Handler) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

func pricingSyncInterval(cfg config.Config) time.Duration {
	if !cfg.PricingSyncEnabled || cfg.PricingSyncIntervalSeconds <= 0 {
		return 0
	}
	return time.Duration(cfg.PricingSyncIntervalSeconds) * time.Second
}

func gatewayAllowedHeaders() string {
	return strings.Join([]string{
		"Authorization",
		"Content-Type",
		"X-API-Key",
		"X-Session-ID",
		"X-Routing-Scenario",
		"X-Codex-Turn-State",
		"X-Codex-Turn-Metadata",
		"X-Codex-Parent-Thread-Id",
		"X-Codex-Window-Id",
		"X-OpenAI-Subagent",
		"OpenAI-Beta",
		"X-ResponsesAPI-Include-Timing-Metrics",
	}, ", ")
}

func Run(cfg config.Config) error {
	app, err := New(cfg)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := app.Close(); closeErr != nil {
			slog.Error("gateway store close failed", "error", closeErr)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	app.StartBackground(ctx)

	server := newHTTPServer(cfg.BindAddress, app.Handler())
	slog.Info("gateway listening", "bind_address", cfg.BindAddress)
	errCh := make(chan error, 1)
	go func() {
		if serveErr := server.ListenAndServe(); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			errCh <- serveErr
		}
		close(errCh)
	}()

	select {
	case serveErr := <-errCh:
		return serveErr
	case <-ctx.Done():
	}
	slog.Info("gateway shutdown requested")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	shutdownErr := server.Shutdown(shutdownCtx)
	serveErr := <-errCh
	return errors.Join(shutdownErr, serveErr)
}

func withCORS(next http.Handler, cfg config.Config, allowedOrigins []string, allowedHeaders, allowedMethods string, rejectDisallowedOrigin bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		if origin != "" {
			if !originAllowed(cfg, allowedOrigins, origin) {
				if rejectDisallowedOrigin {
					http.Error(w, "origin not allowed", http.StatusForbidden)
					return
				}
			} else {
				if allowsAnyOrigin(allowedOrigins) {
					w.Header().Set("Access-Control-Allow-Origin", "*")
				} else {
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Add("Vary", "Origin")
				}
				w.Header().Set("Access-Control-Allow-Headers", allowedHeaders)
				w.Header().Set("Access-Control-Allow-Methods", allowedMethods)
			}
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func originAllowed(cfg config.Config, allowedOrigins []string, origin string) bool {
	for _, candidate := range allowedOrigins {
		if candidate == "*" {
			return true
		}
	}
	if len(allowedOrigins) == 0 {
		return false
	}
	switch {
	case slicesEqual(allowedOrigins, cfg.AdminAllowedOrigins):
		return cfg.AllowsAdminOrigin(origin)
	case slicesEqual(allowedOrigins, cfg.RealtimeAllowedOrigins):
		return cfg.AllowsRealtimeOrigin(origin)
	default:
		return cfg.AllowsGatewayOrigin(origin)
	}
}

func allowsAnyOrigin(allowedOrigins []string) bool {
	for _, candidate := range allowedOrigins {
		if candidate == "*" {
			return true
		}
	}
	return false
}

func slicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func matchGeminiProtocol(path string) (model.Protocol, bool) {
	const prefix = "/v1beta/models/"
	fragment, ok := strings.CutPrefix(path, prefix)
	if !ok || fragment == "" {
		return "", false
	}
	switch {
	case strings.HasSuffix(fragment, ":streamGenerateContent"):
		modelName := strings.TrimSuffix(fragment, ":streamGenerateContent")
		return validGeminiModelSegment(modelName, model.ProtocolGeminiStream)
	case strings.HasSuffix(fragment, ":generateContent"):
		modelName := strings.TrimSuffix(fragment, ":generateContent")
		return validGeminiModelSegment(modelName, model.ProtocolGeminiGenerate)
	default:
		return "", false
	}
}

func validGeminiModelSegment(segment string, protocol model.Protocol) (model.Protocol, bool) {
	if segment == "" || strings.Contains(segment, "/") {
		return "", false
	}
	return protocol, true
}

func methodHandler(method string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func ensureBootstrapGatewayKey(cfg config.Config, stateStore *store.FileStore) error {
	secret := strings.TrimSpace(cfg.BootstrapGatewayKey)
	if secret == "" {
		return nil
	}
	if err := model.ValidateGatewaySecretStrength(secret); err != nil {
		return err
	}
	lookupHash := model.GatewaySecretLookupHash(secret, cfg.SecretLookupPepper())

	snapshot := stateStore.Snapshot()
	matchedKeyID := ""
	for _, key := range snapshot.GatewayKeys {
		if model.VerifyGatewaySecret(key.SecretHash, secret) {
			matchedKeyID = key.ID
			if key.SecretLookupHash != lookupHash {
				_, err := stateStore.Update(func(state *model.State) error {
					for index := range state.GatewayKeys {
						if state.GatewayKeys[index].ID == matchedKeyID {
							state.GatewayKeys[index].SecretLookupHash = lookupHash
							state.GatewayKeys[index].UpdatedAt = time.Now().UTC()
							return nil
						}
					}
					return nil
				})
				if err != nil {
					return err
				}
			}
			return nil
		}
	}

	hash, err := model.HashGatewaySecret(secret)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	_, err = stateStore.Update(func(state *model.State) error {
		for _, key := range state.GatewayKeys {
			if key.ID == matchedKeyID || key.SecretLookupHash == lookupHash {
				return nil
			}
		}
		state.GatewayKeys = append([]model.GatewayKey{{
			ID:               model.NewID("gk"),
			Name:             "bootstrap",
			SecretHash:       hash,
			SecretLookupHash: lookupHash,
			SecretPreview:    model.SecretPreview(secret),
			Enabled:          true,
			CreatedAt:        now,
			UpdatedAt:        now,
			Notes:            "bootstrapped from environment",
		}}, state.GatewayKeys...)
		return nil
	})
	return err
}
