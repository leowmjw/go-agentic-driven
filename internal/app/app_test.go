package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

func testApp() *App { return New(Deps{Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))}) }

func request(a *App, method, path, body, role string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if role != "" {
		r.AddCookie(&http.Cookie{Name: "demo_role", Value: role})
	}
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}

func input(key, scenario string, amount int64) string {
	return fmt.Sprintf(`{"amount":%d,"currency":"USD","provider":"bank","scenario":%q,"idempotencyKey":%q}`, amount, scenario, key)
}

func TestSessionAndPermissions(t *testing.T) {
	a := testApp()
	if w := request(a, "GET", "/api/session", "", ""); w.Code != 200 || w.Body.String() != "{\"role\":\"customer\"}\n" {
		t.Fatal(w.Code, w.Body.String())
	}

	for _, role := range Roles {
		w := request(a, "POST", "/api/login", fmt.Sprintf(`{"role":%q}`, role), "")
		if w.Code != 200 || len(w.Result().Cookies()) != 1 || !w.Result().Cookies()[0].HttpOnly {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := request(a, "POST", "/api/login", `{"role":"root"}`, ""); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := request(a, "POST", "/api/logout", `{}`, "admin"); w.Code != 200 || w.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal(w.Code)
	}
	if w := request(a, "POST", "/api/payments", input("x", "normal", 100), "analyst"); w.Code != 403 {
		t.Fatal(w.Code)
	}
	for _, path := range []string{"/admin", "/admin/events"} {
		if w := request(a, "GET", path, "", "customer"); w.Code != 403 {
			t.Fatal(path, w.Code)
		}
	}
	if w := request(a, "POST", "/api/seed", `{"scenario":"fresh"}`, "customer"); w.Code != 403 {
		t.Fatal(w.Code)
	}
}

func TestHealthAndConfiguredVersion(t *testing.T) {
	var logs bytes.Buffer
	a := New(Deps{Version: "a1b2c3d4e5f6", Region: "local", Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	w := request(a, "GET", "/healthz", "", "")
	if w.Code != http.StatusOK {
		t.Fatal(w.Code, w.Body.String())
	}
	var event map[string]any
	if json.Unmarshal(logs.Bytes(), &event) != nil || event["service.version"] != "a1b2c3d4e5f6" || event["region"] != "local" {
		t.Fatal(logs.String())
	}
	payments, balance := a.store.snapshot()
	if len(payments) != 0 || balance != 0 {
		t.Fatal("fresh store is not empty")
	}
}

func TestPaymentScenariosAndIdempotency(t *testing.T) {
	a := New(Deps{Delay: func(context.Context, time.Duration) error { return nil }, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))})
	expected := []string{"settled", "declined", "settled", "review", "expired"}
	for i, scenario := range Scenarios {
		body := input(scenario, scenario, 100)
		w := request(a, "POST", "/api/payments", body, "")
		var first Payment
		if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &first) != nil || first.Status != expected[i] || first.CreatedAt.IsZero() {
			t.Fatal(w.Code, w.Body.String())
		}
		w = request(a, "POST", "/api/payments", body, "")
		var again Payment
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &again) != nil || first != again {
			t.Fatal(w.Code, w.Body.String())
		}
		if w = request(a, "POST", "/api/payments", input(scenario, scenario, 101), ""); w.Code != 409 {
			t.Fatal(w.Code)
		}
	}
	w := request(a, "GET", "/api/payments", "", "")
	var snapshot struct {
		Payments []Payment
		Balance  int64
	}
	if json.Unmarshal(w.Body.Bytes(), &snapshot) != nil || len(snapshot.Payments) != 5 || snapshot.Balance != 200 {
		t.Fatal(w.Body.String())
	}
}

func TestPaymentValidation(t *testing.T) {
	a := testApp()
	for _, body := range []string{
		input("x", "normal", 0), input("x", "normal", -1), input("x", "normal", MaxSafeInteger+1),
		input("x", "bad", 10), input("", "normal", 10),
		`{"amount":1.5,"currency":"USD","provider":"bank","scenario":"normal","idempotencyKey":"x"}`,
		`{"amount":1,"currency":"JPY","provider":"bank","scenario":"normal","idempotencyKey":"x"}`,
		`{"amount":1,"currency":"USD","provider":"cash","scenario":"normal","idempotencyKey":"x"}`,
		input("x", "normal", 1) + ` {}`, `null`, `{}`, `{"extra":true}`,
		strings.Repeat(" ", 17000) + `{}`,
	} {
		if w := request(a, "POST", "/api/payments", body, ""); w.Code != 400 {
			t.Errorf("body %.100s: got %d", body, w.Code)
		}
	}
	if w := request(a, "POST", "/api/payments", input("max", "normal", MaxSafeInteger), ""); w.Code != 201 {
		t.Fatal(w.Code)
	}
	if w := request(a, "POST", "/api/payments", input("over", "normal", 1), ""); w.Code != 409 {
		t.Fatal(w.Code)
	}
}

func TestConcurrentIdempotency(t *testing.T) {
	a := testApp()
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if w := request(a, "POST", "/api/payments", input("shared", "normal", 99), ""); w.Code != 201 && w.Code != 200 {
				t.Error(w.Code)
			}
		})
	}
	wg.Wait()
	payments, balance := a.store.snapshot()
	if len(payments) != 1 || balance != 99 {
		t.Fatal(len(payments), balance)
	}
}

func TestCurrencyBalancesAndImmutableSnapshot(t *testing.T) {
	a := testApp()
	eur := strings.Replace(input("eur", "normal", MaxSafeInteger), `"USD"`, `"EUR"`, 1)
	for _, body := range []string{eur, input("usd", "normal", 100)} {
		if w := request(a, "POST", "/api/payments", body, ""); w.Code != 201 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	payments, euroBalance := a.store.snapshot("EUR")
	if euroBalance != MaxSafeInteger {
		t.Fatal(euroBalance)
	}
	payments[0].Amount = 0
	original, dollarBalance := a.store.snapshot()
	if original[0].Amount != MaxSafeInteger || dollarBalance != 100 {
		t.Fatal("snapshot mutation or cross-currency accounting", original, dollarBalance)
	}
	review := strings.Replace(input("eur-risk", "risk", 100), `"USD"`, `"EUR"`, 1)
	if w := request(a, "POST", "/api/payments", review, ""); w.Code != 201 {
		t.Fatal(w.Code)
	}
	_, _, copyOfBalances := a.store.snapshotAll("USD")
	copyOfBalances["USD"] = -1
	for _, query := range []string{"", "?currency=EUR", "?currency=GBP"} {
		w := request(a, "GET", "/api/payments"+query, "", "")
		var body struct {
			Balance  int64            `json:"balance"`
			Balances map[string]int64 `json:"balances"`
		}
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Balance != 100 ||
			body.Balances["USD"] != 100 || body.Balances["EUR"] != MaxSafeInteger || body.Balances["GBP"] != 0 {
			t.Fatal(query, w.Code, w.Body.String())
		}
	}
}

func TestSlowPaymentVirtualTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		a := testApp()
		start := time.Now()
		w := request(a, "POST", "/api/payments", input("slow", "slow", 100), "")
		if w.Code != 201 || time.Since(start) != 2*time.Second {
			t.Fatal(w.Code, time.Since(start))
		}
	})
}

func TestCanceledSlowPaymentDoesNotSettle(t *testing.T) {
	a := testApp()
	r := httptest.NewRequest("POST", "/api/payments", strings.NewReader(input("slow", "slow", 100)))
	r.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithCancel(r.Context())
	cancel()
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r.WithContext(ctx))
	p, balance := a.store.snapshot()
	if w.Code != 408 || len(p) != 0 || balance != 0 {
		t.Fatal(w.Code, balance)
	}
}

func TestMutationGuards(t *testing.T) {
	for _, test := range []struct {
		name, key, value string
		status           int
	}{
		{"foreign origin", "Origin", "https://evil.example", 403},
		{"null origin", "Origin", "null", 403},
		{"cross site", "Sec-Fetch-Site", "cross-site", 403},
		{"same site", "Sec-Fetch-Site", "same-site", 403},
		{"text", "Content-Type", "text/plain", 415},
		{"same origin", "Origin", "http://example.com", 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"role":"admin"}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set(test.key, test.value)
			w := httptest.NewRecorder()
			testApp().ServeHTTP(w, r)
			if w.Code != test.status {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
}

func TestAdminSeedAndDatastar(t *testing.T) {
	a := New(Deps{Delay: func(context.Context, time.Duration) error { return context.Canceled }, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil))})
	for _, scenario := range []string{"busy", "incident", "fresh"} {
		w := request(a, "POST", "/api/seed", fmt.Sprintf(`{"scenario":%q}`, scenario), "admin")
		if w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
		p, _ := a.store.snapshot()
		if (scenario == "fresh") != (len(p) == 0) {
			t.Fatal(scenario, len(p))
		}
	}
	w := request(a, "GET", "/admin", "", "admin")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `data-init="@get('/admin/events')"`) || !strings.Contains(w.Body.String(), `data-on-load="@get('/admin/events')"`) || !strings.Contains(w.Body.String(), "/datastar.js") {
		t.Fatal(w.Body.String())
	}
	w = request(a, "GET", "/admin/events", "", "admin")
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/event-stream" || !strings.Contains(w.Body.String(), "event: datastar-patch-elements\ndata: elements <section") {
		t.Fatal(w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "event: datastar-merge-fragments\ndata: fragments <section") {
		t.Fatal("npm beta compatibility event missing", w.Body.String())
	}
}

func TestContractAndClientEvents(t *testing.T) {
	a := testApp()
	w := request(a, "GET", "/api/contract", "", "")
	if !bytes.Equal(w.Body.Bytes(), Contract()) {
		t.Fatal("HTTP contract differs from generated contract")
	}
	var schema struct {
		Defs map[string]json.RawMessage `json:"$defs"`
	}
	if json.Unmarshal(Contract(), &schema) != nil {
		t.Fatal("invalid schema")
	}
	for _, name := range []string{"payment", "payments", "session", "paymentInput"} {
		if schema.Defs[name] == nil {
			t.Fatal(name)
		}
	}
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"name":"clicked","source":"ui","action":"payment.submit","durationMs":12}`, 202},
		{`{"name":"planned","source":"agent"}`, 202},
		{`{"name":"x","source":"bad"}`, 400},
		{`{"name":"x","source":"ui","secret":"must not be recorded"}`, 400},
		{`{"name":"x","source":"ui","durationMs":-1}`, 400},
	} {
		if w := request(a, "POST", "/api/events", tc.body, ""); w.Code != tc.status {
			t.Fatal(w.Code, w.Body.String())
		}
	}
}

func TestCanonicalQuoteAndCohortFields(t *testing.T) {
	var logs bytes.Buffer
	a := New(Deps{Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	first := request(a, "POST", "/api/payments", input("first", "normal", 100), "")
	if first.Code != 201 || len(first.Result().Cookies()) == 0 || first.Result().Cookies()[0].Name != "demo_cohort" {
		t.Fatal(first.Code, first.Header())
	}
	r := httptest.NewRequest("POST", "/api/payments", strings.NewReader(input("expired", "stale_quote", 100)))
	r.Header.Set("Content-Type", "application/json")
	r.AddCookie(first.Result().Cookies()[0])
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 201 {
		t.Fatal(w.Code)
	}
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if len(events) != 2 || events[0]["user.cohort"] != "new" || events[1]["user.cohort"] != "returning" {
		t.Fatal(events)
	}
	for key, expected := range map[string]any{
		"session.step": "pay", "abandoned_at_step": "pay", "shipping.quote_stale": true,
		"payment.step.abandoned": true, "finance.status": "expired", "checkout.completed": false, "order.value": float64(0),
	} {
		if events[1][key] != expected {
			t.Errorf("%s: got %v, want %v", key, events[1][key], expected)
		}
	}
}

func TestNestedFrontendEventPayload(t *testing.T) {
	var logs bytes.Buffer
	a := New(Deps{Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	body := `{"action":"payment.submit","outcome":"success","client":{"app_version":"preview"},"error":{"kind":""},"session":{"step":"confirm"},"user":{"cohort":"customer"},"duration_ms":12.5}`
	w := request(a, "POST", "/api/events", body, "")
	if w.Code != http.StatusAccepted {
		t.Fatal(w.Code, w.Body.String())
	}
	var event map[string]any
	first := strings.SplitN(logs.String(), "\n", 2)[0]
	if err := json.Unmarshal([]byte(first), &event); err != nil {
		t.Fatal(err)
	}
	for key, expected := range map[string]any{
		"event.name": "payment.submit", "event.source": "ui", "event.action": "payment.submit",
		"event.outcome": "success", "client.app_version": "preview", "error.kind": "",
		"session.step": "confirm", "user.cohort": "customer", "duration_ms": 12.5,
	} {
		if event[key] != expected {
			t.Errorf("%s: got %v, want %v", key, event[key], expected)
		}
	}
	for _, invalid := range []string{
		`{}`, `{"action":"x","client":{"unexpected":"secret"}}`,
		`{"action":"x","duration_ms":-1}`,
		`{"action":"x","duration_ms":1,"durationMs":2}`,
		`{"action":"x","session":{"step":"` + strings.Repeat("x", 81) + `"}}`,
	} {
		if rejected := request(a, "POST", "/api/events", invalid, ""); rejected.Code != 400 {
			t.Fatal(rejected.Code, rejected.Body.String())
		}
	}
}

func TestIdempotentReplayIsNotAnotherCompletion(t *testing.T) {
	var logs bytes.Buffer
	a := New(Deps{Logger: slog.New(slog.NewJSONHandler(&logs, nil))})
	for _, status := range []int{201, 200} {
		w := request(a, "POST", "/api/payments", input("same-checkout", "normal", 123), "")
		if w.Code != status {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	var events []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	if len(events) != 2 || events[0]["checkout.completed"] != true || events[0]["order.value"] != float64(123) ||
		events[1]["checkout.completed"] != false || events[1]["order.value"] != float64(0) ||
		events[1]["cache.hit"] != true || events[0]["session.id"] != events[1]["session.id"] ||
		events[0]["retry.count"] != float64(0) || events[1]["retry.count"] != float64(1) {
		t.Fatal(events)
	}
}

func TestOneWideEventAndPanicRecovery(t *testing.T) {
	for _, panicID := range []bool{false, true} {
		var logs bytes.Buffer
		d := Deps{Logger: slog.New(slog.NewJSONHandler(&logs, nil))}
		if panicID {
			d.ID = func() string { panic(errors.New("external failure")) }
		}

		a := New(d)
		w := request(a, "POST", "/api/payments", input("x", "normal", 1), "")
		expected := 201
		if panicID {
			expected = 500
		}
		if w.Code != expected {
			t.Fatal(w.Code)
		}
		lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
		if len(lines) != 1 {
			t.Fatal(logs.String())
		}
		var event map[string]any
		if json.Unmarshal([]byte(lines[0]), &event) != nil {
			t.Fatal(logs.String())
		}
		for _, key := range []string{"service.name", "service.version", "http.route", "http.request.method", "http.response.status_code", "duration_ms", "request.payload_bytes", "outcome", "error.kind", "user.role", "session.mode", "session.id", "feature.flags", "region", "retry", "cache.hit", "db.count", "db.duration_ms", "finance.amount", "checkout.completed", "order.value", "currency", "payment.provider", "plan.tier", "payment.step.abandoned", "db.queries", "http.status_code"} {
			if _, ok := event[key]; !ok {
				t.Error("missing wide event field", key)
			}
		}
		if event["checkout.completed"] != !panicID {
			t.Error("incorrect completion", event)
		}
		if event["http.route"] != "/api/payments" {
			t.Error("route must not include the HTTP method", event)
		}
		expectedValue := float64(1)
		if panicID {
			expectedValue = 0
		}
		if event["order.value"] != expectedValue {
			t.Error("incorrect order value", event)
		}
	}
}
