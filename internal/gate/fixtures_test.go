package gate

import (
	"os"
	"testing"
)

func TestReleaseJSONLFixtures(t *testing.T) {
	read := func(path string) []Event {
		t.Helper()
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		events, err := Decode(file)
		if err != nil {
			t.Fatal(err)
		}
		return events
	}
	healthy := read("testdata/healthy.jsonl")
	regression := read("testdata/regression.jsonl")
	result := Compare(healthy, regression, "stable", "candidate", 2)
	if result.Decision != "rollback" || len(result.Reasons) != 3 {
		t.Fatalf("want revenue, query and latency regressions, got %+v", result)
	}
	result = Compare(healthy, healthy, "stable", "candidate", 2)
	if result.Decision != "pass" {
		t.Fatal(result)
	}
	result = Compare(healthy, healthy, "stable", "candidate", 30)
	if result.Decision != "hold" {
		t.Fatal("small fixtures must not pass production sample threshold")
	}
}
