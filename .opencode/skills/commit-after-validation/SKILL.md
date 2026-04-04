---
name: commit-after-validation
description: Checklist and procedure to commit changes in the saunafs-operator repo after tests have passed, following the Conventional Commits style used in this project.
compatibility: opencode
---

## Prerequisites before committing

Run these checks in order. All must pass.

### 1. Unit tests + fmt + vet
```sh
make test
```
This runs `manifests`, `generate`, `go fmt`, `go vet`, and the controller unit tests via envtest.

### 2. Build (optional but recommended)
```sh
make build
```
Catches compile errors not caught by `go vet`.

### 3. Lint (optional, requires golangci-lint installed)
```sh
make lint
```

### 4. If API types changed: regenerate manifests
```sh
make manifests
git add config/crd/bases/
```
Forget this and the CRD YAML will be stale.

### 5. End-to-end / integration (if the change touches HA or reconcile logic)
```sh
make kind-load
kubectl rollout restart deployment/saunafs-operator-controller-manager -n saunafs-operator-system
bash test/master-failover.sh
```

---

## Commit message format

This repo uses **Conventional Commits**: `type(scope): short description`

### Types

| Type | When to use |
|---|---|
| `feat` | New feature or behaviour |
| `fix` | Bug fix |
| `refactor` | Code restructuring, no behaviour change |
| `test` | Adding or fixing tests |
| `chore` | Tooling, deps, Makefile, CI |
| `docs` | Documentation only |
| `perf` | Performance improvement |

### Scopes used in this repo

| Scope | Covers |
|---|---|
| `ha` | HA Lease election, sidecar, init-container |
| `operator` | Reconcile loop, controller logic |
| `rbac` | Roles, RoleBindings, ServiceAccounts |
| `crd` | API types, printcolumns, status fields |
| `plugin` | kubectl-saunafs plugin |
| `ci` | GitHub Actions, Makefile |

### Examples from this repo

```
feat(ha): unified master StatefulSet with automatic failover and failback
fix(ha): correct holderIdentity parsing and propagate imagePullSecrets to master SA
fix(ha): harden master HA — expose selector, storage precedence, RBAC scope, observability
test(plugin): add filegoal smoke tests (get, set, error case)
chore: remove unused start-kind-lab.sh script
```

### Rules
- Subject line: max ~72 characters, imperative mood, no trailing period
- Body (optional): explain **why**, not what. Wrap at 72 chars.
- Do not use `--no-verify`; do not skip hooks.

---

## Staging and committing

```sh
# Review what changed
git diff
git status

# Stage selectively (prefer explicit over `git add -A`)
git add internal/controller/saunafscluster_controller.go
git add api/v1alpha1/saunafscluster_types.go
git add config/crd/bases/   # only if make manifests was run

# Commit
git commit -m "fix(ha): <short description>"
```

## What NOT to commit

- `.env`, kubeconfig files, any file with secrets or tokens
- Binary build artifacts (`bin/`)
- Temporary test output files
