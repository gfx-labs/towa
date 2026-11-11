// Package telemetry builds the logger, tracer and meter the rest of the
// program uses, from the telemetry section of the config.
package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/metric"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"

	"github.com/gfx-labs/acpharness/src/config"
)

// Scope is the instrumentation scope name of acpharness's tracer, meter and
// exported logs.
const Scope = "github.com/gfx-labs/acpharness"

// Telemetry holds what the program logs, traces and measures with.
type Telemetry struct {
	Logger *slog.Logger
	Tracer trace.Tracer
	Meter  metric.Meter

	shutdown []func(context.Context) error
}

// Start builds the logger, tracer and meter. With cfg.OTel off the tracer
// and meter do nothing and no exporter, goroutine or timer is created.
//
// With it on, traces, metrics and logs are exported over OTLP/HTTP to the
// endpoint the standard OTEL_EXPORTER_OTLP_* variables name. Those variables
// and the other OTEL_* ones (batch sizes, export intervals, timeouts) are
// read by the OpenTelemetry SDK itself.
func Start(ctx context.Context, cfg config.Telemetry, version string, stderr io.Writer) (*Telemetry, error) {
	t := &Telemetry{
		Tracer: tracenoop.NewTracerProvider().Tracer(Scope),
		Meter:  metricnoop.NewMeterProvider().Meter(Scope),
	}
	var handlers []slog.Handler
	if cfg.Logs.Stderr.Enabled {
		opts := &slog.HandlerOptions{Level: cfg.Logs.Stderr.Level}
		var h slog.Handler
		switch cfg.Logs.Stderr.Format {
		case config.FormatText:
			h = slog.NewTextHandler(stderr, opts)
		case config.FormatJSON:
			h = slog.NewJSONHandler(stderr, opts)
		default:
			return nil, fmt.Errorf("telemetry: unknown stderr log format %q", cfg.Logs.Stderr.Format)
		}
		handlers = append(handlers, traceIDs{h})
	}

	if cfg.OTel {
		h, err := t.startOTel(ctx, cfg, version)
		if err != nil {
			return nil, errors.Join(fmt.Errorf("telemetry: %w", err), t.Shutdown(ctx))
		}
		if h != nil {
			handlers = append(handlers, h)
		}
	}

	switch len(handlers) {
	case 0:
		t.Logger = slog.New(slog.DiscardHandler)
	case 1:
		t.Logger = slog.New(handlers[0])
	default:
		t.Logger = slog.New(slog.NewMultiHandler(handlers...))
	}
	return t, nil
}

// startOTel creates the providers and returns the log handler that exports
// records, or nil when exported logs are off.
func (t *Telemetry) startOTel(ctx context.Context, cfg config.Telemetry, version string) (slog.Handler, error) {
	// Environment attributes (OTEL_RESOURCE_ATTRIBUTES, OTEL_SERVICE_NAME)
	// come last, so a host can rename the service.
	res, err := resource.New(ctx,
		resource.WithAttributes(
			attribute.String("service.name", "acpharness"),
			attribute.String("service.version", version),
		),
		resource.WithFromEnv(),
	)
	if err != nil {
		return nil, err
	}

	traces, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, err
	}
	tp := sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithBatcher(traces))
	t.shutdown = append(t.shutdown, tp.Shutdown)
	t.Tracer = tp.Tracer(Scope)

	metrics, err := otlpmetrichttp.New(ctx)
	if err != nil {
		return nil, err
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithResource(res), sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metrics)))
	t.shutdown = append(t.shutdown, mp.Shutdown)
	t.Meter = mp.Meter(Scope)

	if !cfg.Logs.OTel.Enabled {
		return nil, nil
	}
	logs, err := otlploghttp.New(ctx)
	if err != nil {
		return nil, err
	}
	lp := sdklog.NewLoggerProvider(sdklog.WithResource(res), sdklog.WithProcessor(sdklog.NewBatchProcessor(logs)))
	t.shutdown = append(t.shutdown, lp.Shutdown)
	return minLevel{
		level:   cfg.Logs.OTel.Level,
		Handler: otelslog.NewHandler(Scope, otelslog.WithLoggerProvider(lp)),
	}, nil
}

// Shutdown exports what is still buffered and stops the providers. How long
// it may take is set by ctx and by the SDK's export timeouts.
func (t *Telemetry) Shutdown(ctx context.Context) error {
	var errs []error
	for _, stop := range t.shutdown {
		errs = append(errs, stop(ctx))
	}
	t.shutdown = nil
	return errors.Join(errs...)
}

// WithRemoteParent returns ctx with the span that an ACP message's _meta
// names as its parent. ACP reserves the root _meta keys traceparent and
// tracestate for W3C trace context. Without a valid traceparent, ctx is
// returned unchanged.
func WithRemoteParent(ctx context.Context, meta map[string]any) context.Context {
	carrier := propagation.MapCarrier{}
	for _, key := range (propagation.TraceContext{}).Fields() {
		if value, ok := meta[key].(string); ok {
			carrier[key] = value
		}
	}
	return propagation.TraceContext{}.Extract(ctx, carrier)
}

// traceIDs adds the trace and span ids of the span in a record's context.
// The exported log gets them from the bridge. The ids are added as plain
// attributes, so they land inside a group opened with WithGroup.
type traceIDs struct{ slog.Handler }

func (h traceIDs) Handle(ctx context.Context, r slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		r.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	return h.Handler.Handle(ctx, r)
}

func (h traceIDs) WithAttrs(attrs []slog.Attr) slog.Handler {
	return traceIDs{h.Handler.WithAttrs(attrs)}
}

func (h traceIDs) WithGroup(name string) slog.Handler {
	return traceIDs{h.Handler.WithGroup(name)}
}

// minLevel drops records below level. The OTel bridge has no level option.
type minLevel struct {
	level slog.Level
	slog.Handler
}

func (h minLevel) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= h.level && h.Handler.Enabled(ctx, l)
}

func (h minLevel) WithAttrs(attrs []slog.Attr) slog.Handler {
	return minLevel{h.level, h.Handler.WithAttrs(attrs)}
}

func (h minLevel) WithGroup(name string) slog.Handler {
	return minLevel{h.level, h.Handler.WithGroup(name)}
}
