package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/leowmjw/go-agentic-driven/internal/web"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

const Version = "1.0.0"

type Deps struct {
	Now     func() time.Time
	Delay   func(context.Context, time.Duration) error
	ID      func() string
	Logger  *slog.Logger
	Region  string
	Version string
}

type App struct {
	deps    Deps
	store   store
	handler http.Handler
}

func New(d Deps) *App {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Delay == nil {
		d.Delay = delay
	}
	if d.ID == nil {
		d.ID = randomID
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Region == "" {
		d.Region = "local"
	}
	if d.Version == "" {
		d.Version = Version
	}
	a := &App{deps: d, store: store{payments: []Payment{}, keys: make(map[string]entry)}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /api/session", a.session)
	mux.HandleFunc("POST /api/login", a.login)
	mux.HandleFunc("POST /api/logout", a.logout)
	mux.HandleFunc("GET /api/payments", a.payments)
	mux.HandleFunc("POST /api/payments", a.createPayment)
	mux.HandleFunc("GET /api/contract", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/schema+json")
		_, _ = w.Write(Contract())
	})
	mux.HandleFunc("POST /api/events", a.events)
	mux.HandleFunc("POST /api/seed", a.seed)
	mux.HandleFunc("GET /admin", a.admin)
	mux.HandleFunc("GET /admin/events", a.adminEvents)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) { fail(w, 404, "not_found", "API route not found") })
	mux.Handle("/", web.Handler())
	a.handler = a.observe(a.guard(mux))
	return a
}

func (a *App) ServeHTTP(w http.ResponseWriter, r *http.Request) { a.handler.ServeHTTP(w, r) }

func randomID() string {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(data[:])
}

func role(r *http.Request) string {
	c, err := r.Cookie("demo_role")
	if err == nil && slices.Contains(Roles, c.Value) {
		return c.Value
	}
	return "customer"
}

func cohort(r *http.Request) string {
	if cookie, err := r.Cookie("demo_cohort"); err == nil && cookie.Value == "returning" {
		return "returning"
	}
	return "new"
}

func jsonResponse(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func fail(w http.ResponseWriter, status int, kind, message string) {
	if observed, ok := w.(*responseWriter); ok {
		observed.errorKind = kind
	}
	jsonResponse(w, status, map[string]any{"error": message, "kind": kind})
}

func decode(w http.ResponseWriter, r *http.Request, value any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(value); err != nil {
		return err
	}
	if err := dec.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("expected one JSON object")
	}
	return nil
}

func (a *App) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		if strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/admin") {
			w.Header().Set("Cache-Control", "no-store")
		}
		if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" {
			if site := r.Header.Get("Sec-Fetch-Site"); site != "" && site != "same-origin" && site != "none" {
				fail(w, 403, "csrf", "same-origin requests required")
				return
			}
			if origin := r.Header.Get("Origin"); origin != "" {
				u, err := url.Parse(origin)
				scheme := "http"
				if r.TLS != nil {
					scheme = "https"
				}
				if err != nil || u.Scheme != scheme || u.Host != r.Host || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
					fail(w, 403, "csrf", "origin does not match")
					return
				}
			}
			media, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if media != "application/json" {
				fail(w, 415, "content_type", "application/json required")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) session(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, 200, map[string]string{"role": role(r)})
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Role string `json:"role"`
	}
	if decode(w, r, &input) != nil || !slices.Contains(Roles, input.Role) {
		fail(w, 400, "validation", "role must be customer, analyst or admin")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: "demo_role", Value: input.Role, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: 86400})
	jsonResponse(w, 200, map[string]string{"role": input.Role})
}

func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: "demo_role", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: -1})
	jsonResponse(w, 200, map[string]string{"role": "customer"})
}

func (a *App) payments(w http.ResponseWriter, r *http.Request) {
	start := a.deps.Now()
	payments, balance, balances := a.store.snapshotAll("USD")
	if e := eventFrom(r); e != nil {
		e.DBCount++
		e.DBTime += a.deps.Now().Sub(start)
		e.PaymentCount = len(payments)
		e.Balance = balance
		e.Currency = "USD"
	}
	jsonResponse(w, 200, map[string]any{"payments": payments, "balance": balance, "balances": balances})
}

func (a *App) createPayment(w http.ResponseWriter, r *http.Request) {
	if role(r) == "analyst" {
		fail(w, 403, "forbidden", "analysts have read-only access")
		return
	}
	var in PaymentInput
	if decode(w, r, &in) != nil || in.Amount <= 0 || in.Amount > MaxSafeInteger ||
		!slices.Contains(Currencies, in.Currency) || !slices.Contains(Providers, in.Provider) ||
		!slices.Contains(Scenarios, in.Scenario) || strings.TrimSpace(in.IdempotencyKey) == "" || len(in.IdempotencyKey) > 128 {
		fail(w, 400, "validation", "invalid payment input")
		return
	}
	if e := eventFrom(r); e != nil {
		e.Amount = in.Amount
		e.Currency = in.Currency
		e.Provider = in.Provider
		e.Scenario = in.Scenario
		e.PaymentCount = 1
		digest := sha256.Sum256([]byte(in.IdempotencyKey))
		e.SessionID = hex.EncodeToString(digest[:])
	}
	if in.Scenario == "slow" {
		if err := a.deps.Delay(r.Context(), 2*time.Second); err != nil {
			if e := eventFrom(r); e != nil {
				e.Abandoned = true
			}
			fail(w, 408, "canceled", "payment canceled before settlement")
			return
		}
	}
	start := a.deps.Now()
	p, cached, err := a.store.create(in, a.deps.Now(), a.deps.ID())
	if e := eventFrom(r); e != nil {
		e.DBCount++
		e.DBTime += a.deps.Now().Sub(start)
		e.CacheHit = cached
		e.Retry = cached
		e.PaymentStatus = p.Status
		e.Abandoned = p.Status == "expired"
	}
	if err != nil {
		fail(w, 409, "conflict", err.Error())
		return
	}
	status := 201
	if cached {
		status = 200
	}
	if !cached && p.Status == "settled" {
		http.SetCookie(w, &http.Cookie{Name: "demo_cohort", Value: "returning", Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil, MaxAge: 86400})
	}
	jsonResponse(w, status, p)
}

func (a *App) seed(w http.ResponseWriter, r *http.Request) {
	if role(r) != "admin" {
		fail(w, 403, "forbidden", "admin demo role required")
		return
	}
	var in struct {
		Scenario string `json:"scenario"`
	}
	if decode(w, r, &in) != nil || !slices.Contains([]string{"fresh", "busy", "incident"}, in.Scenario) {
		fail(w, 400, "validation", "scenario must be fresh, busy or incident")
		return
	}
	replacement := store{payments: []Payment{}, keys: make(map[string]entry)}
	if in.Scenario != "fresh" {
		for i := range 12 {
			scenario := "normal"
			if in.Scenario == "incident" {
				scenario = Scenarios[i%len(Scenarios)]
			}
			_, _, _ = replacement.create(PaymentInput{Amount: int64(1250 + i*325), Currency: "USD", Provider: Providers[i%2], Scenario: scenario, IdempotencyKey: fmt.Sprintf("seed-%d", i)}, a.deps.Now().Add(-time.Duration(12-i)*time.Minute), a.deps.ID())
		}
	}
	a.store.mu.Lock()
	a.store.payments, a.store.keys, a.store.balances = replacement.payments, replacement.keys, replacement.balances
	a.store.mu.Unlock()
	a.payments(w, r)
}

func (a *App) events(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name          string   `json:"name"`
		Source        string   `json:"source"`
		Action        string   `json:"action"`
		Outcome       string   `json:"outcome"`
		Scenario      string   `json:"scenario"`
		PaymentID     string   `json:"paymentId"`
		DurationMS    *float64 `json:"durationMs"`
		DurationSnake *float64 `json:"duration_ms"`
		Error         struct {
			Kind string `json:"kind"`
		} `json:"error"`
		Session struct {
			Step string `json:"step"`
		} `json:"session"`
		User struct {
			Cohort string `json:"cohort"`
		} `json:"user"`
		Client struct {
			AppVersion string `json:"app_version"`
		} `json:"client"`
	}
	if decode(w, r, &in) != nil {
		fail(w, 400, "validation", "invalid event fields")
		return
	}
	if in.Name == "" {
		in.Name = in.Action
	}
	if in.Source == "" {
		in.Source = "ui"
	}
	duration := 0.0
	if in.DurationMS != nil {
		duration = *in.DurationMS
	}
	if in.DurationSnake != nil {
		if in.DurationMS != nil && *in.DurationSnake != duration {
			fail(w, 400, "validation", "conflicting event durations")
			return
		}
		duration = *in.DurationSnake
	}
	if strings.TrimSpace(in.Name) == "" || !slices.Contains([]string{"ui", "agent"}, in.Source) ||
		duration < 0 || duration > 3600000 {
		fail(w, 400, "validation", "invalid event name, source or duration")
		return
	}
	for _, field := range []string{in.Name, in.Action, in.Outcome, in.Scenario, in.PaymentID, in.Error.Kind, in.Session.Step, in.User.Cohort, in.Client.AppVersion} {
		if len(field) > 80 {
			fail(w, 400, "validation", "event strings must not exceed 80 bytes")
			return
		}
	}
	a.deps.Logger.InfoContext(r.Context(), "client.event",
		"event.name", in.Name, "event.source", in.Source, "event.action", in.Action,
		"event.outcome", in.Outcome, "event.scenario", in.Scenario, "payment.id", in.PaymentID,
		"duration_ms", duration, "event.duration_ms", duration, "error.kind", in.Error.Kind,
		"session.step", in.Session.Step, "user.cohort", in.User.Cohort, "client.app_version", in.Client.AppVersion,
		"user.role", role(r), "service.name", "finance-ledger", "service.version", a.deps.Version)
	jsonResponse(w, 202, map[string]bool{"accepted": true})
}

type eventKey struct{}
type requestEvent struct {
	DBCount, PaymentCount                       int
	DBTime                                      time.Duration
	PayloadBytes                                int64
	Amount, Balance                             int64
	Currency, Provider, Scenario, PaymentStatus string
	SessionID                                   string
	CacheHit, Retry, Abandoned                  bool
}

type countedBody struct {
	io.ReadCloser
	event *requestEvent
}

func (b *countedBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.event.PayloadBytes += int64(n)
	return n, err
}

func eventFrom(r *http.Request) *requestEvent {
	e, _ := r.Context().Value(eventKey{}).(*requestEvent)
	return e
}

type responseWriter struct {
	http.ResponseWriter
	status    int
	bytes     int
	errorKind string
}

func (w *responseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}
func (w *responseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	n, err := w.ResponseWriter.Write(p)
	w.bytes += n
	return n, err
}
func (w *responseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
func (w *responseWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(200)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}

func (a *App) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := a.deps.Now()
		ctx, span := otel.Tracer("finance-ledger").Start(r.Context(), "http.request")
		defer span.End()
		e := &requestEvent{}
		r = r.WithContext(context.WithValue(ctx, eventKey{}, e))
		if r.Body != nil {
			r.Body = &countedBody{ReadCloser: r.Body, event: e}
		}
		rw := &responseWriter{ResponseWriter: w}
		requestID := randomID()
		w.Header().Set("X-Request-ID", requestID)
		defer func() {
			panicValue := recover()
			kind, outcome := rw.errorKind, "success"
			if panicValue != nil {
				kind = "panic"
				if rw.status == 0 {
					fail(rw, 500, "internal", "internal server error")
				}
			}
			if rw.status == 0 {
				rw.status = 200
			}
			if rw.status >= 400 || panicValue != nil {
				outcome = "error"
				if kind == "" {
					kind = http.StatusText(rw.status)
				}
				span.SetStatus(codes.Error, kind)
			}
			route := r.Pattern
			if route == "" && r.Method == http.MethodPost && r.URL.Path == "/api/payments" {
				route = "POST /api/payments"
			}
			if route == "" {
				route = "unmatched"
			}
			if _, patternPath, hasMethod := strings.Cut(route, " "); hasMethod {
				route = patternPath
			}
			payloadCount := 0
			if e.PayloadBytes > 0 {
				payloadCount = 1
			}
			completed := e.PaymentStatus == "settled" && rw.status >= 200 && rw.status < 300 && panicValue == nil && !e.CacheHit
			var orderValue int64
			if completed {
				orderValue = e.Amount
			}
			sessionID := e.SessionID
			if sessionID == "" {
				sessionID = requestID
			}
			retryCount := 0
			if e.Retry {
				retryCount = 1
			}
			sessionStep, abandonedAtStep := "", ""
			if route == "/api/payments" && r.Method == http.MethodPost {
				sessionStep = "pay"
				if e.Abandoned || r.Context().Err() != nil {
					abandonedAtStep = "pay"
				}
			}
			span.SetAttributes(attribute.String("http.route", route), attribute.Int("http.response.status_code", rw.status))
			a.deps.Logger.InfoContext(ctx, "http.request",
				"service.name", "finance-ledger", "service.version", a.deps.Version, "event.name", "http.request",
				"request.id", requestID, "http.method", r.Method, "http.route", route,
				"http.request.method", r.Method, "http.response.status_code", rw.status,
				"http.status", rw.status, "duration_ms", float64(a.deps.Now().Sub(start))/float64(time.Millisecond),
				"request.payload_bytes", e.PayloadBytes, "request.payload_count", payloadCount, "response.bytes", rw.bytes,
				"outcome", outcome, "error.kind", kind, "user.role", role(r), "user.id", "demo-"+role(r), "session.mode", "demo",
				"feature.flags", "demo_roles,scenario_payments", "region", a.deps.Region,
				"session.id", sessionID, "plan.tier", "demo", "checkout.completed", completed,
				"user.cohort", cohort(r), "session.step", sessionStep, "abandoned_at_step", abandonedAtStep,
				"shipping.quote_stale", e.Scenario == "stale_quote",
				"order.value", orderValue, "currency", e.Currency, "payment.provider", e.Provider,
				"payment.step.abandoned", e.Abandoned || r.Context().Err() != nil,
				"db.queries", e.DBCount, "http.status_code", rw.status,
				"retry", e.Retry, "retry.count", retryCount, "cache.hit", e.CacheHit, "db.count", e.DBCount, "db.duration_ms", float64(e.DBTime)/float64(time.Millisecond),
				"finance.amount", e.Amount, "finance.currency", e.Currency, "finance.provider", e.Provider,
				"finance.scenario", e.Scenario, "finance.status", e.PaymentStatus, "finance.balance", e.Balance, "payment.count", e.PaymentCount)
		}()
		next.ServeHTTP(rw, r)
	})
}
