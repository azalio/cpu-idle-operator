#!/usr/bin/env bash

set -euo pipefail

if [[ "$#" -ne 1 ]]; then
  echo "usage: $0 OUTPUT_DIR" >&2
  exit 2
fi

script_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repo_root="$(cd "${script_dir}/.." && pwd)"
output_dir="$1"
chart_dir="${repo_root}/deploy/helm/cpu-idle-operator"

if [[ ! -d "${output_dir}" ]]; then
  echo "ERROR: output directory does not exist: ${output_dir}" >&2
  exit 2
fi
if ! command -v helm >/dev/null 2>&1; then
  echo "ERROR: helm is required" >&2
  exit 2
fi

render_template() {
  local template="$1"

  helm template cpu-idle-operator "${chart_dir}" \
    --namespace cpu-idle-system \
    --show-only "templates/${template}" \
    | grep -v '^# Source:' \
    | awk 'NR==1 && /^---$/{next} {print}' \
    | cat -s \
    | awk '{lines[NR]=$0} END{last=NR; while (last>0 && lines[last] ~ /^[[:space:]]*$/) last--; for(i=1;i<=last;i++) print lines[i]}' \
    >"${output_dir}/${template}"
}

render_template namespace.yaml
render_template rbac.yaml
render_template daemonset.yaml
