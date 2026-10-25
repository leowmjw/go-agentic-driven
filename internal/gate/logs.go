package gate

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Evaluate retrieves both version cohorts and evaluates the release gate.
// A transport or decoding failure returns a hold result and a non-nil error.
// Insufficient or invalid observations return hold with reasons and no error.
// Callers must promote only when err == nil and Decision == "pass".
func Evaluate(ctx context.Context, logsURL, baselineVersion, canaryVersion string, minSamples int) (Result, error) {
	if baselineVersion == "" || canaryVersion == "" || baselineVersion == canaryVersion || minSamples < 1 {
		err := fmt.Errorf("distinct versions and positive min-samples are required")
		return Hold(err.Error()), err
	}
	baseline, err := Fetch(ctx, http.DefaultClient, logsURL, baselineVersion)
	if err != nil {
		err = fmt.Errorf("baseline: %w", err)
		return Hold(err.Error()), err
	}
	canary, err := Fetch(ctx, http.DefaultClient, logsURL, canaryVersion)
	if err != nil {
		err = fmt.Errorf("canary: %w", err)
		return Hold(err.Error()), err
	}
	return Compare(baseline, canary, baselineVersion, canaryVersion, minSamples), nil
}

func endpoint(base, path string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil {
		return "", fmt.Errorf("logs URL must be an http(s) URL without credentials")
	}
	u.Path = strings.TrimRight(u.Path, "/") + path
	u.RawQuery, u.Fragment = "", ""
	return u.String(), nil
}

func do(client *http.Client, req *http.Request) (*http.Response, error) {
	if client == nil {
		client = http.DefaultClient
	}
	bounded := *client
	// Redirecting POST to GET must not be mistaken for successful ingestion.
	bounded.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return bounded.Do(req)
}

// Fetch requests all payment attempts, not just successful checkout events.
func Fetch(ctx context.Context, client *http.Client, base, version string) ([]Event, error) {
	target, err := endpoint(base, "/select/logsql/query")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	query := `_time:1h AND service.version:` + strconv.Quote(version) +
		` AND (http.route:"/api/payments" OR http.route:"POST /api/payments")` +
		` AND (http.request.method:"POST" OR http.method:"POST")`
	form := url.Values{"query": {query}, "limit": {strconv.Itoa(MaxEvents + 1)}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := do(client, req)
	if err != nil {
		return nil, fmt.Errorf("query logs: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("query logs: HTTP %d", resp.StatusCode)
	}
	return Decode(resp.Body)
}

func WriteEvent(ctx context.Context, client *http.Client, base string, payload []byte) error {
	target, err := endpoint(base, "/insert/jsonline")
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(string(payload)+"\n"))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/stream+json")
	resp, err := do(client, req)
	if err != nil {
		return fmt.Errorf("write event: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("write event: HTTP %d", resp.StatusCode)
	}
	return nil
}
