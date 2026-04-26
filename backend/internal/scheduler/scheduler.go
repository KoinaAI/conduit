package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/admin"
)

// defaultCheckinInterval is the cadence at which integration check-ins fire
// when no explicit override is configured.
const defaultCheckinInterval = time.Hour

type Service struct {
	admin           *admin.Handlers
	stop            chan struct{}
	probeInterval   time.Duration
	pricingInterval time.Duration
	checkinInterval time.Duration
}

func New(admin *admin.Handlers, probeInterval, pricingInterval time.Duration) *Service {
	return NewWithCheckinInterval(admin, probeInterval, pricingInterval, defaultCheckinInterval)
}

// NewWithCheckinInterval returns a Service with a custom check-in cadence.
// A non-positive checkinInterval falls back to the default.
func NewWithCheckinInterval(admin *admin.Handlers, probeInterval, pricingInterval, checkinInterval time.Duration) *Service {
	if checkinInterval <= 0 {
		checkinInterval = defaultCheckinInterval
	}
	return &Service{
		admin:           admin,
		stop:            make(chan struct{}),
		probeInterval:   probeInterval,
		pricingInterval: pricingInterval,
		checkinInterval: checkinInterval,
	}
}

func (s *Service) Start(ctx context.Context) {
	checkinTicker := time.NewTicker(s.checkinInterval)
	defer checkinTicker.Stop()

	var probeTicker *time.Ticker
	var probeCh <-chan time.Time
	if s.probeInterval > 0 {
		probeTicker = time.NewTicker(s.probeInterval)
		probeCh = probeTicker.C
		defer probeTicker.Stop()
	}

	var pricingTicker *time.Ticker
	var pricingCh <-chan time.Time
	if s.pricingInterval > 0 {
		pricingTicker = time.NewTicker(s.pricingInterval)
		pricingCh = pricingTicker.C
		defer pricingTicker.Stop()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-checkinTicker.C:
			runSafely("checkins", func() {
				s.admin.RunCheckins(ctx)
			})
		case <-probeCh:
			runSafely("probes", func() {
				s.admin.RunProbes(ctx)
			})
		case <-pricingCh:
			runSafely("pricing_sync", func() {
				s.admin.RunPricingSync(ctx)
			})
		}
	}
}

func (s *Service) Stop() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
		slog.Info("scheduler stopped")
	}
}

func runSafely(name string, fn func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("scheduler task panicked", "task", name, "panic", recovered)
		}
	}()
	fn()
}
