package telemetry

import (
	"context"
	"errors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.34.0"
	"go.opentelemetry.io/otel/trace"
	"strings"
)

func Setup(ctx context.Context, endpoint string) (func(context.Context) error, error) {
	if endpoint == "" {
		otel.SetTracerProvider(trace.NewNoopTracerProvider())
		return func(context.Context) error { return nil }, nil
	}
	opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(strings.TrimPrefix(strings.TrimPrefix(endpoint, "http://"), "https://"))}
	if strings.HasPrefix(endpoint, "http://") {
		opts = append(opts, otlptracehttp.WithInsecure())
	}
	exp, err := otlptracehttp.New(ctx, opts...)
	if err != nil {
		return nil, err
	}
	res, err := resource.Merge(resource.Default(), resource.NewWithAttributes(semconv.SchemaURL, semconv.ServiceName("orrery")))
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exp), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}
func ShutdownAll(ctx context.Context, fns ...func(context.Context) error) error {
	var es []error
	for _, f := range fns {
		if f != nil {
			es = append(es, f(ctx))
		}
	}
	return errors.Join(es...)
}
