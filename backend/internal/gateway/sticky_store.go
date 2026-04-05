package gateway

import "time"

type stickyBindingStore interface {
	LoadStickyBinding(key string, now time.Time) (stickyBinding, bool, error)
	SaveStickyBinding(key string, binding stickyBinding) error
	DeleteStickyBinding(key string) error
}
