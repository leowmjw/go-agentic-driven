// Package gate implements a conservative, cohort-matched telemetry release gate.
package gate

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

const MaxEvents = 10000
const MaxResponseBytes = 16 << 20

type Event map[string]any

type Metrics struct {
	Requests    int     `json:"requests"`
	Sessions    int     `json:"sessions"`
	Conversion  float64 `json:"conversion"`
	Revenue     float64 `json:"revenue_per_session_minor_units"`
	Abandonment float64 `json:"payment_abandonment"`
	Queries     float64 `json:"queries_per_request"`
	ErrorRate   float64 `json:"error_rate"`
	LatencyP95  float64 `json:"latency_p95_ms"`
}

type Cohort struct {
	Key      string  `json:"key"`
	Baseline Metrics `json:"baseline"`
	Canary   Metrics `json:"canary"`
}

type Result struct {
	Decision string   `json:"decision"`
	Reasons  []string `json:"reasons"`
	Cohorts  []Cohort `json:"cohorts"`
}

func Hold(reason string) Result {
	return Result{Decision: "hold", Reasons: []string{reason}, Cohorts: []Cohort{}}
}

// Decode accepts VictoriaLogs JSONL, retaining numbers without float coercion.
// A sentinel record beyond MaxEvents detects truncation instead of silently passing.
func Decode(r io.Reader) ([]Event, error) {
	limited := &io.LimitedReader{R: r, N: MaxResponseBytes + 1}
	scanner := bufio.NewScanner(limited)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	var events []Event
	for scanner.Scan() {
		if strings.TrimSpace(scanner.Text()) == "" {
			continue
		}
		var event Event
		d := json.NewDecoder(strings.NewReader(scanner.Text()))
		d.UseNumber()
		if err := d.Decode(&event); err != nil {
			return nil, fmt.Errorf("invalid JSONL: %w", err)
		}
		var extra any
		if err := d.Decode(&extra); err != io.EOF {
			return nil, fmt.Errorf("multiple values in JSONL record")
		}
		if event == nil {
			return nil, fmt.Errorf("null event")
		}
		events = append(events, event)
		if len(events) > MaxEvents {
			return nil, fmt.Errorf("event limit exceeded; narrow the observation window")
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read JSONL: %w", err)
	}
	if limited.N == 0 {
		return nil, fmt.Errorf("response byte limit exceeded")
	}
	return events, nil
}

func field(e Event, key string) any {
	if v, ok := e[key]; ok {
		return v
	}
	var aliases []string
	switch key {
	case "http.request.method":
		aliases = []string{"http.method"}
	case "http.response.status_code":
		aliases = []string{"http.status", "http.status_code"}
	case "db.queries":
		aliases = []string{"db.count"}
	case "currency":
		aliases = []string{"order.currency", "finance.currency"}
	}
	for _, alias := range aliases {
		if v, ok := e[alias]; ok {
			return v
		}
	}
	return nil
}

func text(e Event, key string) string {
	s, _ := field(e, key).(string)
	return s
}

func number(e Event, key string) (float64, error) {
	var n float64
	var err error
	switch v := field(e, key).(type) {
	case json.Number:
		n, err = v.Float64()
	case string:
		n, err = strconv.ParseFloat(v, 64)
	case float64:
		n = v
	case int:
		n = float64(v)
	default:
		err = fmt.Errorf("missing or invalid %s", key)
	}
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < 0 {
		return 0, fmt.Errorf("missing or invalid %s", key)
	}
	return n, nil
}

func boolean(e Event, key string) (bool, error) {
	switch v := e[key].(type) {
	case bool:
		return v, nil
	case string:
		if v == "true" || v == "false" {
			return v == "true", nil
		}
	}
	return false, fmt.Errorf("missing or invalid %s", key)
}

type sample struct {
	session                 string
	completed, abandoned    bool
	value, queries, latency float64
	failed                  bool
}

func collect(events []Event, version string) (map[string][]sample, error) {
	groups := map[string][]sample{}
	for _, e := range events {
		route := strings.TrimPrefix(text(e, "http.route"), "POST ")
		if route == "/api/payments" && (text(e, "service.version") == "" || text(e, "http.request.method") == "") {
			return nil, fmt.Errorf("payment request missing service.version or HTTP method")
		}
		if text(e, "service.version") != version || route != "/api/payments" || text(e, "http.request.method") != "POST" {
			continue
		}
		keys := []string{"payment.provider", "currency", "plan.tier"}
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			if text(e, key) == "" {
				return nil, fmt.Errorf("%s: missing cohort field %s", version, key)
			}
			parts = append(parts, text(e, key))
		}
		for _, key := range []string{"user.role", "region", "session.mode", "user.cohort"} {
			parts = append(parts, text(e, key))
		}
		keyBytes, _ := json.Marshal(parts)
		s := sample{session: text(e, "session.id")}
		if s.session == "" {
			return nil, fmt.Errorf("%s: missing session.id", version)
		}
		var err error
		if s.completed, err = boolean(e, "checkout.completed"); err != nil {
			return nil, err
		}
		if s.abandoned, err = boolean(e, "payment.step.abandoned"); err != nil {
			return nil, err
		}
		if s.value, err = number(e, "order.value"); err != nil {
			return nil, err
		}
		if s.value > 9007199254740991 || math.Trunc(s.value) != s.value || (!s.completed && s.value != 0) {
			return nil, fmt.Errorf("invalid minor-unit order.value")
		}
		if s.completed && s.abandoned {
			return nil, fmt.Errorf("completed and abandoned payment")
		}
		if _, ok := e["cache.hit"]; ok {
			cached, err := boolean(e, "cache.hit")
			if err != nil {
				return nil, err
			}
			if cached {
				s.completed, s.value = false, 0
			}
		}
		if s.queries, err = number(e, "db.queries"); err != nil {
			return nil, err
		}
		if math.Trunc(s.queries) != s.queries {
			return nil, fmt.Errorf("invalid db.queries")
		}
		if s.latency, err = number(e, "duration_ms"); err != nil {
			return nil, err
		}
		status, err := number(e, "http.response.status_code")
		if err != nil || status < 100 || status > 599 || math.Trunc(status) != status {
			return nil, fmt.Errorf("missing or invalid http.response.status_code")
		}
		s.failed = status >= 500
		groups[string(keyBytes)] = append(groups[string(keyBytes)], s)
	}
	return groups, nil
}

func summarize(samples []sample) (Metrics, error) {
	m := Metrics{Requests: len(samples)}
	sessions, completed, abandoned := map[string]bool{}, map[string]bool{}, map[string]bool{}
	latencies := make([]float64, 0, len(samples))
	revenue, queries, errors := 0.0, 0.0, 0.0
	for _, s := range samples {
		sessions[s.session] = true
		if s.completed {
			// Replayed successes must not inflate session conversion or revenue.
			if completed[s.session] {
				return m, fmt.Errorf("duplicate completed session; ambiguous revenue")
			}
			completed[s.session] = true
			revenue += s.value
		}
		if s.abandoned {
			abandoned[s.session] = true
		}
		queries += s.queries
		if s.failed {
			errors++
		}
		latencies = append(latencies, s.latency)
	}
	m.Sessions = len(sessions)
	m.Conversion = float64(len(completed)) / float64(m.Sessions)
	m.Revenue = revenue / float64(m.Sessions)
	m.Abandonment = float64(len(abandoned)) / float64(m.Sessions)
	m.Queries = queries / float64(m.Requests)
	m.ErrorRate = errors / float64(m.Requests)
	sort.Float64s(latencies)
	m.LatencyP95 = latencies[int(math.Ceil(.95*float64(len(latencies))))-1]
	for _, n := range []float64{m.Revenue, m.Queries, m.LatencyP95} {
		if math.IsInf(n, 0) || math.IsNaN(n) {
			return m, fmt.Errorf("nonfinite aggregate")
		}
	}
	return m, nil
}

// Compare requires sufficient sessions in every provider/currency/plan cohort.
// Mixing weights never affect decisions: every matched cohort must be healthy.
func Compare(baseline, canary []Event, baselineVersion, canaryVersion string, minSamples int) Result {
	if minSamples < 1 || baselineVersion == "" || canaryVersion == "" || baselineVersion == canaryVersion {
		return Hold("distinct versions and positive min-samples are required")
	}
	b, err := collect(baseline, baselineVersion)
	if err != nil {
		return Hold(err.Error())
	}
	c, err := collect(canary, canaryVersion)
	if err != nil {
		return Hold(err.Error())
	}
	if len(b) == 0 || len(c) == 0 {
		return Hold("missing payment request observations")
	}
	r := Result{Decision: "pass", Reasons: []string{}, Cohorts: []Cohort{}}
	add := func(decision, reason string) {
		if decision == "rollback" || r.Decision == "pass" {
			r.Decision = decision
		}
		r.Reasons = append(r.Reasons, reason)
	}
	keys := map[string]bool{}
	for k := range b {
		keys[k] = true
	}
	for k := range c {
		keys[k] = true
	}
	ordered := make([]string, 0, len(keys))
	for k := range keys {
		ordered = append(ordered, k)
	}
	sort.Strings(ordered)
	for _, key := range ordered {
		if len(b[key]) == 0 || len(c[key]) == 0 {
			add("hold", key+": unmatched cohort")
			continue
		}
		bm, err := summarize(b[key])
		if err != nil {
			add("hold", key+": "+err.Error())
			continue
		}
		cm, err := summarize(c[key])
		if err != nil {
			add("hold", key+": "+err.Error())
			continue
		}
		r.Cohorts = append(r.Cohorts, Cohort{key, bm, cm})
		if bm.Sessions < minSamples || cm.Sessions < minSamples {
			add("hold", key+": insufficient distinct sessions")
			continue
		}
		if bm.Conversion == 0 || bm.Revenue == 0 {
			add("hold", key+": zero baseline conversion or revenue")
		}
		if bm.Conversion > 0 && cm.Conversion < bm.Conversion*.98-1e-12 {
			add("rollback", key+": conversion dropped more than 2%")
		}
		if bm.Revenue > 0 && cm.Revenue < bm.Revenue*.98-1e-9 {
			add("rollback", key+": revenue per session dropped more than 2%")
		}
		if cm.Abandonment > bm.Abandonment+1e-12 {
			add("hold", key+": payment-step abandonment increased")
		}
		if (bm.Queries > 0 && cm.Queries >= bm.Queries*2) || (bm.Queries == 0 && cm.Queries > 0) {
			add("rollback", key+": queries per request doubled or increased from zero")
		}
		if cm.ErrorRate > bm.ErrorRate+.01+1e-12 {
			add("rollback", key+": server error rate increased more than 1 percentage point")
		}
		if cm.LatencyP95 > bm.LatencyP95*1.2 && cm.LatencyP95 > bm.LatencyP95+50 {
			add("rollback", key+": p95 latency increased more than 20% and 50ms")
		}
	}
	return r
}
