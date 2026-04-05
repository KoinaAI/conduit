package gateway

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"

	"github.com/KoinaAI/conduit/backend/internal/config"
	"github.com/KoinaAI/conduit/backend/internal/model"
)

func TestRedisStickyBindingsAreSharedAcrossRuntimeInstances(t *testing.T) {
	t.Parallel()

	redisServer := miniredis.RunT(t)
	store := newRedisStickyStore(config.Config{
		RedisAddr:      redisServer.Addr(),
		RedisKeyPrefix: "conduit-test",
	})

	first := newRuntimeState(store)
	second := newRuntimeState(store)
	now := time.Now().UTC()
	candidate := resolvedCandidate{
		provider: model.Provider{
			ID:                      "provider-1",
			StickySessionTTLSeconds: 300,
		},
		route: model.ModelRoute{
			Alias: "gpt-5.4",
		},
		endpoint: model.ProviderEndpoint{
			ID: "endpoint-1",
		},
		credential: model.ProviderCredential{
			ID: "credential-1",
		},
		gatewayKeyID: "gateway-key-1",
	}

	first.bindStickyCandidate(candidate, "session-1", now)

	binding, ok := second.stickyBindingFor("gateway-key-1", stickyRouteKey("gpt-5.4", ""), "session-1", now)
	if !ok {
		t.Fatal("expected sticky binding to be loaded from redis-backed cache")
	}
	if binding.ProviderID != "provider-1" || binding.EndpointID != "endpoint-1" || binding.CredentialID != "credential-1" {
		t.Fatalf("unexpected sticky binding: %+v", binding)
	}
}
