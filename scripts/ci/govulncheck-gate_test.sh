#!/usr/bin/env bash
# [F241-01] Self-test (positive controls) for govulncheck-gate.sh.
# Runs the gate against fixtures and compares exit codes. Prints one
# "PASS T<n>" / "FAIL T<n>" line per case; exits 1 if any case fails.
# Allowlist expiry dates in fixtures are 2999-12-31 or 2000-01-01 so the
# tests never age out with the real date.
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
gate="${GATE:-$here/govulncheck-gate.sh}"
fx="$here/testdata/govulncheck"
allow_ok="$fx/allow-ok.txt"

command -v jq >/dev/null 2>&1 || { echo "FAIL setup: jq not found"; exit 1; }
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fails=0

# run_gate <json> <allowlist>: prints the gate's exit code.
run_gate() {
  local rc=0
  bash "$gate" "$1" "$2" >"$tmp/out.txt" 2>&1 || rc=$?
  echo "$rc"
}

# check <name> <want-rc> <json> <allowlist> [<json2>]: all json inputs must return want-rc.
check() {
  local name="$1" want="$2" allow="$3" ok=1 got
  shift 3
  for j in "$@"; do
    got="$(run_gate "$j" "$allow")"
    if [ "$got" != "$want" ]; then
      ok=0
      echo "  $name: input $(basename "$j") want rc=$want got rc=$got" >&2
    fi
  done
  if [ "$ok" = 1 ]; then echo "PASS $name"; else echo "FAIL $name"; fails=$((fails + 1)); fi
}

# jq filter applying a setter to every echo finding's trace[0].
mut_echo() { # <jq assignment on .finding.trace[0]>
  jq -c "if .finding and .finding.trace[0].module==\"github.com/labstack/echo/v4\" then (.finding.trace[0] |= ($1)) else . end" "$fx/base.jsonl"
}

base="$fx/base.jsonl"

# T1: only allowlisted osvs called, version ok, not expired -> 0
check T1 0 "$allow_ok" "$base"

# T2: extra called osv outside the allowlist -> 1
cp "$base" "$tmp/t2.jsonl"
jq -c 'select(.finding.osv=="GO-2026-6872" and .finding.trace[0].function) | .finding.osv="GO-2099-0001"' "$base" | head -1 >>"$tmp/t2.jsonl"
check T2 1 "$allow_ok" "$tmp/t2.jsonl"

# T3: same as T1 but allowlist expired -> 1
check T3 1 "$fx/allow-expired.txt" "$base"

# T4: finding version below the floor -> 1
mut_echo '.version="v4.16.2"' >"$tmp/t4.jsonl"
check T4 1 "$allow_ok" "$tmp/t4.jsonl"

# T5: non-allowlisted osv with only module / package level findings -> 0
cp "$base" "$tmp/t5.jsonl"
echo '{"finding":{"osv":"GO-2099-0002","trace":[{"module":"example.com/other","version":"v1.0.0","package":"example.com/other/pkg"}]}}' >>"$tmp/t5.jsonl"
check T5 0 "$allow_ok" "$tmp/t5.jsonl"

# T6a: empty file -> 2
: >"$tmp/empty.jsonl"
check T6a 2 "$allow_ok" "$tmp/empty.jsonl"

# T6b: content but no config message -> 2
grep -v '^{"config"' "$base" >"$tmp/noconfig.jsonl"
check T6b 2 "$allow_ok" "$tmp/noconfig.jsonl"

# T7: allowlisted osv but a different module -> 1
mut_echo '.module="github.com/labstack/echo/v5"' >"$tmp/t7.jsonl"
check T7 1 "$allow_ok" "$tmp/t7.jsonl"

# T8: valid config + findings, then truncated JSON tail -> 2
cp "$base" "$tmp/t8.jsonl"
printf '%s' '{"finding":{"osv":"GO-2026-6872","trace":[{"modu' >>"$tmp/t8.jsonl"
check T8 2 "$allow_ok" "$tmp/t8.jsonl"

# T9: scan_level package, and scan_mode binary -> both 2
jq -c 'if .config then .config.scan_level="package" else . end' "$base" >"$tmp/t9a.jsonl"
jq -c 'if .config then .config.scan_mode="binary" else . end' "$base" >"$tmp/t9b.jsonl"
check T9 2 "$allow_ok" "$tmp/t9a.jsonl" "$tmp/t9b.jsonl"

# T10: prerelease and pseudo-version finding versions -> both 2
mut_echo '.version="v4.16.3-rc.1"' >"$tmp/t10a.jsonl"
mut_echo '.version="v4.16.3-0.20261001000000-abcdef123456"' >"$tmp/t10b.jsonl"
check T10 2 "$allow_ok" "$tmp/t10a.jsonl" "$tmp/t10b.jsonl"

# T11: allowlist row with 4 columns (reason URL removed) -> 2
check T11 2 "$fx/allow-4col.txt" "$base"

# T12: allowlist expiry column is "never" -> 2
check T12 2 "$fx/allow-never.txt" "$base"

# T13: allowlist file missing -> 2
check T13 2 "$tmp/does-not-exist.txt" "$base"

if [ "$fails" -ne 0 ]; then
  echo "SELFTEST FAIL: $fails case(s)"
  exit 1
fi
echo "SELFTEST OK"
