#!/usr/bin/env bash
#
# check-readme-keys.sh checks two things about README.md for CI:
#
#   test_vc2_annotation_keys_match_source: every "cpu.azalio.net/..."
#   public annotation key mentioned in README.md is exactly one of the two
#   user-facing keys built from internal/annotations/keys.go, and both keys
#   actually appear in README.md. Internal state keys are intentionally not
#   documented as a workload interface. This is a drift check: a
#   typo'd key, a stale key left behind after a rename, or a missing key
#   all fail it.
#
#   test_vc3_no_forbidden_claims: README.md contains none of a fixed list
#   of unmeasured claims -- a numeric latency/delay threshold, an effect
#   of cpu.max.burst on tail latency (p99), or a node/cost savings claim.
#   None of these have been measured on this project; see the spec's Out
#   of Scope section.
#
# Both checks are written to catch a violation, not just confirm the
# expected content is present -- see the injection tests this script's
# own commit was validated against (copies of README.md with a bad key or
# a forbidden phrase added, both under a scratch directory, never under
# this repo).
#
# Requires: bash, grep, sed (all POSIX-adjacent, no GNU-only flags).

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"

readme_file="${1:-${repo_root}/README.md}"
keys_file="${2:-${repo_root}/internal/annotations/keys.go}"

failures=0

fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}

die() {
  echo "ERROR: $*" >&2
  exit 2
}

[[ -r "${readme_file}" ]] || die "cannot read README at ${readme_file}"
[[ -r "${keys_file}" ]] || die "cannot read annotation keys source at ${keys_file}"

# --- parse the canonical keys out of internal/annotations/keys.go ----------
#
# keys.go defines domainPrefix once and every annotation key as
# `domainPrefix + "<suffix>"`. Parsing this as data (rather than, say,
# compiling and running a Go snippet) keeps this script dependency-free and
# keeps its failure mode legible: if keys.go's shape changes, parsing comes
# up empty and this script dies loudly instead of silently checking nothing.

domain_prefix=""
tier_key_suffix=""
burst_key_suffix=""
tier_value_idle=""

while IFS= read -r line; do
  if [[ "${line}" =~ domainPrefix[[:space:]]*=[[:space:]]*\"([^\"]*)\" ]]; then
    domain_prefix="${BASH_REMATCH[1]}"
  elif [[ "${line}" =~ TierKey[[:space:]]*=[[:space:]]*domainPrefix[[:space:]]*\+[[:space:]]*\"([^\"]*)\" ]]; then
    tier_key_suffix="${BASH_REMATCH[1]}"
  elif [[ "${line}" =~ BurstKey[[:space:]]*=[[:space:]]*domainPrefix[[:space:]]*\+[[:space:]]*\"([^\"]*)\" ]]; then
    burst_key_suffix="${BASH_REMATCH[1]}"
  elif [[ "${line}" =~ TierValueIdle[[:space:]]*=[[:space:]]*\"([^\"]*)\" ]]; then
    tier_value_idle="${BASH_REMATCH[1]}"
  fi
done <"${keys_file}"

[[ -n "${domain_prefix}" ]] || die "could not parse domainPrefix out of ${keys_file}; keys.go's format changed and this script needs updating, not silently skipping the check"
[[ -n "${tier_key_suffix}" ]] || die "could not parse TierKey out of ${keys_file}"
[[ -n "${burst_key_suffix}" ]] || die "could not parse BurstKey out of ${keys_file}"
[[ -n "${tier_value_idle}" ]] || die "could not parse TierValueIdle out of ${keys_file}"

tier_key="${domain_prefix}${tier_key_suffix}"
burst_key="${domain_prefix}${burst_key_suffix}"

# --- VC2: annotation keys in README.md match the source of truth ----------

test_vc2_annotation_keys_match_source() {
  local readme_keys key known found_tier found_burst

  found_tier=0
  found_burst=0

  # Every "cpu.azalio.net/<suffix>"-shaped token mentioned anywhere in
  # README.md, deduplicated. grep -o prints each match on its own line.
  readme_keys="$(grep -oE "${domain_prefix}[A-Za-z0-9_-]+" "${readme_file}" | sort -u || true)"

  if [[ -z "${readme_keys}" ]]; then
    fail "VC2: README.md mentions no ${domain_prefix}* annotation key at all"
    return
  fi

  while IFS= read -r key; do
    [[ -n "${key}" ]] || continue
    known=0
    # kubectl annotate removes an annotation by appending "-" to its key.
    # README's uninstall recipe therefore legitimately contains both
    # canonical keys in that command form as well as in ordinary prose.
    if [[ "${key}" == "${tier_key}" || "${key}" == "${tier_key}-" ]]; then
      known=1
      found_tier=1
    fi
    if [[ "${key}" == "${burst_key}" || "${key}" == "${burst_key}-" ]]; then
      known=1
      found_burst=1
    fi
    if [[ "${known}" -eq 0 ]]; then
      fail "VC2: README.md mentions annotation key '${key}', which matches neither TierKey ('${tier_key}') nor BurstKey ('${burst_key}') in ${keys_file}"
    fi
  done <<<"${readme_keys}"

  [[ "${found_tier}" -eq 1 ]] || fail "VC2: README.md never mentions the tier key '${tier_key}' from ${keys_file}"
  [[ "${found_burst}" -eq 1 ]] || fail "VC2: README.md never mentions the burst key '${burst_key}' from ${keys_file}"

  if ! grep -qE "\"${tier_value_idle}\"|\`${tier_value_idle}\`|: ${tier_value_idle}\b" "${readme_file}"; then
    fail "VC2: README.md never mentions TierValueIdle ('${tier_value_idle}') from ${keys_file}"
  fi
}

# --- VC3: README.md contains none of the forbidden, unmeasured claims -----

test_vc3_no_forbidden_claims() {
  local flat

  # Flatten to a single line so a forbidden phrase split across a
  # soft-wrapped markdown paragraph (two source lines, one rendered
  # sentence) is still caught by proximity-based matching below.
  flat="$(tr '\n' ' ' <"${readme_file}" | tr -s '[:space:]' ' ')"

  local time_num_re latency_kw_re
  time_num_re='[0-9]+(\.[0-9]+)?[[:space:]]*(ms|milliseconds?|secs?|seconds?|mins?|minutes?)'
  latency_kw_re='(latency|delay|applies within|application (delay|latency)|\bsla\b|appl(y|ies|ying) the tier|tier appl)'

  if grep -Eqi "(${time_num_re})[^.]{0,80}(${latency_kw_re})" <<<"${flat}" \
    || grep -Eqi "(${latency_kw_re})[^.]{0,80}(${time_num_re})" <<<"${flat}"; then
    fail "VC3: README.md appears to state a numeric latency/delay threshold for tier application, which has never been measured on this project"
  fi

  if grep -Eqi 'p[[:space:]-]?99|99th[[:space:]]+percentile' <<<"${flat}"; then
    fail "VC3: README.md mentions p99 / 99th percentile; cpu.max.burst's effect on tail latency has never been measured on this project"
  fi

  local cost_re
  cost_re='cost saving|cost-saving|sav(e|es|ing) (on )?cost|sav(e|es|ing) money|cheaper|reduc(e|es|ing) (your |the |cloud )?bill|fewer nodes|reduc(e|es|ing) node count|node saving|sav(e|es|ing) nodes|return on investment|\bROI\b'
  if grep -Eqi "${cost_re}" <<<"${flat}"; then
    fail "VC3: README.md appears to claim a node-count or cost saving, which this project does not measure or claim"
  fi
}

test_vc2_annotation_keys_match_source
test_vc3_no_forbidden_claims

if [[ "${failures}" -gt 0 ]]; then
  echo "check-readme-keys: ${failures} check(s) failed" >&2
  exit 1
fi

echo "check-readme-keys: all checks passed"
