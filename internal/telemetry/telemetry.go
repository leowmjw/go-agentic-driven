// Package telemetry delivers bounded, best-effort logs; payment work never waits on HTTP.
package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/bridges/otelslog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

type Config struct {
	VictoriaLogsURL string
	Version         string
	Output          io.Writer
	// Do is injectable for transport tests; the default client has a two-second timeout.
	Do func(*http.Request) (*http.Response, error)
}

func New(c Config) (*slog.Logger, func(context.Context) error, error) {
	if c.Output == nil {
		c.Output = os.Stdout
	}
	if c.Version == "" {
		c.Version = "1.0.0"
	}
	stdout := slog.NewJSONHandler(c.Output, nil)
	fallback := slog.New(stdout)
	res := resource.NewWithAttributes("", attribute.String("service.name", "finance-ledger"), attribute.String("service.version", c.Version))
	tp := sdktrace.NewTracerProvider(sdktrace.WithResource(res), sdktrace.WithSampler(sdktrace.AlwaysSample()))
	var lp *sdklog.LoggerProvider
	var bridge slog.Handler
	if c.VictoriaLogsURL != "" {
		u, err := url.Parse(c.VictoriaLogsURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			_ = tp.Shutdown(context.Background())
			return nil, nil, fmt.Errorf("VICTORIALOGS_URL must be an http(s) URL without credentials")
		}
		u.Path = strings.TrimRight(u.Path, "/") + "/insert/jsonline"
		q := u.Query()
		q.Set("_stream_fields", "service.name")
		q.Set("_msg_field", "message")
		q.Set("_time_field", "timestamp")
		u.RawQuery = q.Encode()
		if c.Do == nil {
			c.Do = (&http.Client{
				Timeout: 2 * time.Second,
				CheckRedirect: func(*http.Request, []*http.Request) error {
					return http.ErrUseLastResponse
				},
			}).Do
		}
		exporter := &victoriaExporter{url: u.String(), do: c.Do}
		lp = sdklog.NewLoggerProvider(sdklog.WithResource(res),
			sdklog.WithProcessor(sdklog.NewBatchProcessor(exporter,
				sdklog.WithMaxQueueSize(512), sdklog.WithExportMaxBatchSize(64),
				sdklog.WithExportInterval(time.Second), sdklog.WithExportTimeout(2*time.Second))))
		bridge = otelslog.NewHandler("finance-ledger", otelslog.WithLoggerProvider(lp))
	}
	otel.SetTracerProvider(tp)
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		fallback.ErrorContext(context.Background(), "telemetry delivery failure; stdout retained; bounded queue may drop events", "error.kind", "telemetry", "error", err.Error())
	}))
	logger := slog.New(&fanout{stdout: stdout, remote: bridge})
	return logger, func(ctx context.Context) error {
		var err error
		if lp != nil {
			err = lp.Shutdown(ctx)
		}
		if traceErr := tp.Shutdown(ctx); err == nil {
			err = traceErr
		}
		return err
	}, nil
}

type victoriaExporter struct {
	url    string
	do     func(*http.Request) (*http.Response, error)
	closed atomic.Bool
}

func (e *victoriaExporter) Export(ctx context.Context, records []sdklog.Record) error {
	if e.closed.Load() {
		return sdklog.ErrExporterShutdown
	}
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	for _, rec := range records {
		fields := map[string]any{"timestamp": rec.Timestamp().UTC().Format(time.RFC3339Nano), "message": rec.Body().AsString(), "level": rec.SeverityText()}
		for _, kv := range rec.Resource().Attributes() {
			fields[string(kv.Key)] = kv.Value.AsInterface()
		}
		rec.WalkAttributes(func(kv attribute.KeyValue) bool { fields[string(kv.Key)] = kv.Value.AsInterface(); return true })
		if id := rec.TraceID(); id.IsValid() {
			fields["trace_id"] = id.String()
		}
		if id := rec.SpanID(); id.IsValid() {
			fields["span_id"] = id.String()
		}
		if err := enc.Encode(fields); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, "POST", e.url, &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/stream+json")
	resp, err := e.do(req)
	if err != nil {
		return fmt.Errorf("VictoriaLogs unavailable: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("VictoriaLogs returned HTTP %d", resp.StatusCode)
	}
	return nil
}
func (e *victoriaExporter) Shutdown(context.Context) error   { e.closed.Store(true); return nil }
func (e *victoriaExporter) ForceFlush(context.Context) error { return nil }

type fanout struct{ stdout, remote slog.Handler }

func (h *fanout) Enabled(ctx context.Context, level slog.Level) bool {
	return h.stdout.Enabled(ctx, level) || h.remote != nil && h.remote.Enabled(ctx, level)
}
func (h *fanout) Handle(ctx context.Context, rec slog.Record) error {
	if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
		rec.AddAttrs(slog.String("trace_id", sc.TraceID().String()), slog.String("span_id", sc.SpanID().String()))
	}
	err := h.stdout.Handle(ctx, rec.Clone())
	if h.remote != nil {
		if remoteErr := h.remote.Handle(ctx, rec); err == nil {
			err = remoteErr
		}
	}
	return err
}
func (h *fanout) WithAttrs(attrs []slog.Attr) slog.Handler {
	copy := &fanout{stdout: h.stdout.WithAttrs(attrs)}
	if h.remote != nil {
		copy.remote = h.remote.WithAttrs(attrs)
	}
	return copy
}
func (h *fanout) WithGroup(name string) slog.Handler {
	copy := &fanout{stdout: h.stdout.WithGroup(name)}
	if h.remote != nil {
		copy.remote = h.remote.WithGroup(name)
	}
	return copy
}
