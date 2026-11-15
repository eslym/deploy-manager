#!/bin/sh
# Offline file phase of docs/operations/upgrade-recovery.md.
# Run only after all manager processes are stopped and the backup, prior
# executable, external keys, and destination paths have been verified.
set -eu

: "${DB:?set DB to the live SQLite database path}"
: "${MARKER:?set MARKER to $DB.maintenance.json}"
: "${BACKUP:?set BACKUP to the exact finalized verified .db file}"
: "${BACKUP_SHA256:?set BACKUP_SHA256 to the manifest-verified backup digest}"
: "${PRIOR:?set PRIOR to the matching retained prior executable}"
: "${PRIOR_SHA256:?set PRIOR_SHA256 to the trusted prior release digest}"
: "${PROGRAM:?set PROGRAM to the installed executable path}"
: "${EVIDENCE:?set EVIDENCE to a new private evidence directory}"
: "${OWNER:?set OWNER to the service account}"
: "${GROUP:?set GROUP to the service group}"
: "${PROGRAM_OWNER:?set PROGRAM_OWNER to the executable owner}"
: "${PROGRAM_GROUP:?set PROGRAM_GROUP to the executable group}"

action=${1:-restore}
if test "$action" = quarantine; then
  test -f "$DB"
  test -f "$MARKER"
  test ! -L "$DB"
  test ! -L "$MARKER"
  test ! -L "$BACKUP"
  test ! -L "$BACKUP.json"
  test ! -L "$PRIOR"
  test ! -L "$PROGRAM"
  test ! -e "$EVIDENCE"
  test ! -L "$EVIDENCE"
  install -d -o "$OWNER" -g "$GROUP" -m 700 "$EVIDENCE"
  mv -- "$DB" "$EVIDENCE/failed-state.sqlite"
  for suffix in -wal -shm -journal; do
    if test -e "$DB$suffix" || test -L "$DB$suffix"; then
      test ! -L "$DB$suffix"
      mv -- "$DB$suffix" "$EVIDENCE/failed-state.sqlite$suffix"
    fi
  done
  cp -p -- "$MARKER" "$EVIDENCE/failed-state.sqlite.maintenance.json"
  exit 0
fi
if test "$action" = archive-marker; then
  test -f "$DB"
  test -x "$PROGRAM"
  test -f "$MARKER"
  test -f "$EVIDENCE/failed-state.sqlite.maintenance.json"
  test ! -L "$DB"
  test ! -L "$MARKER"
  test ! -L "$EVIDENCE"
  test ! -L "$EVIDENCE/failed-state.sqlite.maintenance.json"
  test ! -L "$PROGRAM"
  test ! -e "$EVIDENCE/restored-pair.maintenance.json"
  test "$(sha256sum -- "$DB" | cut -d ' ' -f 1)" = "$BACKUP_SHA256"
  test "$(sha256sum -- "$PROGRAM" | cut -d ' ' -f 1)" = "$PRIOR_SHA256"
  mv -- "$MARKER" "$EVIDENCE/restored-pair.maintenance.json"
  exit 0
fi
test "$action" = restore

test ! -e "$DB"
test ! -L "$DB"
test -f "$MARKER"
test -f "$EVIDENCE/failed-state.sqlite"
test -f "$EVIDENCE/failed-state.sqlite.maintenance.json"
test -f "$BACKUP"
test -f "$BACKUP.json"
test -x "$PRIOR"
test -f "$PROGRAM"
test ! -L "$MARKER"
test ! -L "$EVIDENCE"
test ! -L "$EVIDENCE/failed-state.sqlite"
test ! -L "$EVIDENCE/failed-state.sqlite.maintenance.json"
test ! -L "$BACKUP"
test ! -L "$BACKUP.json"
test ! -L "$PRIOR"
test ! -L "$PROGRAM"
test "$(sha256sum -- "$BACKUP" | cut -d ' ' -f 1)" = "$BACKUP_SHA256"
test "$(sha256sum -- "$PRIOR" | cut -d ' ' -f 1)" = "$PRIOR_SHA256"
test ! -e "$DB.restore-stage"
test ! -e "$PROGRAM.restore-stage"
# Install the standalone database and prior executable via same-filesystem
# staging. The finalized backup itself is read-only evidence and never moved.
install -o "$OWNER" -g "$GROUP" -m 600 -- "$BACKUP" "$DB.restore-stage"
cmp -- "$BACKUP" "$DB.restore-stage"
mv -- "$DB.restore-stage" "$DB"
install -o "$PROGRAM_OWNER" -g "$PROGRAM_GROUP" -m 755 -- "$PRIOR" "$PROGRAM.restore-stage"
cmp -- "$PRIOR" "$PROGRAM.restore-stage"
mv -- "$PROGRAM.restore-stage" "$PROGRAM"

# The marker remains the lock until the operator separately verifies the
# external keys, then explicitly invokes this script with archive-marker.
