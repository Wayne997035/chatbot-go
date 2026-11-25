#!/usr/bin/env bash
# [F241-01] govulncheck gate: reads `govulncheck -format json` output and an
# allowlist, blocks every *called* (symbol level) vulnerability that the
# allowlist does not explicitly cover.
#
# usage: govulncheck-gate.sh <govulncheck.json> <allowlist.txt>
#
# exit 0  VERDICT: PASS  nothing called outside the allowlist
# exit 1  VERDICT: FAIL  at least one BLOCKED finding
# exit 2  VERDICT: FAIL  fail-closed: missing/broken input, wrong scan
#                        level/mode, config protocol_version not v1.x.y,
#                        malformed allowlist or version/date, expiry more than
#                        max_days (30) from today
#
# [F37-24] An allowlist row stops covering a called finding once any finding for
# the same osv+module carries a fixed_version (BLOCKED, rc 1).
#
# Allowlist rows (whitespace separated, 5 columns, '#' starts a comment line):
#   <osv> <module> <min-version vX.Y.Z> <expiry YYYY-MM-DD> <reason URL>
set -euo pipefail

fail() {
  echo "ERROR: $*"
  echo "VERDICT: FAIL"
  exit 2
}

[ "$#" -eq 2 ] || fail "usage: govulncheck-gate.sh <govulncheck.json> <allowlist.txt>"
json="$1"
allow="$2"

command -v jq >/dev/null 2>&1 || fail "jq not found"
[ -f "$json" ] && [ -r "$json" ] || fail "cannot read json file: $json"
[ -f "$allow" ] && [ -r "$allow" ] || fail "cannot read allowlist file: $allow"

semver_re='^v[0-9]+\.[0-9]+\.[0-9]+$'
date_re='^[0-9]{4}-[0-9]{2}-[0-9]{2}$'

# [F37-24] an exception may not expire further out than this many days from today (UTC); past dates are not capped.
max_days=30
max_expiry="$(jq -nr --argjson d "$max_days" 'now + $d * 86400 | strftime("%Y-%m-%d")')" || fail "cannot compute max expiry date"
[[ "$max_expiry" =~ $date_re ]] || fail "cannot compute max expiry date"

# --- allowlist ---------------------------------------------------------
a_osv=()
a_mod=()
a_min=()
a_exp=()
n_allow=0
lineno=0
while IFS= read -r line || [ -n "$line" ]; do
  lineno=$((lineno + 1))
  trimmed="${line#"${line%%[![:space:]]*}"}"
  case "$trimmed" in
    '' | '#'*) continue ;;
  esac
  read -r -a cols <<<"$trimmed"
  [ "${#cols[@]}" -eq 5 ] || fail "allowlist line $lineno: expected 5 columns, got ${#cols[@]}"
  [[ "${cols[2]}" =~ $semver_re ]] || fail "allowlist line $lineno: min version '${cols[2]}' is not vX.Y.Z"
  [[ "${cols[3]}" =~ $date_re ]] || fail "allowlist line $lineno: expiry '${cols[3]}' is not YYYY-MM-DD"
  [ ! "${cols[3]}" \> "$max_expiry" ] || fail "allowlist line $lineno: expiry ${cols[3]} is more than $max_days days away (max $max_expiry)"
  a_osv+=("${cols[0]}")
  a_mod+=("${cols[1]}")
  a_min+=("${cols[2]}")
  a_exp+=("${cols[3]}")
  n_allow=$((n_allow + 1))
done <"$allow"

# --- govulncheck JSON stream ---------------------------------------------
if ! msgs="$(jq -c 'select(has("config") or has("finding"))' "$json")"; then
  fail "cannot parse govulncheck JSON stream: $json"
fi

if ! cfg="$(printf '%s\n' "$msgs" | jq -r 'select(has("config")) | "\(.config.protocol_version // "")|\(.config.scan_level // "")|\(.config.scan_mode // "")"')"; then
  fail "cannot read config message"
fi
[ -n "$cfg" ] || fail "no config message in govulncheck output"
# [F37-24] fail closed on an unknown protocol major: field names and meaning may have changed.
proto_re='^v1\.[0-9]+\.[0-9]+$'
while IFS='|' read -r proto level mode; do
  [[ "$proto" =~ $proto_re ]] || fail "config protocol_version is '$proto', need v1.x.y"
  [ "$level" = "symbol" ] || fail "config scan_level is '$level', need symbol"
  [ "$mode" = "source" ] || fail "config scan_mode is '$mode', need source"
done <<<"$cfg"

# A finding counts as "called" only when trace[0] has a function.
if ! called="$(printf '%s\n' "$msgs" | jq -r 'select(has("finding")) | .finding | select((.trace[0].function // "") != "") | "\(.osv // "")|\(.trace[0].module // "")|\(.trace[0].version // "")"' | sort -u)"; then
  fail "cannot read findings"
fi

# [F37-24] osv|module|fixed_version for every finding that carries a fix (any level, not only called ones).
# fixed_version sits on the finding itself, not inside trace; it may be a pseudo-version, so it is never compared.
if ! fixed="$(printf '%s\n' "$msgs" | jq -r 'select(has("finding")) | .finding | select((.fixed_version // "") != "") | "\(.osv // "")|\(.trace[0].module // "")|\(.fixed_version)"' | sort -u)"; then
  fail "cannot read fixed versions"
fi

today="$(date -u +%F)"
[[ "$today" =~ $date_re ]] || fail "cannot determine today's date"

blocked=0
seen=""

if [ -n "$called" ]; then
  while IFS='|' read -r osv mod ver; do
    seen="$seen
$osv|$mod|"
    has_osv=0
    has_mod=0
    ver_ok=0
    allowed_until=""
    i=0
    while [ "$i" -lt "$n_allow" ]; do
      if [ "${a_osv[$i]}" = "$osv" ]; then
        has_osv=1
        if [ "${a_mod[$i]}" = "$mod" ]; then
          has_mod=1
          [[ "$ver" =~ $semver_re ]] || fail "finding $osv $mod version '$ver' is not vX.Y.Z (prerelease/pseudo-version not allowed)"
          lowest="$(printf '%s\n%s\n' "${a_min[$i]#v}" "${ver#v}" | sort -V | sed -n 1p)"
          if [ "$lowest" = "${a_min[$i]#v}" ]; then
            ver_ok=1
            if [ ! "$today" \> "${a_exp[$i]}" ]; then
              allowed_until="${a_exp[$i]}"
            fi
          fi
        fi
      fi
      i=$((i + 1))
    done

    fix_ver=""
    if [ -n "$allowed_until" ] && [ -n "$fixed" ]; then
      fix_ver="$(printf '%s\n' "$fixed" | awk -F'|' -v o="$osv" -v m="$mod" '$1 == o && $2 == m { print $3; exit }')"
    fi

    if [ -n "$allowed_until" ] && [ -n "$fix_ver" ]; then
      echo "BLOCKED $osv $mod@$ver fix available ($fix_ver), allowlist entry no longer valid"
      blocked=$((blocked + 1))
    elif [ -n "$allowed_until" ]; then
      echo "ALLOWED $osv $mod@$ver until $allowed_until"
    elif [ "$has_osv" -eq 0 ]; then
      echo "BLOCKED $osv $mod@$ver not in allowlist"
      blocked=$((blocked + 1))
    elif [ "$has_mod" -eq 0 ]; then
      echo "BLOCKED $osv $mod@$ver module differs from allowlist"
      blocked=$((blocked + 1))
    elif [ "$ver_ok" -eq 0 ]; then
      echo "BLOCKED $osv $mod@$ver version below allowlist minimum"
      blocked=$((blocked + 1))
    else
      echo "BLOCKED $osv $mod@$ver allowlist entry expired"
      blocked=$((blocked + 1))
    fi
  done <<<"$called"
fi

i=0
while [ "$i" -lt "$n_allow" ]; do
  case "$seen" in
    *"
${a_osv[$i]}|${a_mod[$i]}|"*) ;;
    *) echo "NOTICE ${a_osv[$i]} not reported this run, can be removed from allowlist" ;;
  esac
  i=$((i + 1))
done

if [ "$blocked" -gt 0 ]; then
  echo "VERDICT: FAIL"
  exit 1
fi
echo "VERDICT: PASS"
exit 0
