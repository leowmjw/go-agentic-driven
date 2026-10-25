// Command sensor evaluates release telemetry or records a bounded agent task event.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/leowmjw/go-agentic-driven/internal/gate"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, out, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "event" {
		return event(args[1:], out, stderr)
	}
	f := flag.NewFlagSet("sensor", flag.ContinueOnError)
	f.SetOutput(stderr)
	logs := f.String("logs", "http://localhost:9428", "VictoriaLogs base URL")
	baseline := f.String("baseline", "", "baseline service.version")
	canary := f.String("canary", "", "canary service.version")
	min := f.Int("min-samples", 30, "minimum distinct sessions per cohort and version")
	baselineFile := f.String("baseline-file", "", "baseline wide-event JSONL (offline)")
	canaryFile := f.String("canary-file", "", "canary wide-event JSONL (offline)")
	if err := f.Parse(args); err != nil {
		return 2
	}
	result := gate.Hold("invalid arguments")
	if f.NArg() != 0 || *baseline == "" || *canary == "" || *baseline == *canary || *min < 1 {
		result = gate.Hold("distinct -baseline and -canary and positive -min-samples are required; no positional arguments")
	} else if (*baselineFile == "") != (*canaryFile == "") {
		result = gate.Hold("both fixture files must be provided together")
	} else {
		load := func(file, version string) ([]gate.Event, error) {
			if file == "" {
				return gate.Fetch(context.Background(), http.DefaultClient, *logs, version)
			}
			reader, err := os.Open(file)
			if err != nil {
				return nil, err
			}
			defer reader.Close()
			return gate.Decode(reader)
		}
		b, err := load(*baselineFile, *baseline)
		if err != nil {
			result = gate.Hold("baseline: " + err.Error())
		} else {
			c, err := load(*canaryFile, *canary)
			if err != nil {
				result = gate.Hold("canary: " + err.Error())
			} else {
				result = gate.Compare(b, c, *baseline, *canary, *min)
			}
		}
	}
	if err := json.NewEncoder(out).Encode(result); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	switch result.Decision {
	case "pass":
		return 0
	case "rollback":
		return 1
	default:
		return 2
	}
}

func event(args []string, out, stderr io.Writer) int {
	f := flag.NewFlagSet("sensor event", flag.ContinueOnError)
	f.SetOutput(stderr)
	logs := f.String("logs", "http://localhost:9428", "VictoriaLogs base URL")
	name := f.String("name", "", "agent task name")
	spec := f.String("spec", "", "task specification or reference (do not include secrets)")
	tokens := f.Int64("tokens", 0, "reported token usage")
	retries := f.Int64("retries", 0, "reported retry count")
	testsPassed := f.Bool("tests-passed", false, "whether task tests passed")
	rollback := f.Bool("rollback", false, "whether task rolled back")
	human := f.Bool("human-intervened", false, "whether a human intervened")
	maxTokens := f.Int64("max-tokens", 100000, "maximum reported token usage")
	maxRetries := f.Int64("max-retries", 3, "maximum reported retry count")
	if err := f.Parse(args); err != nil {
		return 2
	}
	if f.NArg() != 0 || *name == "" || *spec == "" || len(*name) > 256 || len(*spec) > 8192 ||
		*tokens < 0 || *retries < 0 || *maxTokens < 0 || *maxRetries < 0 {
		fmt.Fprintln(stderr, "name/spec are required, lengths bounded, and usage/budgets must be nonnegative")
		return 2
	}
	exceeded := *tokens > *maxTokens || *retries > *maxRetries
	payload, err := json.Marshal(map[string]any{
		"_time": time.Now().UTC().Format(time.RFC3339Nano), "_msg": "agent task completed",
		"event.name": "agent.task", "service.name": "sensor", "agent.name": *name, "agent.spec": *spec,
		"agent.tokens": *tokens, "agent.retries": *retries, "agent.tests_passed": *testsPassed,
		"agent.rollback": *rollback, "agent.human_intervened": *human,
		"agent.max_tokens": *maxTokens, "agent.max_retries": *maxRetries, "agent.budget_exceeded": exceeded,
	})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	// Print the exact event even when ingestion fails, but never report success.
	if _, err := fmt.Fprintln(out, string(payload)); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if err := gate.WriteEvent(context.Background(), http.DefaultClient, *logs, payload); err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if exceeded {
		fmt.Fprintln(stderr, "reported agent budget exceeded")
		return 1
	}
	return 0
}
