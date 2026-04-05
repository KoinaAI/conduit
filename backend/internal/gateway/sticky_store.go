package gateway

import (
	"time"

	"github.com/KoinaAI/conduit/backend/internal/model"
)

type stickyBindingStore interface {
	LoadStickyBinding(key string, now time.Time) (stickyBinding, bool, error)
	SaveStickyBinding(key string, binding stickyBinding) error
	DeleteStickyBinding(key string) error
}

type stickyBindingRecord struct {
	Key     string
	Binding stickyBinding
}

type stickyBindingListingStore interface {
	ListStickyBindings(now time.Time) ([]stickyBindingRecord, error)
}

type roundRobinCounterStore interface {
	NextRoundRobinValue(key string) (uint64, error)
}

type gatewayKeyRuntimeStore interface {
	AcquireGatewayKey(key model.GatewayKey, now time.Time) error
	ReleaseGatewayKey(keyID string, costUSD float64, now time.Time) error
}

type providerRuntimeStore interface {
	AcquireProvider(provider model.Provider, now time.Time) error
	ReleaseProvider(providerID string, costUSD float64, now time.Time) error
}

type gatewayAuthRuntimeStore interface {
	GatewayAuthSourceLocked(source string, now time.Time) (bool, error)
	InvalidGatewayLookupCached(lookupHash, candidateFingerprint string, now time.Time) (bool, error)
	RecordGatewayAuthFailure(source, lookupHash, candidateFingerprint string, now time.Time) error
	ClearGatewayAuthFailures(source, lookupHash string) error
}

type endpointRuntimeStore interface {
	EndpointOpen(candidate resolvedCandidate, now time.Time) (bool, error)
	AcquireEndpoint(candidate resolvedCandidate, now time.Time) (bool, error)
	ReportEndpointSuccess(candidate resolvedCandidate, now time.Time, halfOpen bool) error
	ReportEndpointFailure(candidate resolvedCandidate, now time.Time, halfOpen bool) error
	LoadEndpointState(candidate resolvedCandidate) (endpointRuntimeState, bool, error)
	ResetEndpoint(candidate resolvedCandidate) error
}
