#!/usr/bin/env bash
# [F39-05][F39-06] Self-test (positive controls) for the "Secret scan" step of .github/workflows/ci.yml.
# Pulls the step's run block out of ci.yml itself (the same text CI runs, never a hand copy), runs it against
# throwaway repos and compares exit codes. One "PASS <case>" / "FAIL <case>" line per case; exits 1 if any fails.
#
# usage: secret-scan_test.sh            (needs git, jq, python3 and gitleaks at $(go env GOPATH)/bin/gitleaks)
#        GITLEAKS=/path/to/gitleaks secret-scan_test.sh
set -uo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ci="${CI_FILE:-$here/../../.github/workflows/ci.yml}"
gl="${GITLEAKS:-$(go env GOPATH)/bin/gitleaks}"
for tool in git jq python3; do
  command -v "$tool" >/dev/null 2>&1 || { echo "FAIL setup: $tool not found"; exit 1; }
done
[ -x "$gl" ] || { echo "FAIL setup: gitleaks not found at $gl"; exit 1; }

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
# The block calls "$(go env GOPATH)/bin/gitleaks": point GOPATH at a directory whose bin/ holds the binary under test.
mkdir -p "$tmp/gopath/bin" && ln -s "$gl" "$tmp/gopath/bin/gitleaks"

# Run block = the lines after "run: |" of the "- name: Secret scan" step, de-indented by 10 spaces.
awk '
  /^      - name: Secret scan$/ { step = 1; next }
  step && /^        run: \|$/ { inrun = 1; next }
  inrun && /^          / { sub(/^          /, ""); print; next }
  inrun && /^ *$/ { print ""; next }
  inrun { exit }
' "$ci" >"$tmp/scan.sh"
# [F39-15] each variable on its own: a bare substring match would also accept "unset GITLEAKS_CONFIG_TOML" alone.
if ! grep -q 'GIT_ATTR_SOURCE' "$tmp/scan.sh" \
  || ! grep -Eq 'unset( [A-Z_]+)* GITLEAKS_CONFIG( |$)' "$tmp/scan.sh" \
  || ! grep -Eq 'unset( [A-Z_]+)* GITLEAKS_CONFIG_TOML( |$)' "$tmp/scan.sh"; then
  echo "FAIL extract: no Secret scan run block with the [F39-05] guards found in $ci"
  exit 1
fi
# Same block with the GIT_ATTR_SOURCE probe broken, as an older git that ignores the variable would behave.
# shellcheck disable=SC2016 # the pattern matches the literal text "$(GIT_ATTR_SOURCE=$empty_tree", nothing to expand
sed 's/\[ "$(GIT_ATTR_SOURCE=$empty_tree git -C/[ "$(GIT_ATTR_SOURC=$empty_tree git -C/' "$tmp/scan.sh" >"$tmp/scan-oldgit.sh"
if cmp -s "$tmp/scan.sh" "$tmp/scan-oldgit.sh"; then
  echo "FAIL extract: GIT_ATTR_SOURCE probe line not found, cannot build the old-git control"
  exit 1
fi

# [F40-09][F40-10] install-isolation: both go install steps must run with a fresh GOMODCACHE and GOCACHE under
# $RUNNER_TEMP, so a cache restored by setup-go cannot decide which binary gets built. Only the step's own "run:" line
# is read (anchored on the exact "- name:" line, "#" comment lines never count), and each variable must appear.
for step in gitleaks govulncheck; do
  awk -v anchor="      - name: Install $step" '
    $0 == anchor { step = 1; next }
    step && /^      - name:/ { exit }
    step && /^        run: / { print }
  ' "$ci" >"$tmp/install-$step.txt"
  # shellcheck disable=SC2016 # the patterns are the literal text GOMODCACHE="$RUNNER_TEMP, nothing to expand
  if ! grep -Fq 'GOMODCACHE="$RUNNER_TEMP' "$tmp/install-$step.txt" \
    || ! grep -Fq 'GOCACHE="$RUNNER_TEMP' "$tmp/install-$step.txt"; then
    echo "FAIL install-isolation: the Install $step step in $ci lacks GOMODCACHE=\"\$RUNNER_TEMP and GOCACHE=\"\$RUNNER_TEMP on its run line"
    exit 1
  fi
done

# [SE41-01][F42-01][F39-17] scan-before-restore: setup-go restores caches with tar -P, which can write anywhere the
# runner can, and a repo script that runs before or alongside the scan can hide files from it. So the check job's
# first six steps must be exactly checkout, setup-go ("cache: false" under its own with:), Install gitleaks, Secret
# scan, Install govulncheck, Govulncheck, each with only the keys it has now (adding background:, if:,
# continue-on-error:, shell: or env: turns this red); the two install run lines must be the bare go install; the job
# has one setup-go; and neither the workflow nor the job sets env: or defaults: (BASH_ENV or defaults.run.shell would
# run repo code inside these steps); no line of the first five steps names scripts/ (a guarded call to a repo script
# would be skipped by the Secret scan extraction test above, whose throwaway repos do not have it). "#" comment lines
# never count.
if ! awk '
  /^(env|defaults):/ { bad = 1 }
  !done && /^  check:$/ { job = 1; next }
  job && /^  [^ #]/ { job = 0; done = 1 }
  !job || /^ *#/ { next }
  /^    (env|defaults):/ { bad = 1 }
  /uses: actions\/setup-go@/ { sg++ }
  /^      - / { n++; head[n] = $0; inwith = 0; inenv = 0; k = $0; sub(/^      - /, "", k); sub(/:.*/, "", k); keys[n] = k }
  n <= 5 && /scripts\// { bad = 1 }
  /^        [^ ]/ {
    inwith = (n == 2 && $0 == "        with:")
    inenv = (n == 4 && $0 == "        env:")
    k = $0; sub(/^ +/, "", k); sub(/:.*/, "", k); keys[n] = keys[n] " " k
    if (n == 3 || n == 5) run[n] = $0
  }
  inwith && /^          cache: false *$/ { cache = 1 }
  inenv && /^          [^ ]/ { k = $0; sub(/^ +/, "", k); sub(/:.*/, "", k); envk = envk " " k }
  END {
    want[1] = "      - uses: actions/checkout@"; want[2] = "      - uses: actions/setup-go@"
    want[3] = "      - name: Install gitleaks"; want[4] = "      - name: Secret scan"
    want[5] = "      - name: Install govulncheck"; want[6] = "      - name: Govulncheck"
    wk[1] = "uses with"; wk[2] = "uses with"; wk[3] = "name run"; wk[4] = "name env run"; wk[5] = "name run"; wk[6] = "name run"
    ok = cache && n > 6 && !bad && sg == 1 && envk == " EVENT_NAME PR_BASE PR_HEAD PUSH_BEFORE"
    if (run[3] !~ /^        run: GOMODCACHE="\$RUNNER_TEMP\/[a-z-]+" GOCACHE="\$RUNNER_TEMP\/[a-z-]+" go install github\.com\/zricethezav\/gitleaks\/v8@v[0-9.]+$/) ok = 0
    if (run[5] !~ /^        run: GOMODCACHE="\$RUNNER_TEMP\/[a-z-]+" GOCACHE="\$RUNNER_TEMP\/[a-z-]+" go install golang\.org\/x\/vuln\/cmd\/govulncheck@v[0-9.]+$/) ok = 0
    for (i = 1; i <= 6; i++) {
      if (i <= 2 && index(head[i], want[i]) != 1) ok = 0
      if (i > 2 && head[i] != want[i]) ok = 0
      if (keys[i] != wk[i]) ok = 0
    }
    exit !ok
  }
' "$ci"; then
  echo "FAIL scan-before-restore: in $ci the check job must start with checkout, setup-go (cache: false under with:), Install gitleaks, Secret scan, Install govulncheck, Govulncheck, with no extra step keys, bare install run lines, one setup-go, no workflow/job env: or defaults:, and no scripts/ anywhere in the job before Govulncheck"
  exit 1
fi

fails=0
token="$(python3 -c 'import secrets,string;a=string.ascii_letters+string.digits;print("ghp"+"_"+"".join(secrets.choice(a) for _ in range(36)))')"

# make_repo <dir> <case>: base commit, then the PR commits the case needs; prints "<base> <head>".
make_repo() {
  local dir="$1" case="$2" base fp
  git init -q -b main "$dir"
  git -C "$dir" config user.email t@example.invalid
  git -C "$dir" config user.name t
  echo hi >"$dir/README"
  git -C "$dir" add README
  if [ "$case" = basehasignore ]; then
    echo '0000000000000000000000000000000000000000:old.txt:generic-api-key:1' >"$dir/.gitleaksignore"
    git -C "$dir" add .gitleaksignore
  fi
  git -C "$dir" commit -qm base
  base="$(git -C "$dir" rev-parse HEAD)"
  if [ "$case" = attr ]; then
    printf 'secret.txt -diff\n' >"$dir/.gitattributes"
    git -C "$dir" add .gitattributes && git -C "$dir" commit -qm attrs
  fi
  if [ "$case" = clean ]; then
    echo ok >"$dir/notes.txt" && git -C "$dir" add notes.txt && git -C "$dir" commit -qm notes
  else
    printf 'token = "%s"\n' "$token" >"$dir/secret.txt" && git -C "$dir" add secret.txt && git -C "$dir" commit -qm secret
  fi
  case "$case" in ignore | basehasignore | pushignore)
    fp="$(cd "$dir" && "$gl" git --no-banner --report-format json --report-path - --log-opts="--no-merges $base..HEAD" . 2>/dev/null | jq -r '.[0].Fingerprint')"
    echo "$fp" >>"$dir/.gitleaksignore"
    git -C "$dir" add .gitleaksignore && git -C "$dir" commit -qm allow-self
    ;;
  esac
  echo "$base $(git -C "$dir" rev-parse HEAD)"
}

# check <name> <want-rc> <script> <case>
check() {
  local name="$1" want="$2" script="$3" case="$4" dir base head got=0
  dir="$tmp/repo-$name"
  read -r base head < <(make_repo "$dir" "$case")
  mkdir -p "$tmp/runner-$name"
  (
    cd "$dir" || exit 9
    export GOPATH="$tmp/gopath" RUNNER_TEMP="$tmp/runner-$name" GITHUB_SHA="$head" GITHUB_REF_NAME=feature
    export PR_BASE="$base" PR_HEAD="$head"
    if [ "$case" = pushignore ]; then export EVENT_NAME=push PUSH_BEFORE="$base"; else export EVENT_NAME=pull_request PUSH_BEFORE=; fi
    if [ "$case" = config ]; then export GITLEAKS_CONFIG_TOML=$'[allowlist]\npaths = [\'\'\'.*\'\'\']\n'; fi
    if [ "$case" = configpath ]; then
      printf "[allowlist]\npaths = ['''.*''']\n" >"$tmp/allowall-$name.toml"
      export GITLEAKS_CONFIG="$tmp/allowall-$name.toml"
    fi
    bash "$script"
  ) >"$tmp/out-$name.txt" 2>&1 || got=$?
  if [ "$got" = "$want" ]; then
    echo "PASS $name"
  else
    echo "FAIL $name"
    echo "  $name: want rc=$want got rc=$got; last lines: $(tail -n 3 "$tmp/out-$name.txt" | tr '\n' ' ')" >&2
    fails=$((fails + 1))
  fi
}

check clean 0 "$tmp/scan.sh" clean                 # a clean PR passes
check plain 1 "$tmp/scan.sh" plain                 # a leaked token fails
check attr 1 "$tmp/scan.sh" attr                   # [F39-05] PR .gitattributes "-diff" must not hide the file
check config 1 "$tmp/scan.sh" config               # [F39-05] GITLEAKS_CONFIG_TOML from an earlier step must not replace the rules
check configpath 1 "$tmp/scan.sh" configpath       # [F39-15] GITLEAKS_CONFIG (a config file path) must not replace the rules either
check ignore 1 "$tmp/scan.sh" ignore               # [F39-06] a PR may not allow its own finding through .gitleaksignore
check basehasignore 1 "$tmp/scan.sh" basehasignore # [F39-06] same, when the base branch already has a .gitleaksignore
check pushignore 0 "$tmp/scan.sh" pushignore       # [F39-06] on push (after review) the committed .gitleaksignore still counts
check oldgit 1 "$tmp/scan-oldgit.sh" clean         # [F39-05] a git that ignores GIT_ATTR_SOURCE fails closed
if [ "$fails" = 0 ] && ! grep -q '::error::this git ignores GIT_ATTR_SOURCE' "$tmp/out-oldgit.txt"; then
  echo "FAIL oldgit-reason"
  echo "  oldgit: red for another reason, the GIT_ATTR_SOURCE error line is missing" >&2
  fails=$((fails + 1))
fi

if [ "$fails" -ne 0 ]; then
  echo "SELFTEST FAIL: $fails case(s)"
  exit 1
fi
echo "SELFTEST OK"
