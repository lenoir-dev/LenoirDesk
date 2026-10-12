package telemetry

import (
	"context"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutmetric"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

var metricsHandler http.Handler

func MetricsHandler() http.Handler { return metricsHandler }

func Setup() (func(context.Context) error, error) {
	otel.SetTextMapPropagator(propagation.TraceContext{})
	provider := sdktrace.NewTracerProvider()
	if endpoint := os.Getenv("RELAYDESK_OTLP_TRACES_URL"); endpoint != "" {
		exporter, err := otlptracehttp.New(context.Background(), otlptracehttp.WithEndpointURL(endpoint))
		if err != nil {
			return nil, err
		}
		provider = sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter))
	} else if os.Getenv("RELAYDESK_TRACE_STDOUT") == "true" {
		exporter, err := stdouttrace.New(stdouttrace.WithWriter(os.Stdout))
		if err != nil {
			return nil, err
		}
		provider = sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter))
	}
	otel.SetTracerProvider(provider)
	metrics := sdkmetric.NewMeterProvider()
	if os.Getenv("RELAYDESK_METRICS_ADDR") != "" {
		registry := prometheus.NewRegistry()
		exporter, err := otelprom.New(otelprom.WithRegisterer(registry))
		if err != nil {
			return nil, err
		}
		metrics = sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))
		metricsHandler = promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	} else if os.Getenv("RELAYDESK_TRACE_STDOUT") == "true" {
		metricExporter, err := stdoutmetric.New(stdoutmetric.WithWriter(os.Stdout))
		if err != nil {
			return nil, err
		}
		metrics = sdkmetric.NewMeterProvider(sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter, sdkmetric.WithInterval(30*time.Second))))
	}
	otel.SetMeterProvider(metrics)
	return func(ctx context.Context) error {
		if err := metrics.Shutdown(ctx); err != nil {
			return err
		}
		return provider.Shutdown(ctx)
	}, nil
}
