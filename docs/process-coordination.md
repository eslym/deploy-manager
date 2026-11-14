# Daemon and local command coordination

Run one main `serve` daemon per installation, on a supported local Linux
filesystem. A second daemon using the same database fails immediately, even
when configured with a different HTTP port. `setup` and account `recover`
commands do not take daemon ownership: routine grant issuance can run alongside
the daemon, without stopping it or triggering its workers or publication/cache
recovery. Use the same executable and data directory for all commands.

Two permanent owner-only files beside the database coordinate processes:

- `state.sqlite.daemon.lock`: exclusive daemon lifetime ownership.
- `state.sqlite.access.lock`: shared database-use access, held until database
  closure. Fresh initialization takes exclusive access before database/key work
  and switches to shared access only after preparation succeeds. This is also
  the coordination boundary for future schema maintenance.

Never delete, replace, hard-link, or repurpose these files to bypass contention.
Locks belong to open OS descriptors, not file contents or a PID. Process death
releases ownership automatically; file presence alone does not mean a process
is running. The data directory must be owned by the current user and not
writable by others. Database and lock aliases (including symlinks, hard links,
and symlinked database-directory ancestors) are refused. Every process must use
the same installation paths; separate database copies are separate installations.
External tools and privileged changes cannot be made safe by advisory locks.

Lock identities are empty files. Directory ancestors must belong to root or
the invoking user and cannot be writable by others, except for sticky shared
directories such as `/tmp`.

Access contention during initialization/maintenance fails within approximately
one second; retry once that operation finishes. Compatible SQLite transactions
also have a one-second busy timeout. A busy error does not authorize deleting
locks, resetting state, or running another daemon.

Maintenance refusal and exact-version compatibility are separate safeguards:
OS crash release must never clear a durable maintenance marker. The required
policy is to refuse commands while marked or when their schema/history does not
exactly match, without migration, grant issuance, or repair. Marker enforcement
is delivered separately in #30; exact schema/history validation and replacing
the existing ad hoc schema preparation are deferred to #31. **This coordination
change alone does not implement or certify version compatibility.** Do not run
mixed executable versions against an existing installation.

For offline database restoration, stop the daemon **and all database-capable
CLI commands**, disable automatic daemon restarts, and prevent new command use
throughout restoration. Routine account recovery only issues a grant; it is not
database repair. Replacing live database files while a shared user exists is not
supported. Retain matching external keys and follow the dedicated upgrade
recovery procedure when available; deleting an OS lock or maintenance marker
is not database repair.
