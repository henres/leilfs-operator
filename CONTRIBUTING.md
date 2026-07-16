# Contributing to leilfs-operator

This is a small, single-maintainer Kubernetes operator. This file covers
what a contributor needs before opening a PR: how to build/test/lint
locally, the commit convention, and the generated-code rule. See
`AGENTS.md` for architecture and deeper conventions.

## Before you open a PR

CI (`.github/workflows/test.yml`, runs on every push and PR to `master`)
gates on three things, in this order. Run them locally first:

```sh
make manifests generate   # regenerate CRD YAML, RBAC YAML, deepcopy
git diff --exit-code      # must be clean -- see "Generated code" below
make test                 # manifests, generate, fmt, vet, envtest unit tests
make lint                 # golangci-lint + yamllint
```

If your change touches HA election or reconcile logic, also run the
relevant end-to-end script against the shared `sfs-lima` test cluster
(see `test/master-failover.sh` and `AGENTS.md`'s build/test table).

## Generated code

`config/crd/bases/`, `config/rbac/role.yaml`, and
`zz_generated.deepcopy.go` are generated from kubebuilder annotations.
**Never hand-edit them.** Change the `+kubebuilder:...` annotations on
the API types (`api/v1alpha1/`) or controller methods, then run
`make manifests generate` and commit the resulting diff. CI re-runs the
same generation step and fails the build if it produces a diff, so a
stale generated file is caught before review.

Changing an RBAC annotation also requires re-applying
`config/rbac/role.yaml` to any live cluster you're testing against --
regenerating the file alone does not update permissions already granted
in-cluster.

## Copyright headers

Every Go file carries the Apache 2.0 header from
`scripts/boilerplate.go.txt`, applied automatically by
`make generate` (`controller-gen object:headerFile=...`). You shouldn't
need to add or edit headers by hand -- if `make generate` reports a
clean diff, headers are already correct.

## Commit messages

Conventional Commits: `type(scope): summary`, subject under ~72
characters, imperative mood, no trailing period. Add a body for any
non-trivial change explaining **why**, wrapped at ~72 chars -- the
subject line says what changed, the body says why it was needed and any
subtle behaviour the diff alone doesn't convey.

Types: `feat`, `fix`, `refactor`, `test`, `chore`, `docs`, `perf`.

Recurring scopes: `ha`, `operator`, `rbac`, `crd`, `plugin`, `metrics`,
`monitoring`, `ci`.

```
fix(ha): correct holderIdentity parsing and propagate imagePullSecrets to master SA
```

See `.opencode/skills/commit-after-validation/SKILL.md` for the full
pre-commit checklist and more worked examples.

## Branching and PRs

The only branch is `master`; there is no branch protection configured
on it today, so this is a convention, not an enforced gate. Small,
self-contained changes are pushed directly to `master` once `make test`
and `make lint` pass locally. Anything larger -- new features, API/CRD
changes, or anything you want a second pair of eyes on -- should go
through a PR so CI runs and the diff is reviewable before it lands.

Never push a release tag (`v*.*.*`) unless a human explicitly asks for
that specific release -- see "Cutting a release" in `AGENTS.md`.
