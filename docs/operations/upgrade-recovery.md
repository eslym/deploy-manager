# Recovering from a failed startup upgrade

This is a deliberately offline, database-only rollback for a daemon startup
upgrade that failed before normal application work resumed. Expect downtime
until an operator restores a matching program/database pair. The daemon applies
supported forward migrations automatically at startup; it does not retry a
marked upgrade or restore a backup automatically. A restart with the marker
present stays locked (HTTP 503). Deleting the marker alone is not a repair.

This procedure is not deployment rollback. Deployment rollback republishes a
Git commit; it does not restore the application database or executable. This is
also narrower than whole-installation backup/restore in [README recovery and
backups](../../README.md#recovery-and-backups) and #23: the automatic upgrade
backup contains only SQLite data, not external keys, Git/LFS caches, or published
files.

## Recovery sequence

Use the same account that owns the installation files. The commands below show
the supported default layout: executable `/usr/local/bin/deploy-manager`, data
directory `/srv/deploy-manager/data`, database
`/srv/deploy-manager/data/state.sqlite`, and a systemd unit named
`deploy-manager.service`. Replace these values with the installation's actual
paths/unit. Set `OWNER` to the service's numeric or named Unix owner and `GROUP`
to its group. `RECOVERY_DIR` must be a new, private directory on a filesystem
with enough free space; do not put it inside the backup directory.

1. **Stop the service and disable restart loops.** Prevent a supervisor or
   operator automation from starting another daemon during recovery:

   ```sh
   sudo systemctl mask --runtime --now deploy-manager.service
   sudo systemctl is-active deploy-manager.service || true
   ```

   For a different supervisor, use its explicit stop and temporary
   disable/maintenance operation. Do not proceed if the service keeps restarting.

2. **Stop and prevent every CLI database user.** Routine compatible `setup` and
   `recover` CLI commands are allowed alongside a running daemon when their
   executable supports the database's exact schema version. They do not migrate
   existing stores. Offline restoration is different: stop all daemon **and**
   CLI users, and prevent new CLI access (for example, restrict access to the
   executable/state directory to the maintenance operator while the service is
   masked). Check process lists and the installation's process supervisor:

   ```sh
   pgrep -a -f '(^|/)deploy-manager( |$)' || true
   ```

   The command can report the `pgrep` itself or unrelated matching text; inspect
   each result. Continue only when no manager process owns or uses this store and
   new CLI invocations are prevented. Do not manipulate SQLite files while any
   user remains.

3. **Identify both recovery members before changing files.** Read the local
   daemon diagnostic/log for the maintenance phase, source/target schema,
   operation, and backup location. The marker is
   `/srv/deploy-manager/data/state.sqlite.maintenance.json`. A recoverable
   backup is the `.db` file named by that marker **and** its adjacent `.db.json`
   manifest with `"status":"complete"`, matching source version/program
   identity, and a SHA-256 matching the database file. Verify it read-only with
   the procedure below. The executable is not included in that backup: retrieve
   and verify the exact previous release using the recorded program/build
   identity (release archive/package or your independently retained executable).
   Do not substitute an arbitrary older binary. If any member is missing or
   cannot be verified, stop here and remain locked.

   Set the following shell variables from the inspected installation and
   manifest. Use the exact absolute backup path from the marker; do not guess
   the newest file:

   ```sh
   DATA=/srv/deploy-manager/data
   DB="$DATA/state.sqlite"
   MARKER="$DB.maintenance.json"
   BACKUP=/srv/deploy-manager/data/state.sqlite.upgrade-backups/REPLACE-WITH-EXACT-FINALIZED-BACKUP.db
   PRIOR=/srv/deploy-manager/rollback/deploy-manager-REPLACE-WITH-RECORDED-PRIOR-BUILD
   OWNER=deploy-manager
   GROUP=deploy-manager
   RECOVERY_DIR=/srv/deploy-manager/recovery-REPLACE-WITH-UNIQUE-UTC-ID
   test -f "$DB" && test -f "$MARKER" && test -f "$BACKUP" && test -f "$BACKUP.json"
   test -x "$PRIOR"
   sudo install -d -o "$OWNER" -g "$GROUP" -m 700 "$RECOVERY_DIR"
   test "$(stat -c '%a' "$BACKUP")" = 600
   sha256sum "$BACKUP"
   sqlite3 -readonly "$BACKUP" \
     'PRAGMA integrity_check; SELECT max(version_id) FROM goose_db_version WHERE is_applied=1;'
   ```

   Compare the printed hash with `sha256` in `$BACKUP.json`; inspect its
   `status`, `source_version`, and `program` fields. `integrity_check` must print
   `ok`, and the reported version must equal `source_version`. The read-only
   invocation does not create sidecars. If `sqlite3` is unavailable, use a
   trusted equivalent in read-only/immutable mode; if verification is not
   possible, do not restore. Do not modify the finalized backup.

4. **Preserve failed state as evidence.** Capture service logs to a named file
   in the recovery directory. Quarantine the failed database and every existing
   SQLite sidecar together. Preserve, do not overwrite or remove, the marker.
   These are individual file moves; a missing sidecar is expected and is not an
   error:

   ```sh
   sudo install -d -o "$OWNER" -g "$GROUP" -m 700 "$RECOVERY_DIR"
   sudo sh -c 'journalctl -u deploy-manager.service --no-pager >"$1"' sh "$RECOVERY_DIR/daemon.log"
   sudo chown "$OWNER:$GROUP" "$RECOVERY_DIR/daemon.log"
   sudo chmod 600 "$RECOVERY_DIR/daemon.log"
   sudo mv -- "$DB" "$RECOVERY_DIR/failed-state.sqlite"
   for suffix in -wal -shm -journal; do
     if sudo test -e "$DB$suffix"; then
       sudo mv -- "$DB$suffix" "$RECOVERY_DIR/failed-state.sqlite$suffix"
     fi
   done
   sudo cp -p -- "$MARKER" "$RECOVERY_DIR/failed-state.sqlite.maintenance.json"
   ```

   If a move or copy fails, stop and investigate. Never delete sidecars from a
   running installation or attach sidecars from the failed database to the
   standalone backup.

5. **Stage and verify the matched pair offline.** Copy (do not move) the
   finalized backup into a new staging file on the database's filesystem,
   verify its hash again, then atomically rename it into place. Install the
   retained previous executable as a separate staged file and replace the
   executable only after its identity has been checked. These commands preserve
   the backup and ensure no failed-generation sidecar remains beside the new DB:

   ```sh
   sudo install -o "$OWNER" -g "$GROUP" -m 600 -- "$BACKUP" "$DB.restore-stage"
   sudo cmp -- "$BACKUP" "$DB.restore-stage"
   sudo mv -- "$DB.restore-stage" "$DB"
   sudo install -o root -g root -m 755 -- "$PRIOR" /usr/local/bin/deploy-manager.restore-stage
   sudo cmp -- "$PRIOR" /usr/local/bin/deploy-manager.restore-stage
   sudo mv -- /usr/local/bin/deploy-manager.restore-stage /usr/local/bin/deploy-manager
   sudo chown "$OWNER:$GROUP" "$DB"
   sudo chmod 600 "$DB"
   ```

   If the executable is managed by a package manager, restore the exact package
   instead of replacing its file manually. Do not run the new executable against
   the restored old database: once unlocked, it will attempt the failed upgrade
   again. Never run an older executable against a newer or partially migrated
   live database.

6. **Check external keys before unlocking.** The OPAQUE key and repository-key
   master are outside the database backup and must remain the same files/bytes
   that were used before the upgrade. Confirm both configured files are present,
   owned by the service account, restrictive, and unchanged against any trusted
   pre-upgrade checksums/backup inventory. The default names are
   `$DATA/opaque-server.key` and `$DATA/repository-key-master.key`. The upgrade
   does not rotate keys. If a key is missing, changed, or cannot be matched to
   trusted pre-upgrade evidence, leave the service stopped and marker in place;
   never regenerate or replace it to make startup pass.

7. **Archive the marker only after steps 3–6 succeed.** Its presence is the
   durable lock; it is not evidence that restoration has completed. Preserve it
   in the evidence directory and move the live marker out of the way only now:

   ```sh
   sudo mv -- "$MARKER" "$RECOVERY_DIR/restored-pair.maintenance.json"
   ```

   Do not use a command that clears it before restoring and checking both
   members. If this operation fails, remain stopped and investigate.

8. **Start the restored program and verify the installation.** Make the unit
   available again, start it, and verify normal readiness (not HTTP 503),
   administrator login, repository-key signing/decryption by an actual
   repository operation, and durable job state. Confirm accepted work is handled
   conservatively and publication reconciliation preserves the existing live
   directory identity/content; do not assume a database-only copy restores
   publications. Keep the recovery evidence and finalized backup:

   ```sh
   sudo systemctl unmask --runtime deploy-manager.service
   sudo systemctl start deploy-manager.service
   curl --fail --silent --show-error http://127.0.0.1:8080/ >/dev/null
   sudo systemctl --no-pager --full status deploy-manager.service
   ```

   Use the actual listener/HTTPS endpoint. HTTP 503, key-binding errors,
   unexpected publication identity, missing jobs, or an inability to prove
   readiness means stop the service and preserve the resulting evidence. Do not
   retry repeatedly or clear another marker without diagnosis.

## Cases where restoration is not safe

- **No finalized verified backup, incomplete copy, bad manifest/hash/integrity,**
  or unknown migration phase: remain stopped and locked. Preserve the marker,
  database, sidecars, logs, and partial backup. Do not guess whether migration
  committed, edit goose history, manufacture a version, or clear the marker.
- **No exact prior executable/build** matching the manifest: remain stopped.
  The database-only backup cannot supply it.
- **Missing/mismatched keys, insufficient disk, ownership/permission failure,**
  or failed staging: do not unlock. Resolve the underlying issue without
  regenerating keys or changing the verified backup.
- **Fresh-install failure:** there may be no previous database/program pair.
  Preserve all files and use the explicit, isolated procedure in [fresh
  initialization and rebootstrap](fresh-initialization.md), only after
  confirming entirely new data, key, cache, and publication locations. Never
  reset an existing installation under the name of rebootstrap.

Restoring an arbitrarily old backup after normal operation has resumed is not
covered: it may resurrect consumed setup/recovery grants, stale jobs, or
filesystem publication intents. This narrow procedure is for a blocked startup
upgrade before normal application work resumes. It cannot restore lost keys,
Git/LFS cache, publications, or unrelated filesystem damage; use a separately
planned whole-installation recovery for those cases.

## Rehearsal

The repository's isolated operator-recovery rehearsal builds two test-only
fixture program versions, induces a real child-process interruption during the
upgrade lifecycle, confirms that an ordinary restart stays locked, and executes
the same explicit quarantine/stage/verify/marker-archive command sequence above
against temporary fixture paths. It never targets a maintainer installation.
Run it with:

```sh
go test ./lib -run '^TestDocumentedOperatorRecoveryRehearsal$' -count=1 -v
```

The fixture binaries and state are disposable; they are not production fault
endpoints and do not replace the tested application startup/maintenance paths.
