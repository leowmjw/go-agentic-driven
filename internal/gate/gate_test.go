package gate

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func fixture(version string, n int) []Event {
	events := make([]Event, n)
	for i := range events {
		events[i] = Event{
			"service.version": version, "http.route": "/api/payments", "http.request.method": "POST",
			"session.id": fmt.Sprint(i), "payment.provider": "card", "currency": "USD", "plan.tier": "standard",
			"checkout.completed": true, "payment.step.abandoned": false, "order.value": 10000,
			"db.queries": 2, "duration_ms": 100, "http.response.status_code": 201,
		}
	}
	return events
}

func TestCompare(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]Event, []Event)
		want   string
	}{
		{"healthy", func(b, c []Event) {}, "pass"},
		{"exact revenue threshold", func(b, c []Event) {
			for _, e := range c {
				e["order.value"] = 9800
			}
		}, "pass"},
		{"below revenue threshold", func(b, c []Event) {
			for _, e := range c {
				e["order.value"] = 9799
			}
		}, "rollback"},
		{"exact conversion threshold", func(b, c []Event) {
			for _, e := range c[:2] {
				e["checkout.completed"], e["order.value"] = false, 0
			}
		}, "pass"},
		{"conversion drop", func(b, c []Event) {
			for _, e := range c[:3] {
				e["checkout.completed"], e["order.value"] = false, 0
			}
		}, "rollback"},
		{"missing field", func(b, c []Event) { delete(c[0], "order.value") }, "hold"},
		{"user cohort mismatch", func(b, c []Event) {
			for _, event := range b {
				event["user.cohort"] = "new"
			}
			for _, event := range c {
				event["user.cohort"] = "returning"
			}
		}, "hold"},
		{"missing method", func(b, c []Event) { delete(c[0], "http.request.method") }, "hold"},
		{"missing version", func(b, c []Event) { delete(c[0], "service.version") }, "hold"},
		{"provider mismatch", func(b, c []Event) {
			for _, e := range c {
				e["payment.provider"] = "bank"
			}
		}, "hold"},
		{"currency mismatch", func(b, c []Event) {
			for _, e := range c {
				e["currency"] = "EUR"
			}
		}, "hold"},
		{"tier mismatch", func(b, c []Event) {
			for _, e := range c {
				e["plan.tier"] = "premium"
			}
		}, "hold"},
		{"query doubled", func(b, c []Event) {
			for _, e := range c {
				e["db.queries"] = 4
			}
		}, "rollback"},
		{"zero queries unchanged", func(b, c []Event) {
			for _, list := range [][]Event{b, c} {
				for _, e := range list {
					e["db.queries"] = 0
				}
			}
		}, "pass"},
		{"zero queries increased", func(b, c []Event) {
			for _, e := range b {
				e["db.queries"] = 0
			}
		}, "rollback"},
		{"zero business baseline", func(b, c []Event) {
			for _, list := range [][]Event{b, c} {
				for _, e := range list {
					e["checkout.completed"], e["order.value"] = false, 0
				}
			}
		}, "hold"},
		{"nan", func(b, c []Event) { c[0]["duration_ms"] = math.NaN() }, "hold"},
		{"infinity string", func(b, c []Event) { c[0]["order.value"] = "+Inf" }, "hold"},
		{"negative", func(b, c []Event) { c[0]["db.queries"] = -1 }, "hold"},
		{"fractional minor units", func(b, c []Event) { c[0]["order.value"] = 100.5 }, "hold"},
		{"stale quote", func(b, c []Event) {
			c[0]["checkout.completed"], c[0]["order.value"], c[0]["payment.step.abandoned"], c[0]["http.response.status_code"] = false, 0, true, 409
		}, "hold"},
		{"error regression", func(b, c []Event) {
			for _, e := range c[:2] {
				e["http.response.status_code"] = 500
			}
		}, "rollback"},
		{"exact error threshold", func(b, c []Event) { c[0]["http.response.status_code"] = 500 }, "pass"},
		{"latency regression", func(b, c []Event) {
			for _, e := range c {
				e["duration_ms"] = 151
			}
		}, "rollback"},
		{"exact latency absolute threshold", func(b, c []Event) {
			for _, e := range c {
				e["duration_ms"] = 150
			}
		}, "pass"},
		{"exact latency relative threshold", func(b, c []Event) {
			for _, e := range b {
				e["duration_ms"] = 1000
			}
			for _, e := range c {
				e["duration_ms"] = 1200
			}
		}, "pass"},
		{"duplicate completion", func(b, c []Event) { c[1]["session.id"] = c[0]["session.id"] }, "hold"},
		{"ignored UI", func(b, c []Event) { c[0]["http.route"] = "/ui/payment" }, "pass"},
		{"zero revenue", func(b, c []Event) {
			for _, e := range b {
				e["order.value"] = 0
			}
		}, "hold"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			b, c := fixture("stable", 100), fixture("candidate", 100)
			test.mutate(b, c)
			got := Compare(b, c, "stable", "candidate", 30)
			if got.Decision != test.want {
				t.Fatalf("got %+v; want %s", got, test.want)
			}
			if _, err := json.Marshal(got); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestInsufficientAndMissing(t *testing.T) {
	for _, n := range []int{0, 1, 29} {
		if got := Compare(fixture("stable", n), fixture("candidate", n), "stable", "candidate", 30); got.Decision != "hold" {
			t.Fatalf("n=%d: %+v", n, got)
		}
	}
	if got := Compare(fixture("stable", 30), fixture("candidate", 30), "stable", "candidate", 30); got.Decision != "pass" {
		t.Fatalf("exact min: %+v", got)
	}
	if got := Compare(fixture("stable", 30), fixture("candidate", 30), "stable", "candidate", 0); got.Decision != "hold" {
		t.Fatal(got)
	}
}

func TestMixWeightsDoNotHideRegression(t *testing.T) {
	b, c := fixture("stable", 100), fixture("candidate", 100)
	bankB, bankC := fixture("stable", 100), fixture("candidate", 300)
	for _, e := range bankB {
		e["payment.provider"], e["order.value"] = "bank", 100000
	}
	for _, e := range bankC {
		e["payment.provider"], e["order.value"] = "bank", 100000
	}
	for _, e := range c {
		e["order.value"] = 9000
	}
	result := Compare(append(b, bankB...), append(c, bankC...), "stable", "candidate", 30)
	if result.Decision != "rollback" || len(result.Cohorts) != 2 {
		t.Fatal(result)
	}
}

func TestDecode(t *testing.T) {
	for _, data := range []string{"null\n", "{} {}\n", "{\"duration_ms\":NaN}\n", strings.Repeat("x", 1<<20+1)} {
		if _, err := Decode(strings.NewReader(data)); err == nil {
			t.Fatalf("accepted invalid input")
		}
	}
	if _, err := Decode(strings.NewReader(strings.Repeat("{}\n", MaxEvents+1))); err == nil {
		t.Fatal("accepted truncated observations")
	}
	if _, err := Decode(strings.NewReader(strings.Repeat(strings.Repeat(" ", 1023)+"\n", MaxResponseBytes/1024+1))); err == nil {
		t.Fatal("accepted oversized response")
	}
	// Numbers and booleans are strings in VictoriaLogs query responses.
	data, _ := json.Marshal(fixture("stable", 1)[0])
	events, err := Decode(strings.NewReader(string(data) + "\n"))
	if err != nil {
		t.Fatal(err)
	}

	for k, v := range events[0] {
		switch value := v.(type) {
		case json.Number:
			events[0][k] = value.String()
		case bool:
			events[0][k] = fmt.Sprint(value)
		}
	}
	candidate := Event{}
	for k, v := range events[0] {
		candidate[k] = v
	}
	candidate["service.version"] = "candidate"
	if result := Compare(events, []Event{candidate}, "stable", "candidate", 1); result.Decision != "pass" {
		t.Fatal(result)
	}
}

func TestWideEventFixture(t *testing.T) {
	file, err := os.Open("testdata/healthy.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	events, err := Decode(file)
	if err != nil {
		t.Fatal(err)
	}
	if result := Compare(events, events, "stable", "candidate", 2); result.Decision != "pass" {
		t.Fatal(result)
	}
	for _, e := range events {
		if text(e, "service.version") == "candidate" && text(e, "http.route") == "/api/payments" {
			e["http.method"] = e["http.request.method"]
			delete(e, "http.request.method")
			e["http.route"] = "POST /api/payments"
			e["http.status"] = e["http.response.status_code"]
			delete(e, "http.response.status_code")
			e["db.count"] = e["db.queries"]
			delete(e, "db.queries")
			e["finance.currency"] = e["currency"]
			delete(e, "currency")
		}
	}
	if result := Compare(events, events, "stable", "candidate", 2); result.Decision != "pass" {
		t.Fatal(result)
	}
}

func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/select/logsql/query" || r.Method != "POST" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("limit") != "10001" {
			t.Error(r.Form)
		}
		q := r.Form.Get("query")
		for _, required := range []string{"_time:1h", `service.version:"stable"`, `http.route:"/api/payments"`, `http.request.method:"POST"`} {
			if !strings.Contains(q, required) {
				t.Errorf("missing %s in %s", required, q)
			}
		}
		_ = json.NewEncoder(w).Encode(fixture("stable", 1)[0])
	}))
	defer srv.Close()
	events, err := Fetch(context.Background(), srv.Client(), srv.URL, "stable")
	if err != nil || len(events) != 1 {
		t.Fatalf("%v, %v", events, err)
	}
}

func TestTransportFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer srv.Close()
	if _, err := Fetch(context.Background(), srv.Client(), srv.URL, "stable"); err == nil {
		t.Fatal("accepted HTTP failure")
	}
	if err := WriteEvent(context.Background(), srv.Client(), srv.URL, []byte("{}")); err == nil {
		t.Fatal("accepted ingest failure")
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if _, err := Fetch(ctx, srv.Client(), srv.URL, "stable"); err == nil {
		t.Fatal("accepted expired context")
	}
	for _, u := range []string{"file:///etc/passwd", "******host", "broken"} {
		if _, err := Fetch(context.Background(), srv.Client(), u, "stable"); err == nil {
			t.Fatal("accepted invalid URL")
		}
	}

}

func TestRedirectsAreNotSuccessfulIngestion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/landing" {
			t.Error("redirect followed")
			return
		}
		http.Redirect(w, r, "/landing", http.StatusFound)
	}))
	defer srv.Close()
	if err := WriteEvent(context.Background(), srv.Client(), srv.URL, []byte("{}")); err == nil {
		t.Fatal("redirect mistaken for ingestion")
	}
	if _, err := Fetch(context.Background(), srv.Client(), srv.URL, "stable"); err == nil {
		t.Fatal("redirect mistaken for telemetry")
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRequestsHaveFiveSecondBound(t *testing.T) {
	client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok || time.Until(deadline) > 5*time.Second || time.Until(deadline) < 4*time.Second {
			t.Fatal("missing five-second deadline")
		}
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: make(http.Header)}, nil
	})}
	if _, err := Fetch(context.Background(), client, "http://logs.example", "stable"); err != nil {
		t.Fatal(err)
	}
	if err := WriteEvent(context.Background(), client, "http://logs.example", []byte("{}")); err != nil {
		t.Fatal(err)
	}
}
