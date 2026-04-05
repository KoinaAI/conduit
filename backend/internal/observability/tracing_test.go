package observability

import (
	"context"
	"testing"
)

func TestConfigureTracingWithoutEndpointIsNoop(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", "")
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "")
	t.Setenv("OTEL_SERVICE_NAME", "")

	shutdown, err := ConfigureTracing(context.Background())
	if err != nil {
		t.Fatalf("configure tracing: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown tracing: %v", err)
	}
}

func TestTraceServiceNamePrefersEnvironment(t *testing.T) {
	t.Setenv("OTEL_SERVICE_NAME", "conduit-test")
	if got := traceServiceName(); got != "conduit-test" {
		t.Fatalf("expected OTEL service name from environment, got %q", got)
	}
}

func TestTraceExporterInsecureRecognizesHTTPAndEnvOverride(t *testing.T) {
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "")
	if !traceExporterInsecure("http://tempo.example:4318") {
		t.Fatal("expected http endpoint to force insecure OTLP")
	}
	t.Setenv("OTEL_EXPORTER_OTLP_INSECURE", "false")
	if traceExporterInsecure("http://tempo.example:4318") {
		t.Fatal("expected explicit OTEL_EXPORTER_OTLP_INSECURE=false to disable insecure mode")
	}
}
