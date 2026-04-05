package scheduler

import (
	"testing"
	"time"
)

func TestRunSafelyRecoversPanic(t *testing.T) {
	t.Parallel()

	completed := false
	runSafely("panic-test", func() {
		defer func() {
			completed = true
		}()
		panic("boom")
	})
	if !completed {
		t.Fatal("expected panic wrapper to complete deferred cleanup")
	}
}

func TestStopIsIdempotent(t *testing.T) {
	t.Parallel()

	service := New(nil, time.Second, time.Second)
	service.Stop()
	service.Stop()
}
