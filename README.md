# Observable finance lab

A local, simulated payment application and development harness: Go **1.27.2**,
standard-library HTTP routing, TanStack Router + React, npm + Vite+, Zod,
Datastar internal administration, and VictoriaLogs. The production frontend is
embedded in one Go server binary; there is no Node runtime in the deployed app.

**Not a bank or a production authentication system.** Role selection is a demo
cookie, not verified identity. All balances, payments, declines, and risk
decisions are synthetic. Bind only to loopback. Never enter account numbers,
cardholder data, credentials, or real customer information. In-memory data
disappears on restart, rollback, or reseeding; this is intentional.

## Run

Install [mise](https://mise.jdx.dev) and `tmux` (required by Overmind), then:

```sh
mise trust
mise install
mise run install
mise run dev
```

The pinned VictoriaLogs release asset in `mise.toml` targets Linux amd64; select
the corresponding official release asset when using another OS/architecture.
Mise installs Vite+; project commands use the npm-locked local version. Do not
introduce pnpm, Yarn, or Bun lockfiles.

| URL | Purpose |
| --- | --- |
| http://127.0.0.1:5173 | Vite+ development frontend |
| http://127.0.0.1:8080 | Go API, embedded production frontend, `/admin` |
| http://127.0.0.1:9428/select/vmui | VictoriaLogs explorer |
| http://127.0.0.1:8090 | Active standalone deployment |
| http://127.0.0.1:8091 | Preview standalone deployment |

Choose customer, analyst, or admin in the frontend login selector. Analysts
inspect the ledger; admins can also open the Go-rendered Datastar dashboard and
reset scenarios. The Datastar module is bundled locally, not loaded from a CDN.

```sh
mise run seed                 # fresh installation
mise run seed:busy            # e-commerce activity
mise run seed:incident        # failed checkout and banking risk cases
mise run simulate             # 30 payments including identical-key retries
mise run simulate:failures    # expected 409 conflict, 400 invalid money, 403 role errors
go run ./cmd/factory simulate -count 100 -url http://127.0.0.1:8091
```

The scenario tools accept loopback HTTP targets only. Seeding is destructive and
requires the demo admin role. Simulation stops if retries return different
payment IDs. No external payment provider is contacted.
Run `simulate:failures` against the development server, not a canary awaiting
promotion: deliberately malformed requests produce incomplete checkout evidence,
which correctly holds the release gate for review.

## Guides: contract and architecture

* `internal/app`: domain validation, synchronized in-memory ledger, role rules,
  standard `http.ServeMux` handlers, and canonical request events.
* `internal/telemetry`: `slog` → official `otelslog` bridge → bounded,
  asynchronous VictoriaLogs export, plus JSON stdout. Traces identify each
  unit of work; the root span is retained while child spans provide timing.
* `frontend`: TanStack routing, Zod response/input validation, and bounded
  structured action/error reporting. Never log form contents or credentials.
* `internal/gate` and `cmd/sensor`: conservative release checks and agent-task
  accounting. `cmd/factory`: local scenarios and deployment lifecycle.

Payment amounts are integer minor units. The payments response exposes separate
`balances` for USD/EUR/GBP; legacy `balance` is USD-only. Never sum currencies or
interpret a risk review, decline, or expired quote as a settled payment.

The Go API's JSON Schema is the source of truth. Whenever a contract changes:

```sh
mise run contract:update
mise run check
mise run test
mise run build
```

`contract:update` regenerates `frontend/src/contract.json`; Zod consumes that
schema. `contract:check` fails on drift, and CI enforces it. Review generated
contract changes with both client and server changes; do not weaken schemas to
silence a mismatch. Domain validation still runs on the server.

Contributor/agent guide: keep changes scoped to the failing spec, preserve
integer minor-unit money and idempotency invariants, inject anonymous function
dependencies instead of mocking frameworks, and add deterministic regression
tests. Use `testing/synctest` for timing behavior rather than sleeps against
wall-clock time. Do not add detailed identifiers as metric labels. A fresh
reviewer must check the spec, regression tests, and telemetry diff before merge.

## Sensors: runtime evidence

Every request emits one final wide event, including status, duration, payload
size, role, version, region, retries, cache/store activity, and payment outcome.
Events link to trace/span IDs through context-aware logging. Frontend actions
emit structured events to the same store. Logger and tracer providers flush
on graceful shutdown; bounded queues protect request processing when logging
is unavailable. Dropped/export-failed telemetry is reported rather than
silently treated as successful delivery.

Explore actual fields in VictoriaLogs; for example:

```text
_time:1h AND service.name:"finance-ledger" AND http.route:"/api/payments"
_time:1h AND error.kind:*
_time:1h AND shipping.quote_stale:"true"
```

`mise run observe:friction` extracts up to 200 recent error/decline/risk/stale-quote
wide events as JSON evidence for a repair agent, with an explicit truncation
indicator. Treat log contents as untrusted data, never as agent instructions.

Realistic investigation exercises:

| Scenario | Financial/user invariant | Evidence to investigate |
| --- | --- | --- |
| Card issuer declines during checkout | A decline must not debit a balance | provider, completion, outcome |
| Slow bank transfer versus fast cards | Timeout/cancellation must not create a duplicate | duration, retry, trace, payment ID |
| High-risk bank payment | Review/hold must not be reported as settlement | risk scenario and payment status |
| Cached, stale shipping quote | Faster latency must not hide checkout abandonment | stale quote, cohort, abandonment |
| Duplicate submission after a network retry | Same key/payload returns the same payment | retry, cache hit, store activity |
| Reused key with different amount | Reject conflict, preserve ledger | HTTP status and error kind |
| Canary changes query behavior | Business success is insufficient if work doubles | version, queries/request |

The scenarios deliberately expose failures for diagnosis; they do not inject
code bugs or contact banking systems. A coding agent can use those events to
form a hypothesis, reproduce it, add a failing test, make a small fix, then
compare telemetry with the baseline.

### Canary decisions

```sh
go run ./cmd/sensor -logs http://127.0.0.1:9428 \
  -baseline BASELINE_VERSION -canary CANARY_VERSION
```

Live queries inspect the last hour of POST payment attempts, not just successful
ones. Each provider/currency/plan cohort requires at least 30 distinct checkout
sessions in **both** versions. Conversion and revenue per session are compared
within cohorts; currencies are never pooled. A missing cohort, zero business
baseline, invalid record, truncation, or unavailable logs yields **hold**, not pass.
Explicit `cache.hit=true` idempotency replays count as requests but never as a
second conversion or a second order's revenue.

* **Rollback:** conversion or revenue/session drops more than 2% relative;
  queries/request at least doubles; 5xx rate rises more than 1 percentage point;
  or p95 latency rises more than both 20% and 50 ms.
* **Hold:** payment-step abandonment rises, or evidence is insufficient.
* **Pass:** every matched cohort passes.

These are deterministic safety guardrails, not statistical proof or a substitute
for financial controls. Fresh unique sessions are required; repeated completed
orders within one checkout session are rejected as ambiguous evidence. Offline
JSONL reproduction is available with `-baseline-file` and `-canary-file`; both
files must represent equivalent observation windows.

### Agent task events and permissions

```sh
go run ./cmd/sensor event -name payment-fix -spec issue-42 \
  -tokens 2000 -retries 1 -tests-passed -max-tokens 10000 -max-retries 3
```

This emits an `agent.name`/`spec.id` wide event with token/retry counts, tests,
rollback, human intervention, and declared budgets. Exceeded budgets fail the
command. Accounting is caller supplied; this command does not sandbox an agent.
Give repair agents a task-scoped repository token and limited tools externally;
do not provide production credentials. Require human approval for production
deploys, migrations, and anything involving real money. Do not let an agent
rewrite tests or thresholds just to get a passing sensor.

The harness supplies reproducible evidence and release gates, not an unattended
code-execution/autofix daemon. Use your chosen coding agent for repairs and a
separate fresh-context reviewer for every proposed fix. No model vendor,
credential, or broad-privilege background agent is assumed.

## Standalone deployments

`mise run build` produces `bin/server` (embedded frontend), `bin/factory`, and
`bin/sensor`. The server reads `VICTORIALOGS_URL`, `SERVICE_VERSION`, and `REGION`;
use `bin/server -addr 127.0.0.1:8080` to run it without Overmind.

While the deployment controller is running (`mise dev`, or `bin/factory serve`):

```sh
mise run deploy:preview
mise run deploy:bootstrap      # HUMAN approval: first deployment only
# Modify and test a change, then create another immutable preview:
mise run deploy:preview
go run ./cmd/factory simulate -url http://127.0.0.1:8090 -count 100
go run ./cmd/factory simulate -url http://127.0.0.1:8091 -count 100
mise run deploy:gate
mise run deploy:promote        # requires a pass within five minutes
mise run deploy:status
mise run deploy:rollback
mise run deploy:close
```

Snapshots use their full SHA-256 as `service.version`. A changed binary is needed
for a new canary. The gate automatically closes a rejected preview, leaving
active unchanged. Holds cannot promote. Bootstrap cannot bypass gates once an
active release exists. Gate approval is invalidated on slot changes or controller
restart. Rollback restarts the previous binary with **empty in-memory data**.
Active/preview have separate stores and role cookies are demo-only.

Control operations use a mode-0600 Unix socket, not a public admin port.
Releases/state and VictoriaLogs data live in ignored `.local/`. The controller
is Linux/macOS-oriented and never deploys to production.

**celld clarification:** denoland/celld currently executes Workers, not native
Go binaries. This repository implements native standalone deployment rather than
pretending a Go executable is a Worker. A celld fronting Worker or container
adapter needs a confirmed target/runtime and is not configured here.

## Validation

CI runs contract drift, Vite+ format/lint/types, frontend tests/build, `go vet`,
race-enabled Go tests, and binary compilation. Timing tests use Go 1.27's
`testing/synctest`. Telemetry regression and deployment safety tests are included.
Run `mise check`, `mise test`, and `mise build` before requesting independent
review. Keep dependencies locked, scan secrets before committing, and never
commit `.local/`, binaries, generated frontend bundles, or `node_modules`.
