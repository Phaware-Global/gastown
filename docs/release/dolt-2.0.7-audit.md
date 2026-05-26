# Dolt 2.0.7 Audit for gt 1.2

Date: 2026-05-26

Base audited: `origin/main` at `94b3d5aa` (clean installed build containing PR `#4080`, `#4081`, and `#4096`).

Scope: audit the Gas Town Dolt dependency from the current release floor (`1.84.0` minimum, `1.83.0` testcontainers image, `1.82.4` E2E Docker build) to Dolt `2.0.7`.

## Updated References

- Runtime minimum: `internal/deps.MinDoltVersion` is now `2.0.7`.
- Testcontainers image: `dolthub/dolt-sql-server:2.0.7` in Unix and Windows testutil constants.
- CI integration image pre-pulls now use `dolthub/dolt-sql-server:2.0.7`.
- CI, nightly, and Docker installs pin Dolt `v2.0.7` instead of relying on `latest`.
- E2E Docker build `DOLT_VERSION` is now `2.0.7`.
- User-facing prerequisites now require Dolt `2.0.7+`.

## What Changed Since the Current Release Floor

- `1.85.0`: marked the oldest 1.x release intended to be binary-compatible with future 2.x releases; introduced adaptive encoding compatibility tests and event scheduler quiescing work.
- `1.86.0`: changed the `dolt_revert()` stored procedure result schema and fixed remotes API push state, ignored-table diff/show behavior, and event scheduler shutdown handling.
- `1.86.5`: added remotes API client capability negotiation and compatibility tests around adaptive encoding.
- `2.0.0`: enabled automatic garbage collection, archival storage, and adaptive storage for TEXT, JSON, GEOMETRY, and BLOB types by default. Dolt states 2.0 is backward compatible with 1.x databases, but databases written by 2.x releases may not be readable by all 1.x clients.
- `2.0.1` through `2.0.6`: fixed checkout/reset/rebase table preservation, `diff -r sql` schema-change failures, SSH child-process leaks from `CALL dolt_fetch`, branch-control/security issues, crash/corruption edge cases in storage files, adaptive encoding cleanup, and several SQL engine regressions.
- `2.0.7`: fixes a `go-mysql-server` CachedResults/hash-join leak in long-lived SQL server processes, reducing memory growth risk for Gas Town's Dolt-backed server flows.

## Compatibility Findings

- Gas Town does not parse Dolt storage internals directly, so no migration code is needed for 2.0 automatic GC, archival storage, or adaptive storage.
- Gas Town does not call `dolt_revert()` directly, so the 1.86 result-schema change does not require a compatibility shim.
- Gas Town does not depend on `dolt diff -r sql` in production paths, so the 2.0.1 nonzero schema-change behavior is not a compatibility blocker.
- Raising the binary floor to `2.0.7` prevents older local clients from writing shared Gas Town Dolt stores after 2.x clients have written data.
- The `2.0.7` CachedResults leak fix is relevant to long-lived `dolt sql-server` processes and is the main reason to require this patch release.

## Validation Evidence

- `dolt version`: local host still reports `dolt version 1.84.0`, so the new `CheckDolt` gate should classify this host as too old until the system binary is upgraded.
- `gt dolt status`: server was running on port `3307`, query latency `0s`, and reported one pre-existing orphan database (`testrig`) for cleanup.
- `gt scheduler status`: scheduler was active with scheduled work and available capacity.
- `gh api repos/dolthub/dolt/releases/tags/v2.0.7 --jq '.assets[].name'`: verified the `install.sh` and `dolt-linux-amd64.tar.gz` release assets used by CI/Docker are present.
- `docker manifest inspect docker.io/dolthub/dolt-sql-server:2.0.7`: verified the pinned testcontainers image exists for linux/amd64 and linux/arm64. The unqualified `dolthub/dolt-sql-server:2.0.7` manifest check fails in this environment because short-name resolution requires an interactive prompt; CI/testcontainers use Docker Hub resolution for the same image name.
- Release-note sources checked with `gh api repos/dolthub/dolt/releases/tags/v2.0.7`, `v2.0.0`, and release entries for `v1.85.0`, `v1.86.0`, `v1.86.5`, `v2.0.1` through `v2.0.6`.
