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
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
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
		"Authorization, Content-Type, X-API-Key",
		"GET, OPTIONS",
		false,
	))
	mux.Handle("/v1/chat/completions", withCORS(
		methodHandler(http.MethodPost, http.HandlerFunc(a.gateway.ProxyHTTP(model.ProtocolOpenAIChat))),
		a.cfg,
		a.cfg.GatewayAllowedOrigins,
		"Authorization, Content-Type, X-API-Key",
		"POST, OPTIONS",
		false,
	))
	mux.Handle("/v1/responses", withCORS(
		methodHandler(http.MethodPost, http.HandlerFunc(a.gateway.ProxyHTTP(model.ProtocolOpenAIResponses))),
		a.cfg,
		a.cfg.GatewayAllowedOrigins,
		"Authorization, Content-Type, X-API-Key",
		"POST, OPTIONS",
		false,
	))
	mux.Handle("/v1/messages", withCORS(
		methodHandler(http.MethodPost, http.HandlerFunc(a.gateway.ProxyHTTP(model.ProtocolAnthropic))),
		a.cfg,
		a.cfg.GatewayAllowedOrigins,
		"Authorization, Content-Type, X-API-Key",
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
		"Authorization, Content-Type, X-API-Key",
		"POST, OPTIONS",
		false,
	))
	if a.cfg.EnableRealtime {
		mux.Handle("/v1/realtime", withCORS(
			methodHandler(http.MethodGet, http.HandlerFunc(a.gateway.ProxyRealtime)),
			a.cfg,
			a.cfg.RealtimeAllowedOrigins,
			"Authorization, Content-Type, X-API-Key",
			"GET, OPTIONS",
			false,
		))
	}

	return mux
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
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	log.Printf("gateway listening on %s", cfg.BindAddress)
	return server.ListenAndServe()
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
	lookupHash := model.GatewaySecretLookupHash(secret, cfg.SecretLookupPepper())

	snapshot := stateStore.Snapshot()
	for _, key := range snapshot.GatewayKeys {
		if model.VerifyGatewaySecret(key.SecretHash, secret) {
			if key.SecretLookupHash != lookupHash {
				_, err := stateStore.Update(func(state *model.State) error {
					for index := range state.GatewayKeys {
						if state.GatewayKeys[index].ID == key.ID {
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
			if model.VerifyGatewaySecret(key.SecretHash, secret) {
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
