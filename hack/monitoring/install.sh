#!/usr/bin/env bash
# Install kube-prometheus-stack into the current kube context for local
# development. Idempotent: safe to re-run to upgrade values.
set -euo pipefail

NAMESPACE="${MONITORING_NAMESPACE:-monitoring}"
RELEASE="${MONITORING_RELEASE:-kube-prom-stack}"
CHART_VERSION="${MONITORING_CHART_VERSION:-65.5.0}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Safety guard: this stack is meant for the local Kind cluster.
# Override with KUBE_CONTEXT_OVERRIDE=1 if you really know what you're doing.
EXPECTED_CONTEXT="${KUBE_CONTEXT:-kind-leilfs-operator}"
# Verify the expected context exists; if so, use it directly without
# requiring the user to switch their global current-context.
if ! kubectl config get-contexts -o name | grep -qx "${EXPECTED_CONTEXT}"; then
  echo "ERROR: kube context '${EXPECTED_CONTEXT}' not found." >&2
  echo "Available contexts:" >&2
  kubectl config get-contexts -o name | sed 's/^/  /' >&2
  exit 1
fi

KCTL=(kubectl --context "${EXPECTED_CONTEXT}")
HELM=(helm --kube-context "${EXPECTED_CONTEXT}")

echo ">> Using kube context: ${EXPECTED_CONTEXT}"
echo ">> Namespace:          ${NAMESPACE}"
echo ">> Release:            ${RELEASE}"

helm repo add prometheus-community https://prometheus-community.github.io/helm-charts >/dev/null 2>&1 || true
helm repo update prometheus-community >/dev/null

"${KCTL[@]}" get ns "${NAMESPACE}" >/dev/null 2>&1 || "${KCTL[@]}" create ns "${NAMESPACE}"

"${HELM[@]}" upgrade --install "${RELEASE}" prometheus-community/kube-prometheus-stack \
  --namespace "${NAMESPACE}" \
  --version "${CHART_VERSION}" \
  --values "${SCRIPT_DIR}/values.yaml" \
  --wait --timeout 5m

echo ">> Applying ServiceMonitor for leilfs-operator"
"${KCTL[@]}" apply -f "${SCRIPT_DIR}/servicemonitor.yaml"

echo ">> Applying dashboards ConfigMap"
KUBE_CONTEXT="${EXPECTED_CONTEXT}" "${SCRIPT_DIR}/sync-dashboards.sh"

cat <<EOF

Monitoring stack installed.

Grafana:    kubectl -n ${NAMESPACE} port-forward svc/${RELEASE}-grafana 3000:80
            http://localhost:3000   (admin/admin)

Prometheus: kubectl -n ${NAMESPACE} port-forward svc/${RELEASE}-kube-prom-prometheus 9090:9090
EOF
