# Full-installation backup and restore

This procedure backs up and restores an existing installation as one consistent
generation. It is distinct from [startup-upgrade recovery](upgrade-recovery.md),
whose automatic backup contains SQLite only. Deployment rollback republishes a
Git commit; it does not restore manager state.

## Required backup set

Use a filesystem/storage snapshot that captures all listed paths at one instant
and preserves inode identities, including directory identities recorded in
publication intents and deployment state. A byte-for-byte copy of SQLite alone,
`cp`/`rsync` of publications, or snapshots taken at different times are **not**
an equivalent backup. If the storage cannot provide a consistent
identity-preserving snapshot, stop the installation and use a separately tested
application-aware export/restore method; do not start with a partial restore.

Stop the daemon and every `setup`/`recover` CLI process before taking the
snapshot. Disable automatic restarts and prevent new CLI invocations during the
snapshot. Follow [process coordination](../process-coordination.md). The
snapshot must include, as one generation:

- The complete data directory, especially `state.sqlite` and any WAL/SHM files,
  both external key files, Git caches, and LFS payload caches.
- The configured `--publication-root` (default:
  `<data>/state.sqlite.publications`), including live directories, staged
  directories, and directories awaiting delayed cleanup.
- Any externally configured `--repository-key-master` file.
- The exact executable/build identity and its configuration: database, OPAQUE
  key, repository-key master, publication-root paths, and canonical origin.

The OPAQUE key (`opaque-server.key` by default) must match the database's
administrator registration, OPAQUE profile, and key/profile generation. The
independent repository-key master (`repository-key-master.key` by default) must
match the database verifier and encrypted repository keys/webhook secrets. Never
regenerate either key to make a restored database open. Record SHA-256 hashes of
both key files and the artifact in a separately protected backup inventory;
hashes are useful for detecting mix-ups but do not replace protection of the
secret backup. Restrict backup storage and access like production secrets.

The project Git and LFS caches can usually be reconstructed by fetching again,
but preserving them avoids needless network work and may be essential: unreachable
Git objects and LFS payloads no longer available upstream cannot be used for
future rollback. Cache absence does not justify changing a deployment target or
deleting publication state. Published content and publication recovery records
are required to restore the currently served sites safely.

## Restore

The example uses the default service/unit and paths from
`upgrade-recovery.md`; substitute the installation's real paths. Restore the
entire snapshot generation to its original paths and filesystem identity. Do
not combine files from different backup generations, manually edit SQLite, or
restore only database/key files while retaining unrelated publication state.

1. Mask/stop the daemon, disable supervisor restart loops, and stop all CLI
   database users. Prevent new users for the complete offline interval. Preserve
   the current failed state and logs as evidence before replacing anything.
2. Verify the selected snapshot's inventory, executable hash, and both key-file
   hashes against independently retained trusted records. If a member is
   missing, mismatched, or unverified, remain stopped. Never substitute a new
   key. Verify that configuration points to the restored data, keys, and
   publication root.
3. Restore the snapshot as a whole, preserving filesystem identity. Ensure
   service ownership and restrictive permissions are intact. Do not run a
   different executable against the restored schema.
4. While still offline, remove all restored setup/recovery grants from the
   registration-grant table. Old snapshots can resurrect grants that were
   consumed after the snapshot. With the version-matched executable available,
   the schema must be exactly supported; the command below only deletes grant
   rows and does not perform a migration:

   ```sh
   sqlite3 "$DATA/state.sqlite" \
     'DELETE FROM registration_grants; PRAGMA wal_checkpoint(TRUNCATE);'
   ```

   Run only with all application processes stopped. If the table is absent, the
   schema is unsupported, or the command/checkpoint fails, preserve the restored
   snapshot and remain stopped. Do not hand-edit schema history. Removing these
   grants does not alter the administrator account or its OPAQUE registration;
   issue any needed new setup/recovery grant through the normal CLI after the
   daemon is ready. Setup remains subject to the single-administrator enrollment
   rule; it is not a way to create a second account.
5. Start the daemon with the matching executable and configuration. Startup
   reconciliation must resolve saved publication intents using observed
   directory identities. It must not blindly repeat an exchange or clean up a
   directory whose identity is ambiguous. If readiness fails, publication
   state is blocked, or any expected path/key binding is uncertain, stop and
   preserve all evidence. Do not clear intent/maintenance state manually.
6. Verify login with the restored administrator password, list the expected
   repository keys/projects, and perform an actual repository operation that
   needs the assigned key and trusted SSH host. Confirm the expected publication
   content and identity and verify delayed cleanup did not remove the live
   directory. Check accepted jobs and warnings before restoring external traffic.
   A daemon restart invalidates all server-side sessions and pending OPAQUE login
   handshakes; clients must log in again. Treat every session as invalid even if
   an old browser cookie remains.
7. If the password is unknown, issue a normal `recover` grant and complete the
   OPAQUE recovery flow. Recovery replaces the existing administrator record,
   invalidates sessions/pending logins, and leaves repository-key encryption
   unchanged. Do not use `setup` as a password-reset shortcut.

Keep the restored snapshot and recovery evidence protected until the installation
has been verified and a new backup has been completed.

## Rehearsal

The operator recovery rehearsal exercises the real daemon startup/maintenance
path, the documented offline file actions, OPAQUE login, repository-key signing,
durable jobs, and preservation of a live publication. Run after building the
embedded frontend assets:

```sh
go test ./lib -run '^TestDocumentedOperatorRecoveryRehearsal$' -count=1 -v
```

This is specifically a startup-upgrade recovery rehearsal, not proof that a
storage provider's whole-volume snapshot is atomic or identity preserving.
Rehearse the full-installation snapshot and restore procedure on the actual
storage/supervisor configuration before relying on it for disaster recovery.
