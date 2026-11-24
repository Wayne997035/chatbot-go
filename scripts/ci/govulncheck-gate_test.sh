#!/usr/bin/env bash
# [F241-01] Self-test (positive controls) for govulncheck-gate.sh.
# Runs the gate against fixtures and compares exit codes. Prints one
# "PASS T<n>" / "FAIL T<n>" line per case; exits 1 if any case fails.
# [F37-24] The gate caps allowlist expiry at 30 days from today, so every
# allowlist that must be accepted is generated here with an expiry relative to
# the real date (jq, UTC). Static fixtures only carry dates that are expected
# to be rejected (2999-12-31) or already expired (2000-01-01).
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
gate="${GATE:-$here/govulncheck-gate.sh}"
fx="$here/testdata/govulncheck"

command -v jq >/dev/null 2>&1 || { echo "FAIL setup: jq not found"; exit 1; }
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

fails=0

# date_plus <days>: UTC date <days> from now, same expression family the gate uses.
date_plus() {
  jq -nr --argjson n "$1" 'now + $n * 86400 | strftime("%Y-%m-%d")'
}

# mk_allow <file> <expiry>: the two echo rows with the given expiry.
mk_allow() {
  {
    echo "GO-2026-6872 github.com/labstack/echo/v4 v4.16.3 $2 https://example.invalid/a"
    echo "GO-2026-6875 github.com/labstack/echo/v4 v4.16.3 $2 https://example.invalid/b"
  } >"$1"
}

allow_ok="$tmp/allow-ok.txt"
mk_allow "$allow_ok" "$(date_plus 20)"

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

# check_msg <name> <want-rc> <must-match ERE> <must-not-match ERE or ""> <allowlist> <json>...:
# every input must return want-rc AND print the expected message, so a right
# exit code for the wrong reason (for example the expiry cap) cannot pass.
check_msg() {
  local name="$1" want="$2" must="$3" mustnot="$4" allow="$5" ok=1 got j
  shift 5
  for j in "$@"; do
    got="$(run_gate "$j" "$allow")"
    if [ "$got" != "$want" ]; then
      ok=0
      echo "  $name: input $(basename "$j") want rc=$want got rc=$got" >&2
    fi
    if ! grep -Eq -- "$must" "$tmp/out.txt"; then
      ok=0
      echo "  $name: input $(basename "$j") output lacks /$must/: $(head -c 200 "$tmp/out.txt" | tr '\n' ' ')" >&2
    fi
    if [ -n "$mustnot" ] && grep -Eq -- "$mustnot" "$tmp/out.txt"; then
      ok=0
      echo "  $name: input $(basename "$j") output must not match /$mustnot/" >&2
    fi
  done
  if [ "$ok" = 1 ]; then echo "PASS $name"; else echo "FAIL $name"; fails=$((fails + 1)); fi
}

# out_has <name> <ERE>: the last gate run's output must match.
out_has() {
  if grep -Eq -- "$2" "$tmp/out.txt"; then echo "PASS $1"; else
    echo "FAIL $1"
    echo "  $1: last output lacks /$2/" >&2
    fails=$((fails + 1))
  fi
}

# jq filter applying a setter to every echo finding's trace[0].
mut_echo() { # <jq assignment on .finding.trace[0]>
  jq -c "if .finding and .finding.trace[0].module==\"github.com/labstack/echo/v4\" then (.finding.trace[0] |= ($1)) else . end" "$fx/base.jsonl"
}

base="$fx/base.jsonl"

# T1: only allowlisted osvs called, version ok, not expired -> 0
check_msg T1 0 'ALLOWED GO-2026-6872' 'days away' "$allow_ok" "$base"

# T2: extra called osv outside the allowlist -> 1
cp "$base" "$tmp/t2.jsonl"
jq -c 'select(.finding.osv=="GO-2026-6872" and .finding.trace[0].function) | .finding.osv="GO-2099-0001"' "$base" | head -1 >>"$tmp/t2.jsonl"
check_msg T2 1 'GO-2099-0001.*not in allowlist' 'days away' "$allow_ok" "$tmp/t2.jsonl"

# T3: same as T1 but allowlist expired -> 1 (the expiry cap must not turn past dates into rc 2)
check_msg T3 1 'allowlist entry expired' 'days away' "$fx/allow-expired.txt" "$base"

# T4: finding version below the floor -> 1
mut_echo '.version="v4.16.2"' >"$tmp/t4.jsonl"
check_msg T4 1 'version below allowlist minimum' 'days away' "$allow_ok" "$tmp/t4.jsonl"

# T5: non-allowlisted osv with only module / package level findings -> 0
cp "$base" "$tmp/t5.jsonl"
echo '{"finding":{"osv":"GO-2099-0002","trace":[{"module":"example.com/other","version":"v1.0.0","package":"example.com/other/pkg"}]}}' >>"$tmp/t5.jsonl"
check T5 0 "$allow_ok" "$tmp/t5.jsonl"

# T6a: empty file -> 2
: >"$tmp/empty.jsonl"
check_msg T6a 2 'no config message|cannot' 'days away' "$allow_ok" "$tmp/empty.jsonl"

# T6b: content but no config message -> 2
grep -v '^{"config"' "$base" >"$tmp/noconfig.jsonl"
check_msg T6b 2 'no config message|cannot' 'days away' "$allow_ok" "$tmp/noconfig.jsonl"

# T7: allowlisted osv but a different module -> 1
mut_echo '.module="github.com/labstack/echo/v5"' >"$tmp/t7.jsonl"
check_msg T7 1 'module differs from allowlist' 'days away' "$allow_ok" "$tmp/t7.jsonl"

# T8: valid config + findings, then truncated JSON tail -> 2
cp "$base" "$tmp/t8.jsonl"
printf '%s' '{"finding":{"osv":"GO-2026-6872","trace":[{"modu' >>"$tmp/t8.jsonl"
check_msg T8 2 'cannot parse' 'days away' "$allow_ok" "$tmp/t8.jsonl"

# T9: scan_level package, and scan_mode binary -> both 2
jq -c 'if .config then .config.scan_level="package" else . end' "$base" >"$tmp/t9a.jsonl"
jq -c 'if .config then .config.scan_mode="binary" else . end' "$base" >"$tmp/t9b.jsonl"
check_msg T9a 2 'need symbol' 'days away' "$allow_ok" "$tmp/t9a.jsonl"
check_msg T9b 2 'need source' 'days away' "$allow_ok" "$tmp/t9b.jsonl"

# T10: prerelease and pseudo-version finding versions -> both 2
mut_echo '.version="v4.16.3-rc.1"' >"$tmp/t10a.jsonl"
mut_echo '.version="v4.16.3-0.20261001000000-abcdef123456"' >"$tmp/t10b.jsonl"
check_msg T10a 2 'not vX\.Y\.Z' 'days away' "$allow_ok" "$tmp/t10a.jsonl"
check_msg T10b 2 'not vX\.Y\.Z' 'days away' "$allow_ok" "$tmp/t10b.jsonl"

# T11: allowlist row with 4 columns (reason URL removed) -> 2
{
  echo "GO-2026-6872 github.com/labstack/echo/v4 v4.16.3 $(date_plus 20) https://example.invalid/a"
  echo "GO-2026-6875 github.com/labstack/echo/v4 v4.16.3 $(date_plus 20)"
} >"$tmp/allow-4col.txt"
check_msg T11 2 'expected 5 columns' 'days away' "$tmp/allow-4col.txt" "$base"

# T12: allowlist expiry column is "never" -> 2
{
  echo "GO-2026-6872 github.com/labstack/echo/v4 v4.16.3 $(date_plus 20) https://example.invalid/a"
  echo "GO-2026-6875 github.com/labstack/echo/v4 v4.16.3 never https://example.invalid/b"
} >"$tmp/allow-never.txt"
check_msg T12 2 'is not YYYY-MM-DD' 'days away' "$tmp/allow-never.txt" "$base"

# T13: allowlist file missing -> 2
check_msg T13 2 'cannot read allowlist file' 'days away' "$tmp/does-not-exist.txt" "$base"

# [F37-24] T14: a fix exists (module-level finding, no function, fixed_version on the finding itself) -> the
# exception stops covering it. GO-2026-6875 has no fix in the same input and must still be ALLOWED.
jq -c 'if .finding and .finding.osv=="GO-2026-6872" and ((.finding.trace[0].function // "")=="") and ((.finding.trace[0].package // "")=="") then .finding.fixed_version="v4.16.9" else . end' "$base" >"$tmp/t14.jsonl"
check_msg T14 1 'GO-2026-6872 .*fix available \(v4\.16\.9\)' 'days away' "$allow_ok" "$tmp/t14.jsonl"
out_has T14-unfixed-still-allowed 'ALLOWED GO-2026-6875'

# T14b: a pseudo-version fixed_version is reported as a fix, never compared (so no rc 2 from the vX.Y.Z check)
jq -c 'if .finding and .finding.osv=="GO-2026-6872" and ((.finding.trace[0].function // "")=="") and ((.finding.trace[0].package // "")=="") then .finding.fixed_version="v4.16.4-0.20261201000000-abcdef123456" else . end' "$base" >"$tmp/t14b.jsonl"
check_msg T14b 1 'fix available \(v4\.16\.4-0\.20261201000000-abcdef123456\)' 'not vX\.Y\.Z|days away' "$allow_ok" "$tmp/t14b.jsonl"

# [F37-24] T15: protocol_version must be v1.x.y; anything else (or nothing) fails closed
jq -c 'if .config then .config.protocol_version="v2.0.0" else . end' "$base" >"$tmp/t15a.jsonl"
check_msg T15a 2 'protocol_version' 'days away' "$allow_ok" "$tmp/t15a.jsonl"
jq -c 'if .config then del(.config.protocol_version) else . end' "$base" >"$tmp/t15b.jsonl"
check_msg T15b 2 'protocol_version' 'days away' "$allow_ok" "$tmp/t15b.jsonl"
jq -c 'if .config then .config.protocol_version="" else . end' "$base" >"$tmp/t15c.jsonl"
check_msg T15c 2 'protocol_version' 'days away' "$allow_ok" "$tmp/t15c.jsonl"
jq -c 'if .config then .config.protocol_version="v1.3.0" else . end' "$base" >"$tmp/t15d.jsonl"
# real-min.jsonl is a trimmed capture of a real govulncheck v1.9.0 run (config v1.0.0 + its one finding)
check_msg T15d 0 'VERDICT: PASS' 'protocol_version' "$allow_ok" "$tmp/t15d.jsonl" "$fx/real-min.jsonl"

# [F37-24] T16: expiry more than 30 days from today (UTC) -> 2; the boundary day itself is accepted
check_msg T16a 2 'more than 30 days away' '' "$fx/allow-too-far.txt" "$base"
mk_allow "$tmp/allow-d30.txt" "$(date_plus 30)"
mk_allow "$tmp/allow-d31.txt" "$(date_plus 31)"
check_msg T16b-30 0 'VERDICT: PASS' 'days away' "$tmp/allow-d30.txt" "$base"
check_msg T16b-31 2 'more than 30 days away' '' "$tmp/allow-d31.txt" "$base"

# T16c: pin the value 30. The same today+31 allowlist must be accepted by a gate whose cap constant is 31, so
# T16b-31 above goes red if the cap is ever raised. An unchanged copy (mutation not applied) fails this case.
sed 's/^max_days=30$/max_days=31/' "$gate" >"$tmp/gate31.sh"
if cmp -s "$gate" "$tmp/gate31.sh"; then
  echo "FAIL T16c"
  echo "  T16c: gate has no 'max_days=30' line to mutate" >&2
  fails=$((fails + 1))
else
  rc31=0
  bash "$tmp/gate31.sh" "$base" "$tmp/allow-d31.txt" >"$tmp/out31.txt" 2>&1 || rc31=$?
  if [ "$rc31" = 0 ]; then
    echo "PASS T16c"
  else
    echo "FAIL T16c"
    echo "  T16c: cap 31 gate should accept today+31, got rc=$rc31" >&2
    fails=$((fails + 1))
  fi
fi

if [ "$fails" -ne 0 ]; then
  echo "SELFTEST FAIL: $fails case(s)"
  exit 1
fi
echo "SELFTEST OK"
