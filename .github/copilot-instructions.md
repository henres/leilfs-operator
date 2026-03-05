# Copilot Instructions — saunafs-operator

This is a Kubernetes operator written in Go (Kubebuilder) that manages SaunaFS clusters via a `SaunaFSCluster` CRD.

## Project structure

- `api/v1alpha1/` — CRD types (`SaunaFSClusterSpec`, sub-specs)
- `internal/controller/` — reconciliation logic
- `cmd/main.go` — operator entrypoint
- `config/` — Kustomize manifests (CRDs, RBAC, manager)
- `chart/` — Helm chart
- `hack/` — helper scripts (image build, kind config)

## Development conventions

- Language: Go 1.22+
- Framework: Kubebuilder / controller-runtime
- Code generation: `make generate && make manifests` after editing types
- Tests: `make test` (envtest) — always run before committing
- Linting: `make lint` (golangci-lint)
- Kubernetes API groups: `saunafs.saunafs-operator.io`

## Coding rules

- All new CRD fields must have `+kubebuilder:` validation markers and Go doc comments.
- Status conditions must follow the `metav1.Condition` pattern with `Type`, `Status`, `Reason`, `Message`.
- Every reconciled child resource must have `ctrl.SetControllerReference` called before creation.
- Use `createOrUpdate*` helpers already defined in the controller rather than raw Create/Update calls.
- Do not use `panic`; return errors up the call stack.
- Container images in specs should default to a concrete tag, never `latest` in production paths.

## Testing rules

- Unit tests live in `internal/controller/` and use envtest (Ginkgo/Gomega).
- Each test must assert the actual Kubernetes resources created (DaemonSet, Service, StatefulSet), not just the absence of errors.
- E2E tests live in `test/e2e/` and require a running kind cluster (use `hack/kind-config.yaml`).

## Release / distribution

- Helm chart is in `chart/` — bump `chart/Chart.yaml` version on every release.
- `make build-installer` produces `dist/install.yaml` for raw-kubectl installs.
- The operator image is `ghcr.io/henres/saunafs-operator`.

## Roadmap

See [TASK.md](../TASK.md) for the list of open tasks toward production readiness.

Before starting a new task in the above plan, update progress in the plan.
-->
- Work through each checklist item systematically.
- Keep communication concise and focused.
- Follow development best practices.
