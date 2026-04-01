package app

import (
	"context"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/admin"
	"github.com/KoinaAI/conduit/backend/internal/config"
	"github.com/KoinaAI/conduit/backend/internal/gateway"
	"github.com/KoinaAI/conduit/backend/internal/integration"
	"github.com/KoinaAI/conduit/backend/internal/model"
	"github.com/KoinaAI/conduit/backend/internal/scheduler"
	"github.com/KoinaAI/conduit/backend/internal/store"
)

type App struct {
	cfg       config.Config
	store     *store.FileStore
	admin     *admin.Handlers
	gateway   *gateway.Service
	scheduler *scheduler.Service
}

func New(cfg config.Config) (*App, error) {
	store, err := store.Open(cfg.StatePath)
	if err != nil {
		return nil, err
	}
	integrationService := integration.NewService()
	gatewayService := gateway.NewService(cfg, store)
	if err := ensureBootstrapGatewayKey(cfg, store); err != nil {
		return nil, err
	}
	adminHandlers := admin.New(cfg, store, integrationService, gatewayService)

	return &App{
		cfg:       cfg,
		store:     store,
		admin:     adminHandlers,
		gateway:   gatewayService,
		scheduler: scheduler.New(adminHandlers, time.Duration(cfg.ProbeIntervalSeconds)*time.Second),
	}, nil
}

func (a *App) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	adminMux := http.NewServeMux()
	adminMux.HandleFunc("GET /api/admin/state", a.admin.GetState)
	adminMux.HandleFunc("PUT /api/admin/state", a.admin.PutState)
	adminMux.HandleFunc("POST /api/admin/integrations/sync", a.admin.SyncAllIntegrations)
	adminMux.HandleFunc("GET /api/admin/providers", a.admin.ListProviders)
	adminMux.HandleFunc("POST /api/admin/providers", a.admin.CreateProvider)
	adminMux.HandleFunc("GET /api/admin/providers/{id}", a.admin.GetProvider)
	adminMux.HandleFunc("PUT /api/admin/providers/{id}", a.admin.UpdateProvider)
	adminMux.HandleFunc("DELETE /api/admin/providers/{id}", a.admin.DeleteProvider)
	adminMux.HandleFunc("GET /api/admin/routes", a.admin.ListRoutes)
	adminMux.HandleFunc("POST /api/admin/routes", a.admin.CreateRoute)
	adminMux.HandleFunc("GET /api/admin/routes/{alias}", a.admin.GetRoute)
	adminMux.HandleFunc("PUT /api/admin/routes/{alias}", a.admin.UpdateRoute)
	adminMux.HandleFunc("DELETE /api/admin/routes/{alias}", a.admin.DeleteRoute)
	adminMux.HandleFunc("GET /api/admin/pricing-profiles", a.admin.ListPricingProfiles)
	adminMux.HandleFunc("POST /api/admin/pricing-profiles", a.admin.CreatePricingProfile)
	adminMux.HandleFunc("PUT /api/admin/pricing-profiles/{id}", a.admin.UpdatePricingProfile)
	adminMux.HandleFunc("DELETE /api/admin/pricing-profiles/{id}", a.admin.DeletePricingProfile)
	adminMux.HandleFunc("GET /api/admin/integrations", a.admin.ListIntegrations)
	adminMux.HandleFunc("POST /api/admin/integrations", a.admin.CreateIntegration)
	adminMux.HandleFunc("PUT /api/admin/integrations/{id}", a.admin.UpdateIntegration)
	adminMux.HandleFunc("DELETE /api/admin/integrations/{id}", a.admin.DeleteIntegration)
	adminMux.HandleFunc("POST /api/admin/integrations/{id}/sync", a.admin.SyncIntegration)
	adminMux.HandleFunc("POST /api/admin/integrations/{id}/checkin", a.admin.CheckinIntegration)
	adminMux.HandleFunc("GET /api/admin/gateway-keys", a.admin.ListGatewayKeys)
	adminMux.HandleFunc("POST /api/admin/gateway-keys", a.admin.CreateGatewayKey)
	adminMux.HandleFunc("DELETE /api/admin/gateway-keys/{id}", a.admin.DeleteGatewayKey)
	adminMux.HandleFunc("GET /api/admin/request-history", a.admin.ListRequestHistory)
	adminMux.HandleFunc("GET /api/admin/request-history/{id}/attempts", a.admin.GetRequestAttempts)
	adminMux.HandleFunc("GET /api/admin/meta", a.admin.GetMeta)
	adminMux.HandleFunc("GET /api/admin/openapi.json", a.admin.OpenAPI)
	adminMux.HandleFunc("POST /api/admin/maintenance/probes", a.admin.ProbeAllProviders)
	mux.Handle("/api/admin/", a.admin.Middleware(adminMux))

	mux.HandleFunc("GET /v1/models", a.gateway.HandleModels)
	mux.HandleFunc("POST /v1/chat/completions", a.gateway.ProxyHTTP(model.ProtocolOpenAIChat))
	mux.HandleFunc("POST /v1/responses", a.gateway.ProxyHTTP(model.ProtocolOpenAIResponses))
	mux.HandleFunc("POST /v1/messages", a.gateway.ProxyHTTP(model.ProtocolAnthropic))
	mux.HandleFunc("POST /v1beta/models/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ":streamGenerateContent") {
			a.gateway.ProxyHTTP(model.ProtocolGeminiStream)(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, ":generateContent") {
			a.gateway.ProxyHTTP(model.ProtocolGeminiGenerate)(w, r)
			return
		}
		http.NotFound(w, r)
	})
	if a.cfg.EnableRealtime {
		mux.HandleFunc("GET /v1/realtime", a.gateway.ProxyRealtime)
	}

	return withCORS(mux)
}

func (a *App) StartBackground(ctx context.Context) {
	go a.scheduler.Start(ctx)
}

func Run(cfg config.Config) error {
	app, err := New(cfg)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	app.StartBackground(ctx)

	server := &http.Server{
		Addr:              cfg.BindAddress,
		Handler:           app.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("gateway listening on %s", cfg.BindAddress)
	return server.ListenAndServe()
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-Admin-Token")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
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

	snapshot := stateStore.Snapshot()
	for _, key := range snapshot.GatewayKeys {
		if model.VerifyGatewaySecret(key.SecretHash, secret) {
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
			if model.VerifyGatewaySecret(key.SecretHash, secret) {
				return nil
			}
		}
		state.GatewayKeys = append([]model.GatewayKey{{
			ID:            model.NewID("gk"),
			Name:          "bootstrap",
			SecretHash:    hash,
			SecretPreview: model.SecretPreview(secret),
			Enabled:       true,
			CreatedAt:     now,
			UpdatedAt:     now,
			Notes:         "bootstrapped from environment",
		}}, state.GatewayKeys...)
		return nil
	})
	return err
}
