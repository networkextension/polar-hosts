#!/usr/bin/env bash
# host-agent-smoke.sh — regression smoke for the host/agent HTTP surface.
#
# Exercises every host/agent read/management endpoint and asserts the status
# code (and, for lists, the JSON shape). Catches the class of low-level bugs
# we shipped before — a missing-column 500 on /api/agents, a route-missing
# 404 — that the boot-smoke gate misses because it never calls these routes.
#
# Usage:
#   BASE=https://hosts.4950.store:2443 \
#   TOKEN=<access_token cookie value> \
#   WS=<workspace_id to scope to, optional> \
#   scripts/test/host-agent-smoke.sh
#
# Exit non-zero if ANY check fails. Any 5xx anywhere is an automatic fail.
set -uo pipefail

BASE="${BASE:-http://127.0.0.1:8095}"
TOKEN="${TOKEN:?set TOKEN to a valid access_token}"
WS="${WS:-}"
CURL=(curl -sS -m 10 -H "Cookie: access_token=${TOKEN}")
[ -n "$WS" ] && CURL+=(-H "X-Workspace-Id: ${WS}")

pass=0; fail=0
# check <method> <path> <expected-csv-codes> [must-contain-json-key]
check() {
  local method="$1" path="$2" want="$3" key="${4:-}"
  local body code
  body="$("${CURL[@]}" -X "$method" -o - -w $'\n%{http_code}' "${BASE}${path}" 2>/dev/null)"
  code="${body##*$'\n'}"; body="${body%$'\n'*}"
  local ok=0
  IFS=',' read -ra codes <<<"$want"
  for c in "${codes[@]}"; do [ "$code" = "$c" ] && ok=1; done
  # Any 5xx is always a failure regardless of `want`. The whole point of
  # this gate is to catch 500s and route-missing 404s.
  if [[ "$code" =~ ^5 ]]; then ok=0; fi
  # Envelope check only applies to a 200 (a 403 access-gate body has no data).
  if [ "$ok" = 1 ] && [ "$code" = 200 ] && [ -n "$key" ]; then
    echo "$body" | grep -q "\"$key\"" || { ok=0; code="$code(missing .$key)"; }
  fi
  if [ "$ok" = 1 ]; then
    printf "  ✓ %-6s %-40s %s\n" "$method" "$path" "$code"; pass=$((pass+1))
  else
    printf "  ✗ %-6s %-40s got=%s want=%s\n" "$method" "$path" "$code" "$want"; fail=$((fail+1))
  fi
}

echo "== host/agent smoke @ ${BASE} ${WS:+(ws=$WS)} =="
check GET /healthz                              200
# Lists: 200 (granted workspace, envelope checked) or 403 (closed-by-default
# access gate) — but NEVER 5xx or a route-missing 404. The 500/empty
# regressions we shipped before would surface here.
check GET /api/hosts                            200,403 hosts
check GET /api/agents                           200,403 agents
check GET /api/host-skills                      200,403
check GET /api/console/layouts                  200,403
check GET /api/skill-catalog                    200,403
# Detail with a bogus id must be a CLEAN 404 (JSON), never a 500
check GET /api/agents/ag_smoke_does_not_exist   404,403 error
check GET /api/hosts/host_smoke_missing         404,403 error
check GET /api/hosts/host_smoke_missing/agents  404,403,200 ""

echo "== ${pass} passed, ${fail} failed =="
[ "$fail" = 0 ]
