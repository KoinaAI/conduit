package app

import (
	"net/http"
	"testing"
)

func TestNewHTTPServerLeavesWriteTimeoutDisabledForStreaming(t *testing.T) {
	t.Parallel()

	server := newHTTPServer(":8080", http.NewServeMux())
	if server.WriteTimeout != 0 {
		t.Fatalf("expected write timeout to stay disabled for streaming responses, got %v", server.WriteTimeout)
	}
	if server.ReadTimeout <= 0 || server.ReadHeaderTimeout <= 0 || server.IdleTimeout <= 0 {
		t.Fatalf("expected read/header/idle timeouts to remain configured: %+v", server)
	}
}
