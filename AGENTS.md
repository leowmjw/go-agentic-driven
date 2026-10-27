# AGENTS.md

## Scenario E2E tests (agent-browser)

`bash scripts/test-scenarios.sh` — one-shot suite, exit 0 = all pass.

- Starts an isolated server on 127.0.0.1:8099 (`SCENARIO_TEST_PORT` overrides),
  reuses or starts VictoriaLogs on 9428 (brew, not mise), seeds scenarios,
  drives the UI with agent-browser, then asserts wide-event deltas in
  VictoriaLogs (events, 403s, declined/review/expired outcomes).
- Destructive only to its own in-memory store; never touches the 8080 dev server.
- `data/` (VictoriaLogs db) and `tmp/` (script scratch) are gitignored.
- Expected values: busy = 12 settled / $364.50; incident = 12 rows
  (5 settled, 3 declined, 2 review, 2 expired) / $140.50.

## Environment gotchas

- `NODE_ENV=production` is set here, so npm silently skips devDependencies.
  Use `env -u NODE_ENV npm --prefix frontend ci --include=dev` and
  `env -u NODE_ENV npm --prefix frontend run build`.
- The server embeds `internal/web/dist` (`go:embed`). Build the frontend before
  `go run ./cmd/server`, else the SPA returns 503 "Frontend not built".
- Avoid `mise run dev` on macOS: the pinned VictoriaLogs asset is linux-amd64
  and Overmind needs tmux. `go run ./cmd/server` is enough for UI testing.
- VictoriaLogs comes from Homebrew (`brew install victoria-logs`), storage in
  `data/victoria-logs`; wire the server with `VICTORIALOGS_URL=http://127.0.0.1:9428`.
- LogSQL grouping is not needed: filter counts work, e.g.
  `_time:1h AND finance.status:"declined" | count()`; dotted fields are bare
  keys in filter expressions.
- `agent-browser` is pinned in `mise.toml` (`npm:agent-browser`); its shim is on
  PATH after `mise install`. Chrome comes from `agent-browser install`.

## agent-browser patterns (verified here)

- Always export `AGENT_BROWSER_SESSION` (the test script uses `scenario-ui-tests`).
- Set the viewport first: `agent-browser set viewport 1440 900` — the default
  577px height leaves the sidebar Sign-out button below the fold.
- Sign out with CSS: `agent-browser click ".profile > .icon-button"`; `@eN` ref
  clicks on that button were unreliable.
- Re-snapshot after every re-render — refs shift. Extract refs with
  `sed -E 's/.*ref=(e[0-9]+).*/\1/'` (a loose `e[0-9]*` also matches the "e" in
  "expanded").
- `find` requires a locator type: `find role button click --name "…"`,
  `find text "…" click`. `find button "…" click` is invalid.
- `wait --text` matches text nodes only: aria-labels ("Sign out") and split nav
  counts never match. Poll `agent-browser snapshot` with grep instead.
- Modal dialogs replace the snapshot tree with dialog-only elements, so refs
  inside a dialog are unambiguous.
- Bash: escape money in double quotes — `"\$25.00"` (`$25` parses as `$2` under
  `set -u`).

## Scenario semantics (internal/app/store.go)

- Scenario → status: normal/slow → settled, decline → declined, risk → review,
  stale_quote → expired. Only `settled` debits a balance; currencies are never summed.
- Idempotency: same key + same payload → same payment (200, cached); same key +
  different payload → 409 conflict.
- Seeding replaces the whole store (destructive), requires the demo admin role,
  and scenario tools accept loopback HTTP targets only.

## TODO: end-to-end test gaps

`scripts/test-scenarios.sh` does not yet cover:

- **Server-side authorization**: customer must get 403 on `/api/seed`, analyst
  403 on `POST /api/payments` — currently only the UI (disabled button) is checked.
- **Idempotency/retry**: same key + payload → 200 cached with the same payment ID;
  same key + different amount → 409. CLI/Go tests only, never asserted in the UI.
- **Bank provider + slow provider**: all scripted payments use Card; the slow
  path (timeout must not duplicate a payment) is untested.
- **Multi-currency settlements**: EUR/GBP only verified as zero — no real EUR/GBP
  payment proving balances bucket per currency and are never summed.
- **Filters/search**: "Successful" / "Other" filters and the search box are unasserted.
- **Error-path UI**: rendering of 409 conflict / 400 invalid money / 403 role errors
  as notifications (only success notices are checked).
- **Narrow viewport**: at ≤800px CSS hides `.profile > .icon-button` (Sign out)
  while signed in — possibly no way to sign out on mobile. Needs a test and fix.
- **Concurrency**: double-clicking "Simulate payment", or seeding while a slow
  payment is in flight.
- **Telemetry/deploy gate**: VictoriaLogs wide-event assertions and the
  preview/gate/promote/rollback lifecycle (needs VictoriaLogs running).
