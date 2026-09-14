+++
name = "dolt-archive"
description = "Offsite backup: JSONL snapshots to git, dolt push to GitHub/DoltHub"
version = 1

[gate]
type = "cooldown"
duration = "1h"

[tracking]
labels = ["plugin:dolt-archive", "category:data-safety"]
digest = true

[execution]
timeout = "15m"
notify_on_failure = true
severity = "critical"
+++

# Dolt Archive

Gets production data off this machine. Three layers:

1. **JSONL export** — Human-readable snapshots (saved us in Clown Show #13)
2. **Git push** — JSONL files committed and pushed to GitHub
3. **Dolt push** — Native Dolt replication to GitHub/DoltHub (if configured)

JSONL is the last-resort recovery layer. Always maintain it regardless of
whether the other layers work.

## KNOWN ISSUE — operator documentation, not a dog-facing rule (tracked: hq-addxm)

Dogs dispatched to run this plugin never see this file: `plugin.md` ships
alongside a `run.sh`, so dispatch sends `bash run.sh` directly and tells the
dog not to interpret `plugin.md` (`FormatMailBody`,
`internal/plugin/types.go`). Nothing in this section suppresses anything at
run time — it's context for Mayor, a patrol dog, or a human reading the file
directly while hq-addxm is open.

hq-addxm's inventory covers 13 Dolt databases townwide: 11 have no remote
configured, and 2 (`beads`, `heartworks_graphql_api`) point at
`git+https://github.com/...` remotes. Those are working Dolt-capable git
remotes, not inert URLs — `git ls-remote` on `beads` already returns
`refs/dolt/data`, and `beads` is a PUBLIC repo — so a push succeeding there
is not automatically good news; whether publishing that database is
intended is an open question for Mayor, not yet decided. Until Mayor
decides, contain it rather than only watching for it after the fact: drop
the remote on the `beads` Dolt database (`dolt remote remove origin` in
`$DOLT_DATA_DIR/beads`) or run this plugin with `--skip-dolt-push`. The
hourly cooldown (`[gate] duration = "1h"` above) means `run.sh` retries
the push every cycle regardless — a receipt is a detection channel, read
after the push already happened, not a gate. `run.sh`
auto-discovers every non-test database on the Dolt server
(`DEFAULT_DBS=auto`), so this plugin covers all 13 databases in
hq-addxm's inventory — including `beads` and `heartworks_graphql_api` —
and its receipts do speak to them, with one qualification: the export is
`SELECT * FROM issues` per database (`run.sh:127`), so `jsonl=13/13` means
13 issues tables were dumped, not that 13 databases are recoverable.
`SHOW TABLES` on `beads` returns 28 tables — `wisps`, `comments`,
`dependencies`, `labels`, `events`, `issue_snapshots` among them — and none
of those are exported. For the 11 remote-less databases the JSONL/git layer
is the only offsite-capable path (`dolt push` is unconfigured for them), so
even once the offsite gap this note tracks is closed, 27 of the 28 tables
per database still have no copy off this host.

The expected receipt while hq-addxm is open is `jsonl=13/13`, `dolt_push=0/2` — 11
databases have no remote configured, so no push is attempted, while
`beads` and `heartworks_graphql_api` fail against their `git+https`
remotes — and `git=false` (`$BACKUP_REPO` has no `.git` directory),
landing as `result=warning`. That receipt means the export step ran
cleanly and the two remote-bearing databases are not yet landing offsite
— it is not evidence a backup exists anywhere but this host for the
other 11. A disk failure here loses the Dolt data and every JSONL export
together, no matter how clean the logs look. A changed ratio, not the
warning itself, is what to escalate.

hq-wisp-2egq1 is a breadcrumb only, reaper-purgeable; hq-addxm is the
durable record this note is keyed to and holds regardless of the wisp's
lifecycle.

**This note goes stale per-database, not all at once.** As each database in
hq-addxm's inventory gets a working offsite copy — a successful push, or
`$BACKUP_REPO` gaining a working git remote whose visibility has been
established (and confirmed with Mayor if public, per the public-repo
caveat above) — narrow this section to drop that database rather than
waiting for all 13 before touching it. A working git remote alone does not
close the gap; an unvetted remote's visibility is exactly the open question
the dolt-push caveat above raises for `beads`.

**Escalate on a change, not a repeat**: a database that previously pushed
successfully starts failing, JSONL export itself fails, or a database starts
pushing successfully (confirm with Mayor before treating it as resolved,
given the public-repo caveat above), or a database drops out of the
`jsonl` count entirely — any of those is new information. That last case
is silent: `run.sh` auto-discovers whatever `SHOW DATABASES` returns
(`run.sh:83`), and a discovered database without an `issues` table is
skipped without touching `EXPORTED` or `EXPORT_FAILED` (`run.sh:120-122`),
so if the Dolt server stops serving a database, or its `issues` table goes
missing, the receipt still reads a clean `jsonl=12/12` — the database
lands in neither the numerator nor the denominator, so the ratio shows
all-success while coverage has shrunk. Compare the current `jsonl`
denominator against the expected `13` above, not just the ratio.
Anyone unsure whether an observation matches the known gap above should
nudge deacon/mayor rather than assume either way.

## Config

```bash
DOLT_DATA_DIR="$GT_TOWN_ROOT/.dolt-data"
PROD_DBS=("hq" "gt" "mo")
JSONL_EXPORT_DIR="$GT_TOWN_ROOT/.dolt-archive/jsonl"
DOLT_HOST="127.0.0.1"
DOLT_PORT=3307
DOLT_USER="root"
```

## Step 1: JSONL export

Export all issues from each production database to JSONL files. These are
human-readable, diffable, and survive any storage backend failure.

```bash
echo "=== JSONL Export ==="
EXPORTED=0
EXPORT_FAILED=0

mkdir -p "$JSONL_EXPORT_DIR"

for DB in "${PROD_DBS[@]}"; do
  EXPORT_FILE="$JSONL_EXPORT_DIR/${DB}-$(date +%Y%m%d-%H%M).jsonl"
  LATEST_LINK="$JSONL_EXPORT_DIR/${DB}-latest.jsonl"

  echo "Exporting $DB..."

  # Use bd export if available, otherwise query directly
  if bd export --db "$DB" --format jsonl > "$EXPORT_FILE" 2>/dev/null; then
    LINE_COUNT=$(wc -l < "$EXPORT_FILE" | tr -d ' ')
    FILE_SIZE=$(du -h "$EXPORT_FILE" | cut -f1)
    echo "  $DB: $LINE_COUNT issues exported ($FILE_SIZE)"

    # Update latest symlink
    ln -sf "$EXPORT_FILE" "$LATEST_LINK"
    EXPORTED=$((EXPORTED + 1))
  else
    # Fallback: query Dolt directly for issue data
    dolt sql -q "SELECT * FROM issues ORDER BY id" \
      --host "$DOLT_HOST" --port "$DOLT_PORT" -u "$DOLT_USER" \
      -d "$DB" --no-auto-commit --result-format json \
      > "$EXPORT_FILE" 2>/dev/null

    if [ $? -eq 0 ] && [ -s "$EXPORT_FILE" ]; then
      LINE_COUNT=$(wc -l < "$EXPORT_FILE" | tr -d ' ')
      echo "  $DB: exported via SQL ($LINE_COUNT lines)"
      ln -sf "$EXPORT_FILE" "$LATEST_LINK"
      EXPORTED=$((EXPORTED + 1))
    else
      echo "  WARN: $DB export failed"
      rm -f "$EXPORT_FILE"
      EXPORT_FAILED=$((EXPORT_FAILED + 1))
    fi
  fi
done

# Prune old exports (keep last 24 snapshots per DB)
for DB in "${PROD_DBS[@]}"; do
  SNAPSHOTS=$(ls -t "$JSONL_EXPORT_DIR/${DB}-2"*.jsonl 2>/dev/null | tail -n +25)
  if [ -n "$SNAPSHOTS" ]; then
    echo "$SNAPSHOTS" | xargs rm -f
    echo "Pruned old $DB snapshots"
  fi
done

echo "Exported: $EXPORTED, failed: $EXPORT_FAILED"
```

## Step 2: Git commit and push

Commit JSONL snapshots to a backup branch and push to GitHub.

```bash
echo "=== Git Push ==="
GIT_PUSHED=false

# Check if we have a git backup repo configured
BACKUP_REPO="$HOME/gt/.dolt-archive/git"

if [ -d "$BACKUP_REPO/.git" ]; then
  cd "$BACKUP_REPO"

  # Copy latest JSONL files
  for DB in "${PROD_DBS[@]}"; do
    LATEST="$JSONL_EXPORT_DIR/${DB}-latest.jsonl"
    if [ -f "$LATEST" ]; then
      cp "$(readlink "$LATEST" || echo "$LATEST")" "$BACKUP_REPO/${DB}.jsonl"
    fi
  done

  # Check for changes
  if git diff --quiet && git diff --staged --quiet; then
    echo "No changes to commit"
  else
    git add *.jsonl
    git commit -m "Archive snapshot $(date +%Y-%m-%d-%H%M)" \
      --author="Gas Town Archive <archive@gastown.local>" 2>/dev/null

    # Check if remote exists before pushing
    if git remote get-url origin > /dev/null 2>&1; then
      if git push origin main 2>/dev/null; then
        GIT_PUSHED=true
        echo "Pushed to GitHub"
      else
        echo "WARN: Git push to remote failed (check GitHub credentials/permissions)"
      fi
    else
      echo "WARN: No git remote configured for backup repo"
      echo "  To set up: cd $BACKUP_REPO && git remote add origin <github-url>"
    fi
  fi
else
  echo "No git backup repo at $BACKUP_REPO — skipping git push"
  echo "  To set up: git init $BACKUP_REPO && cd $BACKUP_REPO && git remote add origin <url>"
fi
```

## Step 3: Dolt native push

Push production databases to GitHub/DoltHub remotes via `dolt push`.

```bash
echo "=== Dolt Push ==="
DOLT_PUSHED=0
DOLT_PUSH_FAILED=0

for DB in "${PROD_DBS[@]}"; do
  DB_DIR="$DOLT_DATA_DIR/$DB"

  if [ ! -d "$DB_DIR/.dolt" ]; then
    echo "  $DB: no .dolt directory, skipping"
    continue
  fi

  # Check if remotes are configured
  REMOTES=$(cd "$DB_DIR" && dolt remote -v 2>/dev/null | grep -v "^$" | head -5)

  if [ -z "$REMOTES" ]; then
    echo "  $DB: no remotes configured, skipping dolt push"
    continue
  fi

  echo "  $DB: pushing to remotes..."

  # Push to each remote
  cd "$DB_DIR"
  for REMOTE_NAME in $(dolt remote -v 2>/dev/null | awk '{print $1}' | sort -u); do
    if timeout 120 dolt push "$REMOTE_NAME" main 2>/dev/null; then
      echo "    $REMOTE_NAME: pushed"
      DOLT_PUSHED=$((DOLT_PUSHED + 1))
    else
      echo "    $REMOTE_NAME: FAILED"
      DOLT_PUSH_FAILED=$((DOLT_PUSH_FAILED + 1))
    fi
  done
done

echo "Dolt push: $DOLT_PUSHED succeeded, $DOLT_PUSH_FAILED failed"
```

## Step 4: Verify remote has data

Verify that the backup data has successfully reached the remote and is accessible.

```bash
echo "=== Verification ==="
VERIFY_PASSED=0
VERIFY_FAILED=0

# Verify JSONL in git backup
if [ -d "$BACKUP_REPO/.git" ]; then
  echo "Verifying git remote..."
  if cd "$BACKUP_REPO" && git ls-remote origin HEAD > /dev/null 2>&1; then
    # Try to clone into temp directory to verify
    TEMP_CLONE=$(mktemp -d)
    if git clone --depth 1 origin "$TEMP_CLONE" 2>/dev/null; then
      for DB in "${PROD_DBS[@]}"; do
        if [ -f "$TEMP_CLONE/${DB}.jsonl" ]; then
          REMOTE_COUNT=$(wc -l < "$TEMP_CLONE/${DB}.jsonl" | tr -d ' ')
          echo "  git: $DB verified ($REMOTE_COUNT lines in remote)"
          VERIFY_PASSED=$((VERIFY_PASSED + 1))
        else
          echo "  git: $DB MISSING from remote"
          VERIFY_FAILED=$((VERIFY_FAILED + 1))
        fi
      done
    else
      echo "  git: Clone verification failed"
      VERIFY_FAILED=$((VERIFY_FAILED + 1))
    fi
    rm -rf "$TEMP_CLONE"
  else
    echo "  git: Remote not accessible"
  fi
fi

# Verify dolt push (check if remotes have our commits)
for DB in "${PROD_DBS[@]}"; do
  DB_DIR="$DOLT_DATA_DIR/$DB"
  if [ -d "$DB_DIR/.dolt" ]; then
    # Check if any dolt remotes are reachable
    REMOTE_HEADS=$(cd "$DB_DIR" && dolt remote -v 2>/dev/null | awk '{print $1}' | sort -u)
    if [ -n "$REMOTE_HEADS" ]; then
      cd "$DB_DIR"
      # Verify at least one remote has data
      for REMOTE in $REMOTE_HEADS; do
        if dolt log "$REMOTE/main" -n 1 > /dev/null 2>&1; then
          echo "  dolt: $DB on $REMOTE verified"
          VERIFY_PASSED=$((VERIFY_PASSED + 1))
          break
        fi
      done
    fi
  fi
done

echo "Verified: $VERIFY_PASSED, failed: $VERIFY_FAILED"
```

## Record Result

```bash
SUMMARY="Archive: jsonl=$EXPORTED/$((EXPORTED + EXPORT_FAILED)), git=${GIT_PUSHED}, dolt_push=$DOLT_PUSHED/$((DOLT_PUSHED + DOLT_PUSH_FAILED)), verify=$VERIFY_PASSED/$((VERIFY_PASSED + VERIFY_FAILED))"
echo "=== $SUMMARY ==="

RESULT="success"
if [ "$EXPORT_FAILED" -gt 0 ] || [ "$DOLT_PUSH_FAILED" -gt 0 ] || [ "$VERIFY_FAILED" -gt 0 ]; then
  RESULT="warning"
fi

bd create "$SUMMARY" -t chore --ephemeral \
  -l type:plugin-run,plugin:dolt-archive,result:$RESULT \
  -d "$SUMMARY" --silent 2>/dev/null || true

if [ "$EXPORT_FAILED" -gt 0 ]; then
  gt escalate "JSONL export failed for $EXPORT_FAILED databases" \
    --severity critical \
    --reason "JSONL is our last-resort recovery layer. $EXPORT_FAILED databases failed to export."
fi
```
