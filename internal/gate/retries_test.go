package gate

import "testing"

func TestCachedRetryDoesNotDuplicateConversionOrRevenue(t *testing.T) {
	baseline, canary := fixture("stable", 100), fixture("candidate", 100)
	replay := Event{}
	for key, value := range canary[0] {
		replay[key] = value
	}
	replay["cache.hit"] = "true"
	canary = append(canary, replay)
	result := Compare(baseline, canary, "stable", "candidate", 30)
	if result.Decision != "pass" {
		t.Fatal(result)
	}
	metrics := result.Cohorts[0].Canary
	if metrics.Conversion != 1 || metrics.Revenue != 10000 || metrics.Sessions != 100 || metrics.Requests != 101 {
		t.Fatal(metrics)
	}
	replay["cache.hit"] = "invalid"
	if result := Compare(baseline, canary, "stable", "candidate", 30); result.Decision != "hold" {
		t.Fatal(result)
	}
}
