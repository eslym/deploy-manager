# LFS hydration and actual-directory atomic publication

Researched **2026-11-12**. Planning only; no implementation or platform decision. Supplements [go-git compatibility](go-git-compatibility.md). Protocol facts below come from primary sources; proposed restrictions/recovery steps are application recommendations, not guarantees supplied by go-git.

## Feasibility and decisions first

- **LFS is feasible in-process alongside stable go-git v5**, but requires a separate LFS client: v5.19.3 explicitly does not implement LFS. Retain SHA-1 Git repositories as the supported network scope. An LFS pointer's `oid sha256:…` identifies the **large-file payload**, not the Git commit/blob or the repository's object format; SHA-256 LFS hydration does not require or enable SHA-256 Git fetch. [v5] [pointer]
- **Actual-directory atomic replacement is feasible on a qualified Linux storage platform**, using `renameat2(RENAME_EXCHANGE)`, not ordinary replacement rename. Both live and staging can be nonempty actual directories. Linux-only v1 was separately accepted during the research; the filesystem capability requirements and exchange design still require confirmation. [rename]
- Initial practical scope: basic HTTPS LFS downloads; anonymous public HTTPS repositories, and private SSH repositories whose servers provide `git-lfs-authenticate` for HTTPS LFS access. This is not universal Git LFS/provider compatibility. Pure-SSH-only servers, private HTTPS credentials, arbitrary filters/extensions, and repository-directed network configuration should fail explicitly rather than publish pointer text. [discovery] [auth] [basic]

## 1. LFS hydration

### Official HTTP download flow

1. Inspect the **resolved commit's** exported blobs for supported pointers. The v1 format is UTF-8, under 1024 bytes, with `version https://git-lfs.github.com/spec/v1`, `oid sha256:<lowercase hex>`, and nonnegative byte `size`. Preserve tracked executable mode. Empty files remain empty. Define pointer detection explicitly: hydrating every canonical pointer is a simple export policy but differs from native checkout, where `.gitattributes` determines whether the LFS smudge filter runs. Supporting native attribute selection is additional work; reject unsupported pointer extensions rather than silently omit required transformations. [pointer]
2. Derive/approve an LFS endpoint. Normally HTTPS Git `https://host/owner/repo[.git]` maps to `https://host/owner/repo.git/info/lfs`. SSH discovery is described below. POST JSON to `<endpoint>/objects/batch`, with `Accept` and `Content-Type: application/vnd.git-lfs+json`, `operation: "download"`, `transfers: ["basic"]`, and requested `{oid, size}` entries; `hash_algo: "sha256"` is explicit (also the default). An optional `ref.name` is a fully qualified server ref, not an invented branch for a pinned commit. [discovery] [batch]
3. Validate a bounded response against the requested objects and sizes. `transfer` may be omitted (basic default), but reject an unsupported selection. HTTP 200 is **not** proof of success: objects can contain individual errors. Each download needs a `download` action. Missing, duplicate/inconsistent, or unrequested results must not cause partial publication. [batch]
4. GET each action's `href` for its raw bytes, applying only that action's approved headers. Signed cloud-storage URLs on another origin are an intended protocol feature, not necessarily an attack. Batch endpoint authorization and object authorization are separate: `authenticated` describes whether further credential discovery is expected; it is not permission to copy batch credentials to arbitrary URLs. No external credential helper is needed for the proposed scope. [batch] [basic] [auth]
5. Stream to a temporary file, enforce **exact pointer byte size and SHA-256 digest** before incorporating the payload into staging. Read no more than expected size plus one (within configured maxima), detect truncation/excess data, check write/close failures, and reject digest mismatch. Do not trust `Content-Length` or the batch size instead of the pointer. Bound total hydrated export bytes, per-object bytes, object count, batch JSON, transfer concurrency, and time. A verified OID cache is optional, not required for v1. [pointer] [basic]

### SSH discovery without local executables

The documented hybrid flow invokes **on the Git server** `git-lfs-authenticate <repository-path> download`, returning bounded JSON with optional endpoint `href`, HTTP `header` values, and `expires_in`/`expires_at`. Those headers authorize subsequent **batch** requests. A missing `href` leaves the derived endpoint in use. Preserve SSH remote path semantics (scp-like versus `ssh://` paths); the official 3.0 changelog specifically notes preserving the leading slash for `ssh://`. [discovery] [auth] [changelog]

Use `golang.org/x/crypto/ssh` networking and a session `Run`/`Start` exec request, **not** `os/exec`, `ssh`, `git`, or a local `git-lfs` binary. The server must expose the named command/service; this is no more a local-executable dependency than remote `git-upload-pack`. Reuse the assigned repository signer and the same fail-closed SQLite-backed host-key callback, normalized hostname/port policy, and dashboard failure handling used for Git. SSH trust does **not** replace TLS certificate validation for the HTTP leg. [ssh-session] [ssh-client] [auth]

An SSH exec request is a command **string**, typically interpreted by the remote shell. Fix the command and operation; safely quote/validate the parsed repository path, reject control characters and unsupported path forms, and do not splice an arbitrary remote URL into it. Bound stdout/stderr, drain both, redact errors, and close the connection/session on timeout. No interactive shell, agent forwarding, user hooks, or arbitrary commands are needed. If the constraint were intended to prohibit even server-side commands, this hybrid flow would be excluded and that interpretation needs clarification. [ssh-session]

Treat returned headers, tokens, and signed URLs as secrets. Keep them in memory, scoped to the project/repository/operation and exact approved endpoint/action; never persist them to job logs or published files. Prefer `expires_in` over `expires_at`, account for request time and clock skew, and reauthenticate/rebatch before expiry. The API provides expiry hints, not permanent credentials. A bounded refresh of an expired token is distinct from retrying invalid SSH credentials or host-trust rejection; no endless 401/403 loop and no trust bypass. Public HTTPS starts without administrator credentials and fails if interactive/private authentication is required. [auth] [batch]

The upstream client now falls back to a guessed endpoint plus Git credential discovery when `git-lfs-authenticate` is unavailable. **Do not inherit that fallback blindly:** this application's private SSH scope can fail clearly when the service is absent; it has no configured HTTPS password/helper fallback. [discovery]

### Pure SSH is real, but a separate scope

`git-lfs-transfer <path> download` is a different protocol: capability/version negotiation, pkt-line batch, and `get-object` streams over SSH, rather than HTTP URLs. Despite the old proposal document's introductory disclaimer, Git LFS **3.0.0 shipped** pure SSH support; current upstream source implements it and launches an external SSH subprocess. That source cannot simply be adopted unchanged under the no-local-executable constraint. A Go SSH implementation is possible but adds another protocol/client test matrix. Recommend deferring it, explicitly rejecting servers that only offer pure SSH. Do not reuse the changelog's **2021** statement about forge support as a current provider-support claim. [ssh-proposal] [changelog] [ssh-transfer] [ssh-adapter]

### Network trust: proposed fail-closed restrictions

The spec permits `.lfsconfig` to override endpoints, SSH discovery to return another host, and batch actions to direct clients to object storage. These are network destinations from repository/server input and must not become unrestricted server-side requests. The following are **application policy recommendations**, intentionally narrower than native Git LFS: [discovery] [batch] [basic]

- Do not automatically obey committed `.lfsconfig`, remote `lfsurl`, environment/global Git config, includes, proxy settings, custom transfer executables, or credential helpers. Reject a repository endpoint override requiring unsupported behavior, or require an independently approved administrator endpoint. Never quietly hydrate from an unintended default endpoint.
- Require HTTPS with normal certificate/hostname verification, reject URL userinfo and control characters, and validate every discovered endpoint, object URL, and redirect. Use explicit approved origins/ports for batch and object storage (including necessary provider CDN origins); cross-origin actions can work without sharing batch credentials. Private/internal LFS hosts require deliberate separate approval, not a blanket SSRF exception.
- Restrict resolved/dialed IPv4/IPv6 addresses to approved networks; normally deny loopback, link-local/metadata, private and other reserved/internal destinations. Validate at connection time as well as URL parsing to resist DNS rebinding; control proxies so they cannot bypass this policy. A valid TLS certificate alone is not SSRF protection.
- Simplest v1 redirect policy: **reject redirects** and report the unsupported endpoint. If later enabled, bound hops, revalidate destination/network/TLS on every hop, reject HTTPS downgrade, and reconstruct headers under explicit credential scope. Never automatically replay SSH-returned headers, arbitrary token headers, cookies, or signed queries at a new origin. Even same-origin paths can need repository-scoped credential restrictions.
- Go's `http.Client.CheckRedirect`, custom `Transport`/dialing, timeout and proxy controls are the seams. Default redirect handling forwards most initial headers and only specially restricts some sensitive headers (allowing matching domains/subdomains); that is insufficient for arbitrary LFS action headers or an exact-origin security policy. Sanitize returned headers; reject routing/hop-by-hop overrides such as `Host`, not just CR/LF. [http]

**Before promising provider support:** test public anonymous LFS, SSH-issued HTTPS credentials, separate CDN origins, nondefault SSH ports/path quoting, unknown/changed SQLite host keys, expired tokens, batch-per-object errors, missing payloads, malformed pointers, size/hash mismatch, hostile `.lfsconfig`, redirects and DNS changes. A readable Git repository does not prove its LFS payloads are anonymously available.

## 2. Atomic actual-directory publication (conditional Linux option)

### Primitive and operating envelope

`renameat2(..., RENAME_EXCHANGE)` atomically exchanges **two existing names**, including nonempty directories. After exchanging private staging and live, live names the completed new directory and staging names the old one. The stable pathname remains an actual directory; its inode changes. Ordinary rename cannot replace a nonempty destination directory. Neither moving live aside first nor copying files into live meets the atomic switch requirement. [rename]

This Linux-specific syscall appeared in **Linux 3.15**; usable flags also require filesystem support. Both paths must be on the **same mounted filesystem**, not merely devices with matching `st_dev`; different mount points can fail `EXDEV` even for the same underlying filesystem. Live must not itself be a mount point. Put staging beside live, outside the exported site, under manager-owned non-symlink parents. Prefer a qualified local filesystem; do not promise arbitrary NFS/FUSE/overlay/container-volume behavior. The manual's filesystem-version table under `RENAME_NOREPLACE` is **not an exchange support matrix**. [rename]

Probe exchanges between disposable nonempty directories on the **actual publication mount**, verify the result, and retain runtime error handling. Missing syscall, unsupported flags/filesystem, cross-mount placement, seccomp restrictions, permissions, busy mount points, and storage errors must fail closed. **No two-rename, symlink, or in-place-copy fallback.** Capability probing does not prove power-loss durability. The platform/storage choice and acceptance tests remain prerequisites. [rename] [fsync]

Go seam: isolate Linux-specific publication behind a small operation using `golang.org/x/sys/unix.Renameat2` and `unix.RENAME_EXCHANGE`, preferably relative to trusted open parent directory FDs. Keep coordination/journal/cleanup separate from the syscall adapter so unsupported/error/crash paths are testable. No external `mv`/`rename` program or libc shell wrapper is necessary. Other platforms need independently researched atomic directory-swap primitives or an explicit unsupported result, not assumed portable `os.Rename` parity. [unix] [rename]

### First publication, crash recovery, and no retained releases

**Recommended state model, not an already proven recovery implementation:** serialize publication and recovery per deployment (also across service processes). Record a unique **attempt identity**, old/new directory identity, and intended operation durably in SQLite before mutation; commit ID alone is insufficient because hard re-deploy can publish the same commit again. Keep recovery metadata outside tracked/published content; choose and validate an identity mechanism for the selected filesystem.

1. Fully export, hydrate and validate staging before switching. If machine-crash durability is required, sync completed file contents and directory entries bottom-up, and persist the intent record before publication. File `fsync` alone does not persist its parent directory entry. [fsync]
2. **First publication:** live is absent, so exchange cannot work. Rename the complete staging directory into the absent live name atomically; under exclusive ownership/locking, preferably use `RENAME_NOREPLACE` to reject unexpected occupants. Do not expose an empty placeholder first. Sync the affected parent directory/directories. [rename] [fsync]
3. **Replacement:** exchange once; sync affected parent directories, then reconcile/persist the new live attempt identity. The filesystem rename and SQLite commit are not one transaction. A crash between them needs observation of directory identities, not an assumption that the database phase proves which directory is live. Never blindly repeat exchange after restart: it would swap the old directory back into live. [rename] [fsync]
4. Once new live is established durably, delete only the positively identified old directory now at the private staging name. Record/retry cleanup failures without rolling live back. Resume interrupted deletion on restart. Never recursively delete an ambiguous path or infer “safe to delete” solely from a filename. Preserve the observed valid live publication and stop for reconciliation if evidence is inconsistent.
5. Crash before switching: discard the identified unfinished/new staging attempt; old live stays. Crash after switching but before DB update: identify new live, reconcile status and clean old staging. Crash during cleanup: preserve live and finish bounded cleanup. Test process-kill and power-loss cases separately; atomic namespace change is **not** a standalone crash-durability guarantee. Network filesystems can even report rename errors after the server already performed it, another reason to narrow the storage contract. [rename] [fsync]

“No previous release retention” can mean **no rollback/history retention**, with immediate best-effort deletion and restart cleanup. An exchange necessarily leaves the old tree at the staging name until deletion completes; temporary overlap and disk space for old + new are unavoidable here. Literal zero old-directory existence immediately after the switch is incompatible with this mechanism. Persistent cleanup failures must be visible, not silently become release retention.

### What the atomic switch does not guarantee

Open FDs remain attached to their original objects across rename. An already-open old file can continue being read even after unlink until its last reference closes, so reclaimed disk space can lag cleanup. A server holding an old directory FD does not automatically follow the new live pathname; subsequent old-tree child opens can fail once cleanup deletes entries. [rename] [unlink]

Atomic publication is one namespace operation, **not a transaction over all file opens or browser requests**. HTML opened before the exchange and assets requested afterward can come from different releases; a multi-open request can likewise cross the switch. External web-server caching, directory-FD reuse and bind mounts need validation. Guaranteed request/session-level snapshot consistency requires hosting cooperation/versioned assets or delaying old-tree cleanup; it cannot be promised by exchange alone with immediate deletion and hosting out of scope. This is a constraint/tradeoff to document, not grounds for a non-atomic fallback. [rename] [unlink]

## Primary sources

Protocol documents/source on `main` were inspected on the research date; tagged go-git and Go SSH source links are version-specific. No provider interoperability or filesystem/crash tests were executed for this note.

[v5]: https://github.com/go-git/go-git/blob/v5.19.3/COMPATIBILITY.md
[pointer]: https://github.com/git-lfs/git-lfs/blob/main/docs/spec.md
[batch]: https://github.com/git-lfs/git-lfs/blob/main/docs/api/batch.md
[basic]: https://github.com/git-lfs/git-lfs/blob/main/docs/api/basic-transfers.md
[discovery]: https://github.com/git-lfs/git-lfs/blob/main/docs/api/server-discovery.md
[auth]: https://github.com/git-lfs/git-lfs/blob/main/docs/api/authentication.md
[changelog]: https://github.com/git-lfs/git-lfs/blob/main/CHANGELOG.md#300-24-sep-2021
[ssh-proposal]: https://github.com/git-lfs/git-lfs/blob/main/docs/proposals/ssh_adapter.md
[ssh-transfer]: https://github.com/git-lfs/git-lfs/blob/main/ssh/connection.go
[ssh-adapter]: https://github.com/git-lfs/git-lfs/blob/main/tq/ssh.go
[ssh-session]: https://github.com/golang/crypto/blob/v0.59.0/ssh/session.go
[ssh-client]: https://pkg.go.dev/golang.org/x/crypto/ssh#ClientConfig
[http]: https://pkg.go.dev/net/http#Client
[rename]: https://man7.org/linux/man-pages/man2/rename.2.html
[fsync]: https://man7.org/linux/man-pages/man2/fsync.2.html
[unlink]: https://man7.org/linux/man-pages/man2/unlink.2.html
[unix]: https://pkg.go.dev/golang.org/x/sys/unix#Renameat2
