package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
)

type safeBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func TestVictoriaLogsExportAndTraceLink(t *testing.T) {
	var received bytes.Buffer
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/insert/jsonline" || r.URL.Query().Get("_stream_fields") != "service.name" || r.URL.Query().Get("_msg_field") != "message" || r.URL.Query().Get("_time_field") != "timestamp" {
			t.Error(r.URL.String())
		}
		_, _ = io.Copy(&received, r.Body)
		w.WriteHeader(200)
	}))
	defer server.Close()
	var output safeBuffer
	logger, shutdown, err := New(Config{VictoriaLogsURL: server.URL, Output: &output, Version: "abcdef123456"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, span := otel.Tracer("test").Start(context.Background(), "request")
	logger.InfoContext(ctx, "http.request", "finance.amount", 42)
	span.End()
	flushCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := shutdown(flushCtx); err != nil {
		t.Fatal(err)
	}
	if err := shutdown(flushCtx); err != nil {
		t.Fatal("repeated shutdown must be safe", err)
	}
	var event map[string]any
	if err := json.Unmarshal(received.Bytes(), &event); err != nil {
		t.Fatal(err, received.String())
	}
	for _, key := range []string{"timestamp", "service.name", "service.version", "message", "trace_id", "span_id", "finance.amount"} {
		if event[key] == nil {
			t.Error(key)
		}
		if event["service.version"] != "abcdef123456" {
			t.Fatal("configured resource version not exported", event)
		}
	}
	if !strings.Contains(output.String(), `"trace_id"`) {
		t.Fatal(output.String())
	}
}

func TestUnavailableTelemetryDoesNotBlockLogging(t *testing.T) {
	var output safeBuffer
	started := make(chan struct{}, 1)
	logger, shutdown, err := New(Config{VictoriaLogsURL: "http://localhost:9428", Output: &output, Do: func(r *http.Request) (*http.Response, error) {
		started <- struct{}{}
		<-r.Context().Done()
		return nil, r.Context().Err()
	}})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	logger.InfoContext(context.Background(), "payment settled", "amount", 100)
	if time.Since(start) > time.Second {
		t.Fatal("logger blocked on exporter")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_ = shutdown(ctx)
	if !strings.Contains(output.String(), "payment settled") {
		t.Fatal("stdout fallback lost")
	}
}

func TestInvalidEndpoint(t *testing.T) {
	for _, endpoint := range []string{"file:///etc/passwd", "://", "http://", "ftp://localhost:9428"} {
		if _, _, err := New(Config{VictoriaLogsURL: endpoint, Output: io.Discard}); err == nil {
			t.Fatal("invalid endpoint accepted", endpoint)
		}
	}
}

func TestRedirectIsReportedAsDeliveryFailure(t *testing.T) {
	var redirected atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", target.URL)
		w.WriteHeader(http.StatusFound)
	}))
	defer source.Close()
	var output safeBuffer
	logger, shutdown, err := New(Config{VictoriaLogsURL: source.URL, Output: &output})
	if err != nil {
		t.Fatal(err)
	}
	logger.InfoContext(context.Background(), "payment settled")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	shutdownErr := shutdown(ctx)
	if redirected.Load() != 0 {
		t.Fatal("redirect was followed")
	}
	reported := strings.Contains(output.String(), "HTTP 302") ||
		shutdownErr != nil && strings.Contains(shutdownErr.Error(), "HTTP 302")
	if !reported || !strings.Contains(output.String(), "payment settled") {
		t.Fatal("delivery failure or stdout fallback missing", output.String())
	}
}
