package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGateCLI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		version := "stable"
		if strings.Contains(r.Form.Get("query"), `service.version:"candidate"`) {
			version = "candidate"
		}
		for i := 0; i < 30; i++ {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"service.version": version, "http.route": "/api/payments", "http.request.method": "POST",
				"session.id": fmt.Sprint(i), "payment.provider": "card", "currency": "USD", "plan.tier": "standard",
				"checkout.completed": true, "payment.step.abandoned": false, "order.value": 100,
				"db.queries": 2, "duration_ms": 100, "http.response.status_code": 201,
			})
		}
	}))
	defer server.Close()
	var out, stderr bytes.Buffer
	if code := run([]string{"-logs", server.URL, "-baseline", "stable", "-canary", "candidate"}, &out, &stderr); code != 0 {
		t.Fatalf("code %d out %s err %s", code, &out, &stderr)
	}
	var result map[string]any
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || result["decision"] != "pass" {
		t.Fatalf("%v %s", err, &out)
	}
}

func TestEventCLI(t *testing.T) {
	var writes int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writes++
		if r.URL.Path != "/insert/jsonline" || r.Method != "POST" {
			t.Error(r.URL)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload["event.name"] != "agent.task" {
			t.Error(payload)
		}
	}))
	defer server.Close()
	for _, test := range []struct {
		args []string
		want int
	}{
		{[]string{"-tokens", "10", "-retries", "1", "-tests-passed"}, 0},
		{[]string{"-tokens", "11", "-max-tokens", "10"}, 1},
		{[]string{"-retries", "4", "-max-retries", "3"}, 1},
		{[]string{"-tokens", "10", "-max-tokens", "10", "-retries", "3"}, 0},
	} {
		args := append([]string{"event", "-logs", server.URL, "-name", "implement", "-spec", "task-1"}, test.args...)
		var out, stderr bytes.Buffer
		if code := run(args, &out, &stderr); code != test.want {
			t.Fatalf("code=%d want=%d stderr=%s", code, test.want, &stderr)
		}
		if !json.Valid(out.Bytes()) {
			t.Fatalf("invalid JSON: %s", &out)
		}
	}
	if writes != 4 {
		t.Fatal(writes)
	}
}

func TestEventIngestionFailureAndInvalidUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer server.Close()
	var out, stderr bytes.Buffer
	if code := run([]string{"event", "-logs", server.URL, "-name", "task", "-spec", "issue"}, &out, &stderr); code != 2 {
		t.Fatal(code)
	}
	if !json.Valid(out.Bytes()) || stderr.Len() == 0 {
		t.Fatal("failure not reported")
	}
	out.Reset()
	if code := run([]string{"event", "-tokens", "-1"}, &out, &stderr); code != 2 || out.Len() != 0 {
		t.Fatal(code)
	}
}

func TestOfflineAndMissingData(t *testing.T) {
	// Keep test files within the project; no OS temporary directories.
	dir, err := os.MkdirTemp(".", ".sensor-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	file := filepath.Join(dir, "empty.jsonl")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{},
		{"-baseline", "stable", "-canary", "candidate", "-baseline-file", file},
		{"-baseline", "stable", "-canary", "candidate", "-baseline-file", file, "-canary-file", file},
		{"-baseline", "stable", "-canary", "stable"},
	} {
		var out, stderr bytes.Buffer
		if code := run(args, &out, &stderr); code != 2 {
			t.Fatal(code)
		}
		var result struct{ Decision string }
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Decision != "hold" {
			t.Fatalf("%v %s", err, &out)
		}
	}
}
