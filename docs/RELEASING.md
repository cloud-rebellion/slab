# Releasing Slab as a public `go install` tool

Slab's public repo is `github.com/bright-interaction/slab`, but the code lives in
the private `bright-interaction/automations` monorepo under `atomicsite/` (the
directory rename to `slab/` is deferred; the mirror does the rename at publish
time). A public release **mirrors the `atomicsite/` subtree to `slab`, renaming
the module and stripping the Enterprise Edition.**

Slab is open core, and unlike pare/flare/reactor its EE lives IN-tree behind the
`ee` Go build tag. `scripts/split-public-repo.sh`:

1. subtree-splits `atomicsite/` history,
2. strips the estate compose + every `//go:build ee` file (cloud orchestration,
   Mollie billing, billing handlers) + the cloud frontend routes,
3. renames the module `github.com/bright-interaction/slab` ->
   `github.com/bright-interaction/slab` and redacts infra hostnames from history,
4. **asserts no `//go:build ee` file survived** (refuses otherwise, so the
   cloud/billing source can never leak),
5. build-checks the OSS (`!ee`) tree + gitleaks-scans, then prints what it WOULD push.

It requires `git-filter-repo` and `gitleaks` on PATH.

## Dry run (safe, no push)

```
./scripts/split-public-repo.sh
```

Get this green first. The Go build gate does NOT build the frontend, so before an
actual push also confirm the frontend builds with the cloud routes stripped:

```
# in the dry-run mirror dir the script prints:
( cd <mirror>/frontend && bun install && bun run build )
```

## Push (outward, operator step)

```
./scripts/split-public-repo.sh --push      # re-mirror the latest atomicsite/ subtree
# then tag the new version on the public repo:
#   git clone git@github.com:bright-interaction/slab && cd slab && git tag vX.Y.Z && git push origin vX.Y.Z
```

## What stays private

The EE cloud + billing (`ee/cloud_ee.go`, `internal/cloud/mollie`,
`internal/handlers/billing_ee.go`, the cloud frontend routes), the estate deploy
compose, and the rest of the monorepo never leave `bright-interaction/automations`.
The `internal/billing/plans.go` plan catalog is OSS and ships.
