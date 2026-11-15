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
   user remains. For the default layout, block the service account from invoking
   the executable or opening the store while offline work is in progress:

   ```sh
   sudo chown root:root /srv/deploy-manager/data
   sudo chmod 700 /srv/deploy-manager/data
   sudo chmod 000 /usr/local/bin/deploy-manager
   ```

   Substitute actual paths and restrict any other executable copies that can
   access the same state. The recovery script runs as root; restore the data
   directory's owner before restarting. Do not use permission changes instead of
   stopping processes that already have files open.

3. **Identify both recovery members before changing files.** Read the local
   daemon diagnostic/log for the maintenance phase, source/target schema,
   operation, and backup location. The marker is
   `/srv/deploy-manager/data/state.sqlite.maintenance.json`. A recoverable
   backup is the `.db` file named by that marker **and** its adjacent `.db.json`
   manifest with `"status":"complete"`, matching source version and the
   failing program identity in the marker, and a SHA-256 matching the database
   file. Verify it read-only with the procedure below. The executable is not
   included in that backup. Retrieve and verify the exact previous release from
   your pre-upgrade release inventory; do not mistake the manifest's failing
   program identity for the rollback executable or substitute an arbitrary older
   binary. If any member is missing or cannot be verified, stop here and remain
   locked.

   Set the following shell variables from the inspected installation and
   manifest. Use the exact absolute backup path from the marker; do not guess
   the newest file:

   ```sh
   DATA=/srv/deploy-manager/data
   DB="$DATA/state.sqlite"
   MARKER="$DB.maintenance.json"
   PROGRAM=/usr/local/bin/deploy-manager
   BACKUP=/srv/deploy-manager/data/state.sqlite.upgrade-backups/REPLACE-WITH-EXACT-FINALIZED-BACKUP.db
   PRIOR=/srv/deploy-manager/rollback/deploy-manager-REPLACE-WITH-RECORDED-PRIOR-BUILD
   BACKUP_SHA256=REPLACE-WITH-SHA256-FROM-BACKUP-MANIFEST
   PRIOR_SHA256=REPLACE-WITH-TRUSTED-PRIOR-RELEASE-SHA256
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

   `program` identifies the executable that attempted the failed upgrade; it is
   not the prior executable. The exact previous build is identified by your
   retained pre-upgrade release inventory (record its build/revision and trusted
   artifact SHA-256 before installing a new version). Match that inventory to
   the database's `source_version`, and verify `PRIOR_SHA256` against the
   retained executable. If no trusted identity and exact prior artifact exist,
   remain stopped.

4. **Preserve failed state as evidence.** Capture service logs to a named file
   in the recovery directory. Quarantine the failed database and every existing
   SQLite sidecar together. Preserve, do not overwrite or remove, the marker.
   Save logs as shown, then run the script's `quarantine` action. It moves the
   DB and present sidecars together and copies the marker into new evidence:

   ```sh
   sudo sh -c 'journalctl -u deploy-manager.service --no-pager >"$1"' sh "$RECOVERY_DIR/daemon.log"
   sudo chown "$OWNER:$GROUP" "$RECOVERY_DIR/daemon.log"
   sudo chmod 600 "$RECOVERY_DIR/daemon.log"
   EVIDENCE="$RECOVERY_DIR/files"
   sudo env DB="$DB" MARKER="$MARKER" BACKUP="$BACKUP" BACKUP_SHA256="$BACKUP_SHA256" \
     PRIOR="$PRIOR" PRIOR_SHA256="$PRIOR_SHA256" \
      PROGRAM="$PROGRAM" EVIDENCE="$EVIDENCE" \
     OWNER="$OWNER" GROUP="$GROUP" PROGRAM_OWNER=root PROGRAM_GROUP=root \
     sh docs/operations/upgrade-recovery-files.sh quarantine
   ```

   Set `PROGRAM_OWNER`/`PROGRAM_GROUP` to the installed executable owner/group.
   If any command fails, keep the service stopped and investigate. Never remove
   live sidecars or mix generations.

5. **Stage and verify the matched pair offline.** After confirming the matching
   backup and prior executable again, run the `restore` action. It installs both
   via same-filesystem staging and checks the copies, while leaving the marker
   in place:

   ```sh
   sudo env DB="$DB" MARKER="$MARKER" BACKUP="$BACKUP" BACKUP_SHA256="$BACKUP_SHA256" \
     PRIOR="$PRIOR" PRIOR_SHA256="$PRIOR_SHA256" \
      PROGRAM="$PROGRAM" EVIDENCE="$EVIDENCE" \
     OWNER="$OWNER" GROUP="$GROUP" PROGRAM_OWNER=root PROGRAM_GROUP=root \
     sh docs/operations/upgrade-recovery-files.sh restore
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

7. **Archive the marker only after steps 3–6 succeed.** The restore action left
   it in place. After checking keys, archive it explicitly with the same script:

   ```sh
   sudo env DB="$DB" MARKER="$MARKER" BACKUP="$BACKUP" BACKUP_SHA256="$BACKUP_SHA256" \
     PRIOR="$PRIOR" PRIOR_SHA256="$PRIOR_SHA256" \
     PROGRAM="$PROGRAM" EVIDENCE="$EVIDENCE" \
     OWNER="$OWNER" GROUP="$GROUP" PROGRAM_OWNER=root PROGRAM_GROUP=root \
     sh docs/operations/upgrade-recovery-files.sh archive-marker
   ```

   This preserves `restored-pair.maintenance.json` beside the failed marker
   evidence. Do not clear the marker by any other means or before restoring and
   checking both members. If the script fails, remain stopped and investigate.

8. **Start the restored program and verify the installation.** Make the unit
   available again, start it, and verify normal readiness (not HTTP 503),
   administrator login, repository-key signing/decryption by an actual
   repository operation, and durable job state. Confirm accepted work is handled
   conservatively and publication reconciliation preserves the existing live
   directory identity/content; do not assume a database-only copy restores
   publications. Keep the recovery evidence and finalized backup:

   ```sh
   sudo chown "$OWNER:$GROUP" "$DATA"
   sudo chmod 700 "$DATA"
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

The isolated rehearsal builds two distinct test-only fixture executables,
induces a real child-process migration failure during startup, confirms restart
stays locked, and runs `upgrade-recovery-files.sh`—the exact file-restoration
path above—against temporary paths. It checks restored OPAQUE login,
repository-key signing/decryption, durable job state, publication content and
directory identity. It never targets a maintainer installation.
Run it with:

```sh
go test ./lib -run '^TestDocumentedOperatorRecoveryRehearsal$' -count=1 -v
```

The fixture binaries and state are disposable; they are not production fault
endpoints and do not replace the tested application startup/maintenance paths.
