# Upgrade Runbook — beads 1.0.5 + Dolt 2.0.7

This release moves the beads library to **v1.0.5** and raises the minimum **Dolt
version to 2.0.7**. It is **not** a drop-in binary swap: beads 1.0.5 splits the
`dependencies.depends_on_id` column into typed columns and drops it (migrations
`0041` → `0043` → `0044`/`0045`), and that schema requires Dolt ≥ 2.0.7. bd
auto-migrates each database the first time it connects, so **version order and a
backup matter**.

> Skip-ahead order:
> `gt down` → back up `.dolt-data` → upgrade **Dolt (≥ 2.0.7)** → upgrade
> **bd (1.0.5)** + **gt** → `gt up` (auto-migrates) → `gt upgrade` →
> `gt doctor` → verify rig prefixes.

## Why these versions are coupled

- The `gt` binary links `github.com/steveyegge/beads` **v1.0.5**. The **bd CLI
  must match that version.** A mismatch (e.g. bd CLI 1.0.0 against the 1.0.5
  library) fails schema reconciliation with
  `failed to create ready_issues view: table "d" does not have column
  "depends_on_id"`.
- beads 1.0.5's migrations (foreign-key cascades, the generated-column drop)
  require **Dolt ≥ 2.0.7**. Gas Town now hard-validates this and refuses
  beads-enabled `gt install`/`gt doctor` on older Dolt
  (`internal/deps/dolt.go`, `MinDoltVersion = "2.0.7"`).
- beads 1.0.5 reads `issue_prefix` from the Dolt **config table** (1.0.0 read
  `config.yaml`). `gt doctor --fix` reconciles any drift.

## 0. Pre-flight

Record current versions for rollback reference and quiesce the town:

```bash
gt --version
bd version          # likely 1.0.0–1.0.2 pre-upgrade
dolt version        # must end up >= 2.0.7

gt down             # stops daemon, witnesses, refinery, and the Dolt server
```

**Back up the Dolt data.** Dolt is content-versioned (every commit is
recoverable), but take a cold copy before a schema migration:

```bash
# from the town root (e.g. ~/gt)
cp -a .dolt-data .dolt-data.pre-1.0.5.bak
```

If you run the project's backup tooling (`lifecycle.backup.*` / the backup
skill), run a full backup cycle now instead.

## 1. Upgrade Dolt first (≥ 2.0.7)

Order is load-bearing: upgrade Dolt **before** bd 1.0.5 ever opens a database,
or the migration can fail mid-flight.

```bash
# macOS
brew upgrade dolt

# Linux / other: use your package manager, or reinstall from
#   https://github.com/dolthub/dolt#installation

dolt version        # confirm >= 2.0.7
```

## 2. Upgrade the beads CLI to v1.0.5

Keep the CLI in lockstep with the library version in `go.mod`.

```bash
# Homebrew bundles a matching bd with the gastown formula:
brew upgrade gastown

# From source — pin to match go.mod; do NOT use @latest blindly:
go install github.com/steveyegge/beads/cmd/bd@v1.0.5
bd version          # confirm 1.0.5
```

## 3. Upgrade the gt binary

```bash
# Homebrew
brew upgrade gastown

# or build from source on the merged main:
git pull && make build && cp gt "$(go env GOPATH)/bin/"
gt --version
```

## 4. Migrate the databases, then run `gt upgrade`

beads 1.0.5 runs its schema migrations **automatically the first time it opens
each database**. Bring the server up so that happens under your control, then
run the post-install migration:

```bash
gt up               # starts Dolt 2.0.7 + managed server; first bd touch migrates HQ + each rig DB
gt upgrade          # post-install migration: doctor --fix, CLAUDE.md, daemon.json, hooks, formulas
```

Preview first if you like:

```bash
gt upgrade --dry-run --verbose
```

`gt upgrade` runs `gt doctor --fix`, which repairs **rig-config-sync**
(`issue-prefix`, `dolt.idle-timeout`, `export.auto` drift) and beads-redirect
targets — relevant because 1.0.5 reads `issue_prefix` from the Dolt config table.

## 5. Verify

```bash
gt doctor                      # clean: no DoltTooOld, no prefix/redirect drift
bd version && dolt version     # 1.0.5 / >= 2.0.7
bd list --limit 5              # confirm a database opens & queries
gt status                      # town healthy
```

Spot-check a rig — the created ID must use the **rig's configured prefix**, not
the rig directory name:

```bash
cd <rig>
bd create --json --title "upgrade smoke" --type task
# ID should be <prefix>-... ; if it shows the rig name, run `gt doctor --fix`
```

## Caveats

1. **Order is load-bearing.** Dolt 2.0.7 before bd 1.0.5 touches any database.
   Back up `.dolt-data` first.
2. **Migration is per-database and effectively one-way.** HQ and every rig
   database migrate independently on first 1.0.5 connect. Once a database is on
   the 1.0.5 schema, an older bd 1.0.0 binary can no longer open it cleanly — do
   not mix CLI versions across a town.
3. **⚠️ Do not run `gt dolt migrate-wisps` on a migrated town.** It still issues
   `SELECT d.depends_on_id FROM dependencies d`, and 1.0.5 dropped that column,
   so it errors. It is a manually-invoked command (not part of install/upgrade);
   avoid it until patched.
4. **New `gt rig add` is fixed; existing rigs rely on doctor.** Rigs created
   after this upgrade seed `issue_prefix` correctly via the SDK. Pre-existing
   rigs carry their prefix from creation; `gt doctor --fix` (run by
   `gt upgrade`) reconciles any drift.
5. **Local integration tests are slower.** 1.0.5 runs many more migrations per
   `bd init`, so the integration suite takes markedly longer (CI's timeout was
   raised accordingly). Running it locally also needs ICU headers for the CGO
   Dolt engine and Dolt 2.0.7.

## Rollback

Revert `gt` and `bd` to their prior versions **and** restore the data dir:

```bash
gt down
rm -rf .dolt-data && mv .dolt-data.pre-1.0.5.bak .dolt-data
# reinstall the previous gt + bd, then `gt up`
```

You cannot keep the 1.0.5-migrated databases with a 1.0.0 bd — rolling back the
binaries means rolling back the data directory too.
