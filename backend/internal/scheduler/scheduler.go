package scheduler

import (
	"context"
	"log"
	"time"

	"github.com/KoinaAI/conduit/backend/internal/admin"
)

type Service struct {
	admin         *admin.Handlers
	stop          chan struct{}
	probeInterval time.Duration
}

func New(admin *admin.Handlers, probeInterval time.Duration) *Service {
	return &Service{
		admin:         admin,
		stop:          make(chan struct{}),
		probeInterval: probeInterval,
	}
}

func (s *Service) Start(ctx context.Context) {
	checkinTicker := time.NewTicker(1 * time.Hour)
	defer checkinTicker.Stop()

	var probeTicker *time.Ticker
	var probeCh <-chan time.Time
	if s.probeInterval > 0 {
		probeTicker = time.NewTicker(s.probeInterval)
		probeCh = probeTicker.C
		defer probeTicker.Stop()
	}

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.stop:
			return
		case <-checkinTicker.C:
			s.admin.RunCheckins(ctx)
		case <-probeCh:
			s.admin.RunProbes(ctx)
		}
	}
}

func (s *Service) Stop() {
	select {
	case <-s.stop:
	default:
		close(s.stop)
		log.Print("scheduler stopped")
	}
}
