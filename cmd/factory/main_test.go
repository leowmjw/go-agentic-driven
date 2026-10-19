package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/leowmjw/go-agentic-driven/internal/gate"
)

func testController(t *testing.T) (*controller, *int) {
	t.Helper()
	stopped := 0
	return &controller{
		dir: t.TempDir(),
		now: time.Now,
		start: func(release) (*running, error) {
			return &running{stop: func() { stopped++ }}, nil
		},
	}, &stopped
}

func TestDeploymentLifecycle(t *testing.T) {
	c, stopped := testController(t)
	v1 := &release{Version: "v1", Binary: "/v1/server"}
	v2 := &release{Version: "v2", Binary: "/v2/server"}
	if err := c.change("preview", v1, false); err != nil {
		t.Fatal(err)
	}
	if c.change("promote", nil, false) == nil {
		t.Fatal("initial activation must require explicit approval")
	}
	if err := c.change("promote", nil, true); err != nil {
		t.Fatal(err)
	}
	if c.change("preview", v1, false) == nil {
		t.Fatal("same-version canary has no meaning")
	}
	if err := c.change("preview", v2, false); err != nil {
		t.Fatal(err)
	}
	if c.change("promote", nil, true) == nil || c.change("promote", nil, false) == nil {
		t.Fatal("bootstrap cannot bypass an existing deployment's gate")
	}
	c.state.GatedAt = time.Now()
	if err := c.change("promote", nil, false); err != nil {
		t.Fatal(err)
	}
	if c.state.Active.Version != "v2" || c.state.Previous.Version != "v1" || *stopped != 1 {
		t.Fatalf("unexpected promotion state: %+v, stopped=%d", c.state, *stopped)
	}
	if err := c.change("rollback", nil, false); err != nil {
		t.Fatal(err)
	}
	if c.state.Active.Version != "v1" || c.state.Previous.Version != "v2" || *stopped != 2 {
		t.Fatalf("unexpected rollback state: %+v, stopped=%d", c.state, *stopped)
	}
	if err := c.change("close", nil, false); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(c.dir, "state.json"))
	if err != nil || !strings.Contains(string(b), `"version": "v1"`) {
		t.Fatalf("state not persisted: %s: %v", b, err)
	}
}

func TestGateApprovalExpiresWithControlledTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, _ := testController(t)
		c.state.Active = &release{Version: "old"}
		if err := c.change("preview", &release{Version: "new"}, false); err != nil {
			t.Fatal(err)
		}
		c.state.GatedAt = time.Now()
		time.Sleep(5*time.Minute + time.Nanosecond)
		if c.change("promote", nil, false) == nil {
			t.Fatal("stale approval allowed promotion")
		}
		if c.state.Active.Version != "old" {
			t.Fatal("failed promotion changed active version")
		}
	})
}

func TestFailedReadinessAndPersistenceKeepActive(t *testing.T) {
	c, stopped := testController(t)
	c.state.Active = &release{Version: "old"}
	c.start = func(release) (*running, error) { return nil, errors.New("not ready") }
	if c.change("preview", &release{Version: "bad"}, false) == nil || c.state.Preview != nil {
		t.Fatal("failed process was installed")
	}
	c.start = func(release) (*running, error) {
		return &running{stop: func() { *stopped++ }}, nil
	}
	c.dir = filepath.Join(c.dir, "does-not-exist")
	if c.change("preview", &release{Version: "new"}, false) == nil || *stopped != 1 {
		t.Fatal("failed save should stop new process")
	}
	if c.state.Active.Version != "old" || c.state.Preview != nil {
		t.Fatal("failed save mutated state")
	}
}

func TestSnapshotImmutableAndChecksummed(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "input")
	if err := os.WriteFile(source, []byte("binary one"), 0600); err != nil {
		t.Fatal(err)
	}
	first, err := snapshot(dir, source)
	if err != nil {
		t.Fatal(err)
	}
	again, err := snapshot(dir, source)
	if err != nil || first.Version != again.Version || first.Binary != again.Binary {
		t.Fatalf("snapshot isn't stable: %v", err)
	}
	if err := os.WriteFile(first.Binary, []byte("tampered"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := snapshot(dir, source); err == nil {
		t.Fatal("corrupted content-addressed release accepted")
	}
}

func TestEmptySlotsAreUnavailable(t *testing.T) {
	c, _ := testController(t)
	for _, preview := range []bool{false, true} {
		w := httptest.NewRecorder()
		c.proxy(preview).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("empty slot returned %d", w.Code)
		}
	}
}

func TestScenarioToolsRejectRemoteServers(t *testing.T) {
	for _, url := range []string{"https://example.com", "http://example.com", "file:///tmp/input", "http://user@localhost:8080"} {
		if _, err := demoClient(url); err == nil {
			t.Fatalf("accepted remote or unsafe URL: %s", url)
		}
	}
}

func TestGateRejectClosesOnlyPreviewAndHoldBlocksPromotion(t *testing.T) {
	for _, decision := range []string{"hold", "rollback"} {
		t.Run(decision, func(t *testing.T) {
			c, stopped := testController(t)
			c.state.Active = &release{Version: "old"}
			if err := c.change("preview", &release{Version: "new"}, false); err != nil {
				t.Fatal(err)
			}
			c.state.GatedAt = time.Now()
			c.assess = func(context.Context, string, string, string, int) (gate.Result, error) {
				return gate.Result{Decision: decision}, nil
			}
			if _, err := c.evaluate(context.Background(), "http://localhost"); err != nil {
				t.Fatal(err)
			}
			if !c.state.GatedAt.IsZero() || c.state.Active.Version != "old" {
				t.Fatal("failed gate retained approval or mutated active")
			}
			if decision == "rollback" && (c.state.Preview != nil || *stopped != 1) {
				t.Fatal("rejected preview not closed")
			}
			if c.change("promote", nil, false) == nil {
				t.Fatal("non-passing gate allowed promotion")
			}
		})
	}
}

func TestInFlightGateCannotApproveReplacedPreview(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c, _ := testController(t)
		c.state.Active = &release{Version: "old"}
		candidate := &release{Version: "new"}
		if err := c.change("preview", candidate, false); err != nil {
			t.Fatal(err)
		}
		started, finish := make(chan struct{}), make(chan struct{})
		c.assess = func(context.Context, string, string, string, int) (gate.Result, error) {
			close(started)
			<-finish
			return gate.Result{Decision: "pass"}, nil
		}
		errs := make(chan error, 1)
		go func() {
			_, err := c.evaluate(context.Background(), "http://localhost")
			errs <- err
		}()
		<-started
		// Even a same-version replacement is a new instance with empty storage.
		if err := c.change("preview", candidate, false); err != nil {
			t.Fatal(err)
		}
		close(finish)
		if <-errs == nil || !c.state.GatedAt.IsZero() {
			t.Fatal("gate approved a different running instance")
		}
	})
}

func TestDiagnoseExtractsBoundedEvidence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/select/logsql/query" || r.Method != http.MethodPost {
			t.Error("unexpected diagnostic endpoint")
		}
		if err := r.ParseForm(); err != nil || r.Form.Get("limit") != "201" ||
			!strings.Contains(r.Form.Get("query"), "_time:1h") ||
			!strings.Contains(r.Form.Get("query"), `event.outcome:"failure"`) ||
			!strings.Contains(r.Form.Get("query"), `finance.scenario:"stale_quote"`) {
			t.Error("unbounded diagnostic query")
		}
		for range 201 {
			_, _ = w.Write([]byte("{\"outcome\":\"error\"}\n"))
		}
	}))
	defer server.Close()
	var out bytes.Buffer
	if err := diagnose(context.Background(), server.URL, &out); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Events    []map[string]any `json:"events"`
		Truncated bool             `json:"truncated"`
		Guide     string           `json:"guide"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil || len(result.Events) != 200 || !result.Truncated || result.Guide == "" {
		t.Fatalf("bad diagnostic output: %s, %v", out.String(), err)
	}
}

func TestDiagnoseRejectsFailedQueries(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	if diagnose(context.Background(), server.URL, &bytes.Buffer{}) == nil {
		t.Fatal("failed diagnostic query appeared successful")
	}
}

func TestCancelledStartupDoesNotLaunchRelease(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := startProcess(ctx, release{Binary: "does-not-exist"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("startup ignored cancellation: %v", err)
	}
}

func TestFinancialFailureScenariosRequireExpectedRejections(t *testing.T) {
	for _, reject := range []bool{true, false} {
		t.Run(map[bool]string{true: "guards-work", false: "guard-regression"}[reject], func(t *testing.T) {
			attempts := 0
			role := ""
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var input map[string]any
				if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
					t.Error(err)
				}
				if r.URL.Path == "/api/login" {
					role, _ = input["role"].(string)
					_, _ = w.Write([]byte(`{"role":"` + role + `"}`))
					return
				}
				attempts++
				if reject && attempts > 1 {
					status := http.StatusConflict
					if input["amount"] == float64(0) {
						status = http.StatusBadRequest
					}
					if role == "analyst" {
						status = http.StatusForbidden
					}
					w.WriteHeader(status)
				}
				_, _ = w.Write([]byte(`{"id":"probe"}`))
			}))
			defer server.Close()
			err := failureScenarios(server.URL)
			if reject && (err != nil || attempts != 4) {
				t.Fatalf("valid guards failed: attempts=%d err=%v", attempts, err)
			}
			if !reject && err == nil {
				t.Fatal("tool missed a broken payment guard")
			}
		})
	}
}
