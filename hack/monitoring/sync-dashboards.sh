#!/usr/bin/env bash
# Wrap every JSON file under hack/monitoring/dashboards/ into a single
# ConfigMap labelled grafana_dashboard=1, picked up by the Grafana sidecar.
set -euo pipefail

NAMESPACE="${MONITORING_NAMESPACE:-monitoring}"
NAME="${DASHBOARDS_CM_NAME:-saunafs-operator-dashboards}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DASH_DIR="${SCRIPT_DIR}/dashboards"

EXPECTED_CONTEXT="${KUBE_CONTEXT:-kind-saunafs-operator}"
if ! kubectl config get-contexts -o name | grep -qx "${EXPECTED_CONTEXT}"; then
  echo "ERROR: kube context '${EXPECTED_CONTEXT}' not found." >&2
  exit 1
fi
KCTL=(kubectl --context "${EXPECTED_CONTEXT}")

if [[ ! -d "${DASH_DIR}" ]] || [[ -z "$(ls -A "${DASH_DIR}"/*.json 2>/dev/null || true)" ]]; then
  echo ">> No dashboards in ${DASH_DIR}, skipping ConfigMap creation."
  exit 0
fi

# Use --dry-run | apply to make it idempotent.
"${KCTL[@]}" create configmap "${NAME}" \
  --namespace "${NAMESPACE}" \
  $(for f in "${DASH_DIR}"/*.json; do printf -- '--from-file=%s ' "$f"; done) \
  --dry-run=client -o yaml | \
  "${KCTL[@]}" label --local -f - --dry-run=client -o yaml \
    grafana_dashboard=1 \
    app.kubernetes.io/part-of=saunafs-operator | \
  "${KCTL[@]}" apply -f -

echo ">> Dashboards ConfigMap '${NAME}' applied in namespace '${NAMESPACE}'."
