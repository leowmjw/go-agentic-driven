#!/usr/bin/env bash
# End-to-end scenario UI tests for the Northstar finance lab.
#
# Starts an isolated demo server (in-memory data only), seeds the fresh
# scenario, then drives the UI with agent-browser to verify:
#   - fresh scenario empty state and role selector
#   - customer payment outcomes: settled / declined / review / expired
#   - invariants: only settled payments debit balances, currencies stay separate
#   - analyst role is read-only
#   - admin can seed busy and incident scenarios from the Demo controls panel
#   - incident scenario status mix and settled-balance math
#   - /admin Datastar console requires the admin role
#   - VictoriaLogs wide events for payments/roles/outcomes landed (deltas)
#
# Usage:  bash scripts/test-scenarios.sh
# Env:    SCENARIO_TEST_PORT (default 8099)
# Dirs:   data/ (VictoriaLogs db) and tmp/ (scratch) — both gitignored
set -u

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PORT="${SCENARIO_TEST_PORT:-8099}"
BASE="http://127.0.0.1:${PORT}"
export AGENT_BROWSER_SESSION="${AGENT_BROWSER_SESSION:-scenario-ui-tests}"

PASS=0
FAIL=0
FAILED=()
T=""
SERVER_PID=""
VL_URL="http://127.0.0.1:9428"
VL_SELECT="$VL_URL/select/logsql/query"
VL_PID=""

say()  { printf '%s\n' "$*"; }
ok()   { PASS=$((PASS + 1)); printf '  [PASS] %s\n' "$1"; }
no()   { FAIL=$((FAIL + 1)); FAILED+=("$1"); printf '  [FAIL] %s\n' "$1"; }
die()  { say "ERROR: $1"; exit 2; }

check() { # haystack needle label
  if grep -Fq -- "$2" <<<"$1"; then ok "$3"; else no "$3"; fi
}

check_count() { # haystack needle expected label
  local got
  got=$(grep -Fc -- "$2" <<<"$1")
  if [ "$got" = "$3" ]; then ok "$4"; else no "$4 (got $got, want $3)"; fi
}

snap_i() { agent-browser snapshot -i 2>&1; }
snap_f() { agent-browser snapshot 2>&1; }

ref_of() { # grep-extended pattern -> first ref in an interactive snapshot
  snap_i | grep -m1 -E "$1" | sed -E 's/.*ref=(e[0-9]+).*/\1/'
}

wait_for() { # needle timeout -> 0 when needle appears in a full snapshot
  local needle="$1" timeout="${2:-30}" i
  for ((i = 0; i < timeout; i++)); do
    if grep -Fq -- "$needle" < <(snap_f); then return 0; fi
    sleep 1
  done
  return 1
}

wait_for_or_fail() {
  if wait_for "$1" "${2:-30}"; then ok "$3"; else no "$3 (timeout waiting for: $1)"; fi
}

summary_text() {
  local r
  r=$(ref_of 'region "Payment summary"')
  [ -n "$r" ] && agent-browser get text "@$r" 2>&1
}

wait_summary() { # needle timeout
  local needle="$1" timeout="${2:-30}" i
  for ((i = 0; i < timeout; i++)); do
    if grep -Fq -- "$needle" < <(summary_text); then return 0; fi
    sleep 1
  done
  return 1
}

click_ref() { # pattern label -> fails the label when element missing
  local r
  r=$(ref_of "$1")
  if [ -z "$r" ]; then no "$2 (element not found: $1)"; return 1; fi
  agent-browser click "@$r" >/dev/null 2>&1
}

login_as() { # role-word radio-index
  if snap_i | grep -Fq 'button "Sign out"'; then
    agent-browser click ".profile > .icon-button" >/dev/null 2>&1
    wait_for "You have been signed out." 15 || true
    sleep 0.5
  fi
  if ! snap_i | grep -Fq 'radio "'; then
    click_ref 'button "Enter workspace"' "open role dialog ($1)" || return 1
    wait_for "Make yourself at home." 10 || { no "role dialog opened ($1)"; return 1; }
  fi
  local radios r
  radios=$(snap_i | grep 'radio "' | sed -E 's/.*ref=(e[0-9]+).*/\1/')
  r=$(printf '%s\n' "$radios" | sed -n "$2p")
  [ -n "$r" ] || { no "role radio $2 missing ($1)"; return 1; }
  agent-browser click "@$r" >/dev/null 2>&1
  sleep 0.3
  click_ref 'button "Enter workspace"' "submit role dialog ($1)" || return 1
  if wait_for "$1 account" 15; then ok "signed in as $1"; else no "signed in as $1"; fi
}

create_payment() { # scenario-label expected-status-cell
  click_ref 'button "New payment"' "open payment form" || return 1
  wait_for "A new payment." 10 || { no "payment form opened"; return 1; }
  local sc sim
  sc=$(ref_of 'combobox "Simulation scenario"')
  sim=$(ref_of 'button "Simulate payment"')
  if [ -z "$sc" ] || [ -z "$sim" ]; then no "payment form refs missing"; return 1; fi
  agent-browser select "@$sc" "$1" >/dev/null 2>&1
  agent-browser click "@$sim" >/dev/null 2>&1
  if wait_for "StaticText \"$2\"" 30; then
    ok "payment '$1' recorded as $2"
  else
    no "payment '$1' recorded as $2"
  fi
  sleep 0.5
}

cleanup() {
  agent-browser close >/dev/null 2>&1 || true
  if [ -n "$SERVER_PID" ]; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  if [ -n "$VL_PID" ]; then
    kill "$VL_PID" 2>/dev/null || true
    wait "$VL_PID" 2>/dev/null || true
  fi
  [ -n "$T" ] && rm -rf "$T"
}
trap cleanup EXIT

# ---------------------------------------------------------------- preflight
command -v agent-browser >/dev/null 2>&1 || die "agent-browser not on PATH (add \"npm:agent-browser\" to mise.toml and run mise install)"
command -v go >/dev/null 2>&1 || die "go not on PATH"
command -v npm >/dev/null 2>&1 || die "npm not on PATH"
cd "$ROOT" || die "cannot cd to repo root"
mkdir -p data tmp
T="$ROOT/tmp/test-scenarios.$$"
mkdir -p "$T" || die "cannot create scratch dir"

if curl -s --max-time 2 -o /dev/null "$BASE/"; then
  die "port $PORT already serves $BASE — set SCENARIO_TEST_PORT to a free port"
fi

say "== Preflight =="

vl_count() { # filter expression -> integer count (0 on any failure)
  local out n
  out=$(curl -s --max-time 5 "$VL_SELECT" --data-urlencode "query=_time:1h AND $1 | count()" 2>/dev/null) || true
  n=$(printf '%s' "$out" | tr -dc '0-9')
  printf '%s' "${n:-0}"
}
vl_ready() { curl -s --max-time 2 -o /dev/null "$VL_SELECT" --data-urlencode "query=_time:1m | count()"; }

if vl_ready; then
  ok "reusing VictoriaLogs on $VL_URL"
else
  command -v victoria-logs >/dev/null 2>&1 || die "victoria-logs not on PATH (brew install victoria-logs)"
  victoria-logs -httpListenAddr=127.0.0.1:9428 -storageDataPath="$ROOT/data/victoria-logs" >"$T/victoria-logs.log" 2>&1 &
  VL_PID=$!
  vlup=""
  for _ in $(seq 1 30); do
    if vl_ready; then vlup=1; break; fi
    sleep 1
  done
  [ -n "$vlup" ] || die "victoria-logs did not start (log: $T/victoria-logs.log)"
  ok "victoria-logs started (storage: data/victoria-logs)"
fi

B_TOTAL=$(vl_count 'service.name:"finance-ledger"')
B_PAY=$(vl_count 'http.route:"/api/payments"')
B_403=$(vl_count 'http.response.status_code:"403"')
B_DEC=$(vl_count 'finance.status:"declined"')
B_REV=$(vl_count 'finance.status:"review"')
B_EXP=$(vl_count 'finance.status:"expired"')
ok "wide-event baseline captured (total=$B_TOTAL)"
if [ ! -f internal/web/dist/index.html ]; then
  say "  frontend not built; building..."
  env -u NODE_ENV npm --prefix frontend ci --include=dev >/dev/null || die "npm ci failed"
  env -u NODE_ENV npm --prefix frontend run build >/dev/null || die "frontend build failed"
  ok "frontend built"
else
  ok "frontend build present"
fi

go build -o "$T/server" ./cmd/server || die "server build failed"
go build -o "$T/factory" ./cmd/factory || die "factory build failed"
ok "go binaries built"

VICTORIALOGS_URL="$VL_URL" REGION=local "$T/server" -addr "127.0.0.1:$PORT" >"$T/server.log" 2>&1 &
SERVER_PID=$!
ready=""
for _ in $(seq 1 60); do
  if curl -s --max-time 2 -o /dev/null "$BASE/"; then ready=1; break; fi
  sleep 1
done
[ -n "$ready" ] || die "server did not become ready on $PORT (log: $T/server.log)"
ok "server listening on $PORT"

admin_code=$(curl -s -o /dev/null -w '%{http_code}' "$BASE/admin")
if [ "$admin_code" = "403" ]; then ok "/admin rejects requests without admin role (403)"; else no "/admin without role returned $admin_code, want 403"; fi

"$T/factory" seed -scenario fresh -url "$BASE" >/dev/null 2>&1 || die "factory seed fresh failed"
fresh_json=$(curl -s "$BASE/api/payments")
check "$fresh_json" '"payments":[]' "API: fresh seed leaves zero payments"
ok "seeded fresh scenario"

# ---------------------------------------------------------------- browser
say "== Browser tests (session: $AGENT_BROWSER_SESSION) =="
agent-browser close >/dev/null 2>&1 || true
agent-browser set viewport 1440 900 >/dev/null 2>&1
agent-browser open "$BASE" >/dev/null 2>&1
wait_for "Overview" 30 || die "app did not load at $BASE"

# --- fresh scenario + role selector
snapi=$(snap_i)
check "$snapi" 'link "Payment activity 0"' "fresh: nav shows 0 payments"
check "$(snap_f)" "Great things start with one payment" "fresh: empty-state message shown"
click_ref 'button "Enter workspace"' "open role selector" || true
wait_for "Make yourself at home." 10 || no "role selector dialog opened"
snapi=$(snap_i)
check "$snapi" 'radio "Customer' "role selector: Customer option present"
check "$snapi" 'radio "Analyst' "role selector: Analyst option present"
check "$snapi" 'radio "Admin' "role selector: Admin option present"

# --- customer: payment outcomes and invariants
login_as Customer 1 || true

create_payment "Successful payment" "Settled"
s=$(summary_text)
check "$s" '$25.00' "settled: USD balance is \$25.00"
check "$s" '100%' "settled: success rate 100%"
bal=$(agent-browser get text "@$(ref_of 'region "Separate settled balances by currency"')" 2>&1)
check "$bal" '€0.00' "currencies: EUR balance stays 0"
check "$bal" '£0.00' "currencies: GBP balance stays 0"

create_payment "Card declined" "Declined"
wait_summary '$25.00' 15 || true
s=$(summary_text)
check "$s" '$25.00' "decline: balance unchanged at \$25.00"
check "$s" '50%' "decline: success rate drops to 50%"

create_payment "Risk review" "Review"
wait_summary '33%' 15 || true
s=$(summary_text)
check "$s" '$25.00' "risk review: balance unchanged at \$25.00"
check "$s" '33%' "risk review: success rate 33%"

create_payment "Expired quote" "Expired"
wait_summary '25%' 15 || true
s=$(summary_text)
check "$s" '$25.00' "expired quote: balance unchanged at \$25.00"
check "$s" '25%' "expired quote: success rate 25%"

# --- analyst: read-only
login_as Analyst 2 || true
snapi=$(snap_i)
check "$snapi" 'button "New payment" [disabled' "analyst: New payment button disabled"
check "$(agent-browser get text '.profile' 2>&1)" "Read-only access" "analyst: profile shows read-only access"

# --- admin: seed busy and incident from the Demo controls panel
login_as Admin 3 || true
snapi=$(snap_i)
check "$snapi" 'heading "Demo controls"' "admin: Demo controls panel visible"
check "$snapi" 'combobox "Demo environment"' "admin: scenario select present"
check "$snapi" 'button "Load scenario"' "admin: Load scenario button present"

sel=$(ref_of 'combobox "Demo environment"')
load=$(ref_of 'button "Load scenario"')
agent-browser select "@$sel" "Busy workspace" >/dev/null 2>&1
agent-browser click "@$load" >/dev/null 2>&1
wait_summary '$364.50' 30 || true
s=$(summary_text)
check "$s" '$364.50' "busy seed: settled balance \$364.50"
check "$s" '12 successful payments' "busy seed: 12 successful payments"
check "$s" '100%' "busy seed: success rate 100%"
check_count "$(snap_f)" 'StaticText "Settled"' 12 "busy seed: 12 settled rows"

sel=$(ref_of 'combobox "Demo environment"')
load=$(ref_of 'button "Load scenario"')
agent-browser select "@$sel" "Provider incident" >/dev/null 2>&1
agent-browser click "@$load" >/dev/null 2>&1
wait_summary '$140.50' 30 || true
s=$(summary_text)
check "$s" '$140.50' "incident seed: settled balance \$140.50"
check "$s" '5 successful payments' "incident seed: 5 settled of 12"
check "$s" '42%' "incident seed: success rate 42%"
sf=$(snap_f)
check_count "$sf" 'StaticText "Settled"' 5 "incident seed: 5 settled rows"
check_count "$sf" 'StaticText "Declined"' 3 "incident seed: 3 declined rows"
check_count "$sf" 'StaticText "Review"' 2 "incident seed: 2 review rows"
check_count "$sf" 'StaticText "Expired"' 2 "incident seed: 2 expired rows"

# --- admin console (Datastar) while still admin
agent-browser open "$BASE/admin" >/dev/null 2>&1
wait_for "Ledger operations" 15 || no "admin console loaded"
check "$(snap_f)" "Live ledger" "admin console: live ledger section shown"
agent-browser open "$BASE" >/dev/null 2>&1
wait_for "Overview" 15 || no "returned to app from admin console"

# ------------------------------------------------- wide-event telemetry
say "== Wide events in VictoriaLogs =="
# export is bounded/async: poll until deltas appear or 45s elapses
D_TOTAL=0 D_PAY=0 D_403=0 D_DEC=0 D_REV=0 D_EXP=0
for _ in $(seq 1 45); do
  D_TOTAL=$(( $(vl_count 'service.name:"finance-ledger"') - B_TOTAL ))
  D_PAY=$(( $(vl_count 'http.route:"/api/payments"') - B_PAY ))
  D_403=$(( $(vl_count 'http.response.status_code:"403"') - B_403 ))
  D_DEC=$(( $(vl_count 'finance.status:"declined"') - B_DEC ))
  D_REV=$(( $(vl_count 'finance.status:"review"') - B_REV ))
  D_EXP=$(( $(vl_count 'finance.status:"expired"') - B_EXP ))
  if [ "$D_TOTAL" -ge 1 ] && [ "$D_PAY" -ge 1 ] && [ "$D_403" -ge 1 ] && \
     [ "$D_DEC" -ge 1 ] && [ "$D_REV" -ge 1 ] && [ "$D_EXP" -ge 1 ]; then
    break
  fi
  sleep 1
done

telemetry_ok=1
telemetry() { # value min label
  if [ "$1" -ge "$2" ]; then ok "$3 ($1)"; else no "$3 ($1 < $2)"; telemetry_ok=0; fi
}
telemetry "$D_TOTAL" 1 "ledger events exported"
telemetry "$D_PAY" 1 "POST/GET /api/payments events exported"
telemetry "$D_403" 1 "403 forbidden events exported"
telemetry "$D_DEC" 1 "declined payment events exported"
telemetry "$D_REV" 1 "review payment events exported"
telemetry "$D_EXP" 1 "expired quote events exported"
if [ "$telemetry_ok" = "1" ] && [ "$D_TOTAL" -gt 0 ]; then
  say "  feedback: +$D_TOTAL events (payments=$D_PAY, 403=$D_403, declined=$D_DEC, review=$D_REV, expired=$D_EXP)"
fi

# ---------------------------------------------------------------- report
say ""
say "== Results: $PASS passed, $FAIL failed =="
if [ "$FAIL" -gt 0 ]; then
  for f in "${FAILED[@]}"; do say "  FAIL: $f"; done
  exit 1
fi
say "All scenario checks passed."
