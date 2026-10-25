package gate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEvaluate(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   int
		samples  int
		regress  bool
		decision string
		wantErr  bool
	}{
		{"pass", 200, 30, false, "pass", false},
		{"missing", 200, 0, false, "hold", false},
		{"insufficient", 200, 29, false, "hold", false},
		{"regression", 200, 30, true, "rollback", false},
		{"transport", 503, 0, false, "hold", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Error(err)
				}
				if test.status != 200 {
					w.WriteHeader(test.status)
					return
				}
				version := "stable"
				if strings.Contains(r.Form.Get("query"), `service.version:"candidate"`) {
					version = "candidate"
				}
				events := fixture(version, test.samples)
				for _, event := range events {
					if test.regress && version == "candidate" {
						event["db.queries"] = 4
					}
					_ = json.NewEncoder(w).Encode(event)
				}
			}))
			defer server.Close()
			result, err := Evaluate(context.Background(), server.URL, "stable", "candidate", 30)
			if (err != nil) != test.wantErr || result.Decision != test.decision {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if _, err := json.Marshal(result); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestEvaluateInvalidArgumentsAndCancellation(t *testing.T) {
	for _, args := range []struct {
		baseline, canary string
		min              int
	}{
		{"", "candidate", 30}, {"stable", "", 30}, {"same", "same", 30}, {"stable", "candidate", 0},
	} {
		result, err := Evaluate(context.Background(), "http://logs.example", args.baseline, args.canary, args.min)
		if err == nil || result.Decision != "hold" {
			t.Fatalf("%+v %v", result, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := Evaluate(ctx, "http://logs.example", "stable", "candidate", 30)
	if err == nil || result.Decision != "hold" {
		t.Fatalf("%+v %v", result, err)
	}
}
