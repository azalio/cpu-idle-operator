#!/usr/bin/env bash
#
# check-manifests.sh renders config/base with kustomize and enforces two
# hard constraints on the output. Both checks are written to catch excess
# privilege, not just confirm that the required fields are present: a
# manifest with privileged: true dropped in on top of an otherwise correct
# DaemonSet, or a ClusterRole with one extra verb, must fail here.
#
#   test_vc1_no_privileged_no_sysadmin (VC1, HC-6): the DaemonSet never
#   sets privileged: true and never requests SYS_ADMIN, and does carry the
#   hardening it is supposed to: runAsUser: 0, capabilities.drop: [ALL],
#   allowPrivilegeEscalation: false, and a hostPath on /sys/fs/cgroup
#   mounted with readOnly: false.
#
#   test_vc2_rbac_is_minimal (VC2): the ClusterRole grants exactly
#   get/list/watch on pods and create/patch on events -- nothing more.
#
# Requires: kustomize, yq (mikefarah/yq, v4 expression syntax).

set -euo pipefail

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
base_dir="${repo_root}/config/base"

failures=0

fail() {
  echo "FAIL: $*" >&2
  failures=$((failures + 1))
}

require_cmd() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "ERROR: required tool not found on PATH: $1" >&2
    exit 2
  fi
}

require_cmd kustomize
require_cmd yq

rendered_file="$(mktemp)"
trap 'rm -f "${rendered_file}"' EXIT

kustomize build "${base_dir}" >"${rendered_file}"

test_vc1_no_privileged_no_sysadmin() {
  # Global negative checks: these two strings must not appear anywhere in
  # the rendered output, regardless of which object or field hides them.
  if grep -Eq 'privileged:[[:space:]]*true' "${rendered_file}"; then
    fail "VC1: rendered manifest sets privileged: true"
  fi
  if grep -q 'SYS_ADMIN' "${rendered_file}"; then
    fail "VC1: rendered manifest requests the SYS_ADMIN capability"
  fi

  local container_count
  container_count=$(yq eval-all 'select(.kind == "DaemonSet") | .spec.template.spec.containers | length' "${rendered_file}")
  if [[ "${container_count}" -lt 1 ]]; then
    fail "VC1: no DaemonSet with at least one container found"
    return
  fi

  local run_as_user allow_priv_esc readonly_rootfs drop_caps
  run_as_user=$(yq eval-all 'select(.kind == "DaemonSet") | .spec.template.spec.containers[0].securityContext.runAsUser' "${rendered_file}")
  allow_priv_esc=$(yq eval-all 'select(.kind == "DaemonSet") | .spec.template.spec.containers[0].securityContext.allowPrivilegeEscalation' "${rendered_file}")
  readonly_rootfs=$(yq eval-all 'select(.kind == "DaemonSet") | .spec.template.spec.containers[0].securityContext.readOnlyRootFilesystem' "${rendered_file}")
  drop_caps=$(yq eval-all -o=json -I=0 'select(.kind == "DaemonSet") | .spec.template.spec.containers[0].securityContext.capabilities.drop' "${rendered_file}")

  [[ "${run_as_user}" == "0" ]] || fail "VC1: DaemonSet container securityContext.runAsUser is not 0 (got '${run_as_user}')"
  [[ "${allow_priv_esc}" == "false" ]] || fail "VC1: DaemonSet container securityContext.allowPrivilegeEscalation is not false (got '${allow_priv_esc}')"
  [[ "${readonly_rootfs}" == "true" ]] || fail "VC1: DaemonSet container securityContext.readOnlyRootFilesystem is not true (got '${readonly_rootfs}')"
  [[ "${drop_caps}" == '["ALL"]' ]] || fail "VC1: DaemonSet container securityContext.capabilities.drop is not exactly [ALL] (got '${drop_caps}')"

  local host_network host_pid host_ipc
  host_network=$(yq eval-all 'select(.kind == "DaemonSet") | .spec.template.spec.hostNetwork // false' "${rendered_file}")
  host_pid=$(yq eval-all 'select(.kind == "DaemonSet") | .spec.template.spec.hostPID // false' "${rendered_file}")
  host_ipc=$(yq eval-all 'select(.kind == "DaemonSet") | .spec.template.spec.hostIPC // false' "${rendered_file}")

  [[ "${host_network}" == "false" ]] || fail "VC1: DaemonSet sets hostNetwork: true"
  [[ "${host_pid}" == "false" ]] || fail "VC1: DaemonSet sets hostPID: true"
  [[ "${host_ipc}" == "false" ]] || fail "VC1: DaemonSet sets hostIPC: true"

  local cgroup_hostpath cgroup_readonly
  cgroup_hostpath=$(yq eval-all 'select(.kind == "DaemonSet") | .spec.template.spec.volumes[] | select(.hostPath.path == "/sys/fs/cgroup") | .hostPath.path' "${rendered_file}")
  if [[ -z "${cgroup_hostpath}" ]]; then
    fail "VC1: no hostPath volume for /sys/fs/cgroup found"
  fi

  cgroup_readonly=$(yq eval-all 'select(.kind == "DaemonSet") | .spec.template.spec.containers[0].volumeMounts[] | select(.mountPath == "/sys/fs/cgroup") | .readOnly' "${rendered_file}")
  [[ "${cgroup_readonly}" == "false" ]] || fail "VC1: /sys/fs/cgroup volumeMount is not explicitly readOnly: false (got '${cgroup_readonly}')"
}

test_vc2_rbac_is_minimal() {
  local rule_count
  rule_count=$(yq eval-all 'select(.kind == "ClusterRole") | .rules | length' "${rendered_file}")
  if [[ "${rule_count}" != "2" ]]; then
    fail "VC2: ClusterRole has ${rule_count} rule(s), expected exactly 2 (pods + events)"
    return
  fi

  local pods_api_groups pods_verbs events_api_groups events_verbs
  pods_api_groups=$(yq eval-all -o=json -I=0 'select(.kind == "ClusterRole") | .rules[] | select(.resources[0] == "pods" and (.resources | length) == 1) | .apiGroups' "${rendered_file}")
  pods_verbs=$(yq eval-all -o=json -I=0 'select(.kind == "ClusterRole") | .rules[] | select(.resources[0] == "pods" and (.resources | length) == 1) | .verbs | sort' "${rendered_file}")
  events_api_groups=$(yq eval-all -o=json -I=0 'select(.kind == "ClusterRole") | .rules[] | select(.resources[0] == "events" and (.resources | length) == 1) | .apiGroups' "${rendered_file}")
  events_verbs=$(yq eval-all -o=json -I=0 'select(.kind == "ClusterRole") | .rules[] | select(.resources[0] == "events" and (.resources | length) == 1) | .verbs | sort' "${rendered_file}")

  [[ -n "${pods_api_groups}" ]] || fail "VC2: no ClusterRole rule found for resource 'pods' alone"
  [[ "${pods_api_groups}" == '[""]' ]] || fail "VC2: pods rule apiGroups is not exactly [\"\"] (got '${pods_api_groups}')"
  [[ "${pods_verbs}" == '["get","list","watch"]' ]] || fail "VC2: pods rule verbs are not exactly [get,list,watch] (got '${pods_verbs}')"

  [[ -n "${events_api_groups}" ]] || fail "VC2: no ClusterRole rule found for resource 'events' alone"
  [[ "${events_api_groups}" == '[""]' ]] || fail "VC2: events rule apiGroups is not exactly [\"\"] (got '${events_api_groups}')"
  [[ "${events_verbs}" == '["create","patch"]' ]] || fail "VC2: events rule verbs are not exactly [create,patch] (got '${events_verbs}')"

  local other_rules
  other_rules=$(yq eval-all -o=json -I=0 'select(.kind == "ClusterRole") | [.rules[] | select(((.resources[0] == "pods" and (.resources | length) == 1) or (.resources[0] == "events" and (.resources | length) == 1)) | not)]' "${rendered_file}")
  if [[ "${other_rules}" != "[]" ]]; then
    fail "VC2: ClusterRole grants an unexpected rule beyond pods/events: ${other_rules}"
  fi
}

test_vc1_no_privileged_no_sysadmin
test_vc2_rbac_is_minimal

if [[ "${failures}" -gt 0 ]]; then
  echo "check-manifests: ${failures} check(s) failed" >&2
  exit 1
fi

echo "check-manifests: all checks passed"
