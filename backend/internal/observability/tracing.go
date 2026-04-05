package observability

import (
	"context"
	"os"
	"strconv"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkresource "go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// ConfigureTracing enables OTLP/HTTP tracing when OTEL_EXPORTER_OTLP_ENDPOINT
// is configured. Without that environment variable it leaves the default noop
// tracer provider in place.
func ConfigureTracing(ctx context.Context) (func(context.Context) error, error) {
	endpoint := strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if endpoint == "" {
		return func(context.Context) error { return nil }, nil
	}

	options := []otlptracehttp.Option{}
	if strings.Contains(endpoint, "://") {
		options = append(options, otlptracehttp.WithEndpointURL(endpoint))
	} else {
		options = append(options, otlptracehttp.WithEndpoint(endpoint))
	}
	if traceExporterInsecure(endpoint) {
		options = append(options, otlptracehttp.WithInsecure())
	}

	exporter, err := otlptracehttp.New(ctx, options...)
	if err != nil {
		return nil, err
	}
	resource, err := sdkresource.New(ctx,
		sdkresource.WithAttributes(attribute.String("service.name", traceServiceName())),
	)
	if err != nil {
		return nil, err
	}

	provider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(resource),
	)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return provider.Shutdown, nil
}

func traceServiceName() string {
	if value := strings.TrimSpace(os.Getenv("OTEL_SERVICE_NAME")); value != "" {
		return value
	}
	return "conduit-gateway"
}

func traceExporterInsecure(endpoint string) bool {
	if value, ok := os.LookupEnv("OTEL_EXPORTER_OTLP_INSECURE"); ok {
		enabled, err := strconv.ParseBool(strings.TrimSpace(value))
		if err == nil {
			return enabled
		}
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(endpoint)), "http://")
}
