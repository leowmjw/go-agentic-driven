package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/leowmjw/go-agentic-driven/internal/app"
	"github.com/leowmjw/go-agentic-driven/internal/telemetry"
)

func main() {
	if err := run(); err != nil {
		slog.ErrorContext(context.Background(), "server stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", "127.0.0.1:8080", "HTTP listen address (localhost by default)")
	contract := flag.Bool("contract", false, "print frontend JSON Schema contract and exit")
	flag.Parse()
	if *contract {
		_, err := fmt.Println(string(app.Contract()))
		return err
	}
	version := os.Getenv("SERVICE_VERSION")
	if version == "" {
		version = app.Version
	}
	logger, flush, err := telemetry.New(telemetry.Config{VictoriaLogsURL: os.Getenv("VICTORIALOGS_URL"), Version: version})
	if err != nil {
		return err
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := flush(ctx); err != nil {
			slog.ErrorContext(ctx, "telemetry shutdown failed; stdout retained", "error", err)
		}
	}()
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	requestCtx, cancelRequests := context.WithCancel(context.Background())
	defer cancelRequests()
	server := &http.Server{
		Addr: *addr, Handler: app.New(app.Deps{Logger: logger, Region: os.Getenv("REGION"), Version: version}),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 16 << 10,
		BaseContext:    func(net.Listener) context.Context { return requestCtx },
	}
	listener, err := net.Listen("tcp", *addr)
	if err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	logger.InfoContext(ctx, "server listening", "addr", listener.Addr().String(), "authentication", "demo only; never expose as production auth")
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		cancelRequests() // Close long-lived SSE before waiting for graceful drain.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return err
		}
		return nil
	}
}
