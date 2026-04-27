#!/usr/bin/env bash
set -euo pipefail
NAMESPACE="${MONITORING_NAMESPACE:-monitoring}"
RELEASE="${MONITORING_RELEASE:-kube-prom-stack}"
EXPECTED_CONTEXT="${KUBE_CONTEXT:-kind-saunafs-operator}"
if ! kubectl config get-contexts -o name | grep -qx "${EXPECTED_CONTEXT}"; then
  echo "ERROR: kube context '${EXPECTED_CONTEXT}' not found." >&2
  exit 1
fi

helm --kube-context "${EXPECTED_CONTEXT}" uninstall "${RELEASE}" --namespace "${NAMESPACE}" || true
kubectl --context "${EXPECTED_CONTEXT}" delete ns "${NAMESPACE}" --ignore-not-found --wait=false
