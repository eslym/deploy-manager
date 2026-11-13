# go-git compatibility for static-site publication

Researched **2026-11-12**. Scope: feasibility, not an implementation or architecture decision. Terminology follows [GLOSSARY.md](../../GLOSSARY.md): one project can have multiple persistent deployments; a release is identified by its resolved commit.

## Conclusion

**Feasible with qualifications on stable go-git for ordinary SHA-1 repositories over SSH or smart HTTPS.** Bare fetches, immutable commit/tree access, in-process SSH credentials, and a custom fail-closed host-key callback provide the necessary primitives. Separate export directories can share one project's object database without native Git worktrees. Export validation, concurrency coordination, SQLite trust records, dashboard warnings, and frozen-tag semantics remain application responsibilities. **Stable v5 cannot fetch SHA-256 repositories and does not implement LFS hydration or linked-worktree management.** [v5-repo] [v5-compat] [v5-auth]

As of this date, GitHub's official releases identify **v5.19.3** (2026-10-04) as latest stable and **v6.0.0-beta.1** (same date) as a prerelease. Both require Go 1.26.0 in their `go.mod`. Do not accidentally treat current `master`/unversioned documentation as v5 support. [releases] [v5-module] [v6-module]

## Evidence and verification boundary

- **Verified below** means inspected first-party documentation, tagged source, or upstream tests; it does not mean provider interoperability or application behavior was exercised.
- Tagged sources were downloaded outside the project under `/tmp/opencode/go-git-research/`. Installed Go: `go1.26.8 linux/amd64`. No project `go.mod` or implementation was created.
- **Executed upstream probe:** in the isolated v6 beta source, `go test . -run '^TestCloneAll$' -count=1` passed (`ok .../v6 0.266s`). This exercises SHA-1/SHA-256 clone against upstream's HTTP/Git test-server matrix, not external providers or SSH host trust. [v6-tests] [v6-test-servers]
- **Application implications** and **alternatives** are deductions from these primitives, not features supplied by go-git. No SQLite integration, dashboard flow, remote-provider test, load test, or race test was performed.

## Stable v5.19.3: requirements

### Bare repository and shared objects

**Verified:** `PlainCloneContext(ctx, path, true, options)` initializes a bare repository. The lower-level `CloneContext(ctx, storage, nil, options)` also permits no working filesystem. Clone only checks out when `r.wt != nil && !NoCheckout`; `Repository.Worktree()` on a bare repository returns `ErrIsBareRepository`. `FetchContext` updates objects/references without needing checkout. A bare repository is not automatically a mirror; `Mirror` changes the refspec to `+refs/*:refs/*`. [v5-repo]

**Application implication:** one disk-backed bare object store per project can feed separate temporary export directories for different deployments, commits, or attempts. Directory isolation does not require copying the object database. Resolve the target once, then read that commit's tree; do not repeatedly consult a moving branch during export. This is an export, not a native linked checkout with an index and independent `HEAD`. [v5-repo] [v5-tree]

**Alternative primitive:** filesystem storage supports Git alternates, and `CloneOptions.Shared` sets up `objects/info/alternates` for a local source. This adds source-lifetime/cleanup dependencies. Moreover, v5's default local/file transport shells out to **local `git`**, so a local shared clone is not automatically compatible with the no-external-binary requirement. Direct tree reads from the shared store avoid that transport. [v5-storage] [v5-repo] [v5-compat]

### Branches, tags, immutable commits, and forced updates

**Verified:** v5's ordinary fetch mapping is `+refs/heads/*:refs/remotes/origin/*`; clone may also create a local branch, which subsequent fetches need not advance. For branch targets, resolve the explicitly mapped remote-tracking reference, not an assumed current local branch or `HEAD`. Fully qualified references avoid branch/tag short-name ambiguity. [v5-repo] [v5-config]

`ResolveRevision` returns a **commit** hash, including lightweight tags and annotated tags directly targeting commits. Important limit: its annotated-tag path calls `Tag.Commit()`, which requires `TargetType == CommitObject`; **nested annotated tags are not recursively peeled by that helper**. Tags ultimately pointing to trees/blobs cannot identify a release commit. Generic peeling can instead read tag objects repeatedly via `TagObject`/`Tag.Target`, ending at a verified `CommitObject`; depth/cycle guards and rejection of non-commit results would be application logic. Persist the full resolved commit ID, not a tag object's ID. [v5-repo] [v5-tag]

**Verified fetch controls:**

- Clone defaults to `AllTags`, but fetch defaults to `TagFollowing`; specify tag behavior deliberately. `AllTags` appends the forced mapping `+refs/tags/*:refs/tags/*`. [v5-options] [v5-remote]
- Explicit mappings such as `+refs/heads/*:refs/remotes/origin/*` and `+refs/tags/*:refs/tags/*`, or `FetchOptions.Force`, allow non-fast-forward updates. In this v5 source, tag destinations bypass the non-fast-forward check even without force; explicit forced refspecs make the intended policy clear. `NoErrAlreadyUpToDate` is a normal fetch result, not a failed deployment. [v5-remote]
- `Prune` can remove mapped refs missing upstream; that is reference pruning, not permission to delete objects needed by frozen targets or previous releases. Fetch is not a transaction publishing all refs together: the implementation applies individual updates and may return an error after some changes. [v5-options] [v5-remote]

**Application implication:** refreshing the cache must not silently refresh a frozen tag target. Store its resolved commit until explicit refresh, as required by the glossary. A force-pushed branch can move to the newly fetched commit while existing releases keep their previous IDs. Retaining IDs in SQLite alone does not keep Git objects reachable for pruning; object retention requires a corresponding policy, such as retained refs or no reclamation of required objects. An arbitrary old commit pin also cannot be assumed downloadable by hash: it must exist locally or be available through refs/server policy. Avoid assuming a shallow/single-branch cache contains all selectable commits. [v5-options] [v5-remote] [v5-prune] [v5-compat]

### Full tracked-tree export and rejection rules

**Verified:** `CommitObject(id).Tree()` and tree/blob readers expose committed content without checkout. Recursive tree traversal does not apply ignore rules or exclude ordinary dotfiles; entries such as `.well-known`, `.nojekyll`, `.gitignore`, and `.gitattributes` remain tracked content. Reading blobs gives stored bytes, not an external checkout-filter pipeline. Repository administration files are separate from the commit tree. [v5-repo] [v5-tree] [v5-file] [v5-blob]

**Required application checks, not automatic export policy:**

1. Inspect **all tree entries**, including directories and gitlinks. Reject modes `Symlink` (`0120000`) and `Submodule` (`0160000`); allow only intended directory/regular/executable modes, with an explicit policy for historical `Deprecated` mode. Merely disabling recursive submodule clone does not reject a gitlink. [v5-mode]
2. Do **not** use `Tree.Files()`/`FileIter` alone to detect submodules: `FileIter.Next()` silently skips submodule entries, while symlinks are returned as blobs containing link-target text. A mode-aware traversal is needed for rejection rather than accidental omission/materialization. [v5-file]
3. Exclude `.git` metadata without excluding all dotfiles. The default `TreeWalker` validates entry names against traversal, `.git`-shaped components, and platform-specific dangerous names. Do not use `SkipPathValidation()` for filesystem output. Independent output-root containment and safe file creation still matter for a custom exporter. [v5-tree]
4. **Completeness trap:** `TreeWalker.Next()` converts failure to load a child tree into `io.EOF`; its own documentation warns missing objects can be skipped. Consequently, treating EOF as proof of a complete export is unsafe. Strict recursion over `Tree.Entries` with checked subtree/blob loads is an alternative, but then path validation must be retained explicitly. Abort on missing objects, unsupported entries, write/close failures, or limits exceeded; never publish partial output. [v5-tree] [v5-blob]

**Implication:** full tracked-tree export is feasible, but there is no single documented v5 `archive` call that both exports and enforces these rules. Exporting raw blobs also deliberately differs from native checkout/archive transformations such as LFS smudge or archive attribute handling. [v5-compat] [lfs-spec]

### SSH repository keys and SQLite-backed host trust

**Verified:** go-git's SSH transport connects through Go networking and `golang.org/x/crypto/ssh.NewClientConn`, then opens a remote SSH session. No local `git`, `ssh`, or `ssh-keygen` binary is needed on this explicit-key SSH path. The remote Git server still provides its upload-pack service. Do not conflate this with v5's external local/file transport. [v5-ssh] [v5-compat]

- **Generated keys:** Go's `ed25519.GenerateKey` plus `ssh.NewSignerFromKey` can provide a signer for `go-git/ssh.PublicKeys{User, Signer}`. `ssh.MarshalAuthorizedKey` exports the public key; `MarshalPrivateKey` and its passphrase variant serialize OpenSSH private keys. [go-ed25519] [crypto-keys] [v5-auth]
- **Imported keys:** `NewPublicKeys(user, bytes, password)` uses `ParsePrivateKey` and, when necessary, `ParsePrivateKeyWithPassphrase`. The underlying parser supports RSA, ECDSA, Ed25519, and legacy DSA in supported PEM/OpenSSH formats; this is not a promise of every encrypted PKCS#8, PuTTY, hardware-token, or OpenSSH configuration format. Supplied key format, encryption scheme, and server algorithm policy need validation. [v5-auth] [crypto-keys]
- Supply authentication explicitly for each relevant fetch/clone. A nil SSH auth defaults to SSH-agent discovery rather than automatically loading this project's repository key. Host trust and repository-key authorization are separate checks. [v5-ssh] [v5-auth]

**Verified integration seam:** `PublicKeys` embeds `HostKeyCallbackHelper`; setting its non-nil `HostKeyCallback` installs that callback rather than constructing the file-based known-hosts default. The callback receives `(hostname string, remote net.Addr, key ssh.PublicKey)` and **must return an error to reject the key**. `PublicKey.Marshal()` permits comparison against stored public-key bytes. A callback can consult SQLite-backed records directly or an immutable snapshot; go-git has no SQLite dependency or schema requirement. With a custom callback, host-key algorithm selection is the caller's responsibility when explicit restrictions are needed. [v5-auth] [v5-ssh] [crypto-client] [crypto-keys]

**Required application behavior:** lookup the approved key set by normalized effective host **and port**; account for any SSH host alias/config rewriting. Reject unknown hosts, changed/unapproved keys, unavailable trust storage, and invalid records. Record a distinguishable failure and untrusted observed fingerprint for the dashboard, while keeping fetch/deployment failed and the last successful publication intact. Showing a warning must never substitute for callback rejection; do not auto-enroll a key, return nil to continue, use `InsecureIgnoreHostKey`, or fall back to a weaker path. Approval/rotation UI, warning persistence, and retry behavior are application work, not go-git functionality. [v5-ssh] [crypto-client]

### Concurrent deployments and cache maintenance

**Verified caveat:** v5 filesystem object storage lazily mutates maps (`index`, cached packfiles) and fetch adds index entries; reference updates/config writes also mutate shared state. Some components have locks (e.g. `ObjectLRU`), but that does **not** establish thread safety of an entire `Repository`/`Storer`. `ExclusiveAccess` means no external filesystem modification; it is not an application lock. Long-lived storage handles may need reindexing after external pack changes. [v5-objects] [v5-cache] [v5-storage]

**Application implications / unverified operating envelope:** do not assume concurrent fetch, resolve, export, or maintenance on one handle is safe merely because commits are immutable. Serialize shared-handle operations, or evaluate independent per-job read handles plus coordinated project writes/maintenance; test the exact policy under `-race` and overlapping operations. Reopen/reindex when needed. Multiple service processes require cross-process coordination, not just a Go mutex. Never delete/repack packs out from under readers, and ensure retained release/frozen-target objects remain available. Distinct output directories prevent checkout interference but do not solve storage races. [v5-objects] [v5-storage] [v5-prune]

**Documentation mismatch:** the v5 compatibility table marks `repack` and `prune` unsupported, but tagged source exposes `Repository.RepackObjects` and loose-object `Prune`. These are not a documented complete `git gc`/`fsck` equivalent. Cache growth, reclamation, disk quotas, object/file-size limits, and failure recovery need separate evaluation. [v5-compat] [v5-repo] [v5-prune]

## Significant compatibility boundaries

| Area | Stable v5.19.3 | Current v6 beta distinction |
| --- | --- | --- |
| Native linked worktrees | Compatibility table explicitly says multiple worktrees unsupported. `Repository.Worktree()` means a repository's ordinary working tree, not `git worktree add`. Some linked-directory opening support is not a full management API. | Partial add/remove/list/open support exists in `x/plumbing/worktree`, with storage interface requirements and incomplete CLI parity. Not needed for metadata-free tree exports. [v5-compat] [v5-repo] [v6-worktree] [v6-compat] |
| SHA-256 repositories | Experimental local init/commit requires `sha256` build tag; network fetch/pull/push unsupported. It does **not** satisfy SHA-256 remote publication. | Runtime SHA-1/SHA-256 object IDs and object-format negotiation exist; upstream `TestCloneAll` covers SHA-256 clone using its test-server matrix. Storage format must agree; this is not transparent SHA-1/SHA-256 history conversion. The SHA-256 example still mentions a build tag, but runtime source/tests are stronger evidence. [v5-compat] [v6-hash] [v6-negotiate] [v6-tests] [v6-example] |
| Git wire protocol | v1/classic transport supported; v2 unsupported according to stable table. Servers must offer a compatible protocol. Git wire protocol v2 is unrelated to the HTTP/2 transport version. | **Conflicting official documentation:** beta's compatibility table still marks wire v2 unsupported, but tagged config defaults to v2 and tagged HTTP/session implementations include v2 discovery/fetch, with upstream tests. Source verifies implementation exists, not universal protocol completeness/provider compatibility. [v5-compat] [v6-compat] [v6-config] [v6-http] [v6-v2-tests] |
| LFS | Unsupported; fetching/exporting pointer blobs does not download the large-file payload. | Still marked unsupported. A pointer's `oid sha256:...` is an LFS object ID, not evidence the Git repository itself uses SHA-256. [v5-compat] [v6-compat] [lfs-spec] |
| Partial clone | `filter` unsupported; do not assume blobless fetching works. | Filtered fetch partially implemented, but no lazy retrieval of withheld objects; a complete export can fail on missing objects. [v5-compat] [v6-compat] |
| Other interoperability | Dumb HTTP unsupported; not full native Git parity for config/extensions, index formats, maintenance, or capabilities. Shallow fetching exists but limits commit availability. | Prerelease improves several areas but still has explicit gaps. Use a controlled cache rather than assuming arbitrary externally maintained repositories are safe to open. [v5-compat] [v6-compat] |

## Alternatives, without selecting a design

1. **Stable v5 + strict commit-tree export:** compatible with a no-local-binaries deployment for the qualified SHA-1/SSH or smart-HTTPS scope; requires application export and coordination logic.
2. **v6 beta evaluation:** possible route to SHA-256 remotes and optional linked worktrees; adds prerelease/API risk and calls for provider-specific protocol/security testing. Do not import beta claims into the stable support contract.
3. **Native Git (optionally Git LFS):** worth evaluating if full worktree, SHA-256, filter, or provider compatibility is essential; entails a changed no-external-binary constraint and its own credential/host-trust integration. Not a drop-in solution under the current requirement.
4. **Explicit LFS rejection or separate in-process LFS client:** rejection avoids silently publishing pointer text; hydration would need an additional implementation of the official LFS protocol, authorization, integrity checks, and limits. go-git alone does not provide it. [lfs-spec]

Before a production commitment, verify representative providers, ordinary/nested/moved tags, force-push/deletion, missing objects, malicious paths, symlinks/gitlinks, repeated subtree hashes, unknown/changed host keys, and overlapping fetch/export/maintenance. This research does not establish those integration guarantees.

## Primary sources

All go-git implementation links below are pinned to the inspected release tags.

[releases]: https://api.github.com/repos/go-git/go-git/releases?per_page=10
[v5-module]: https://github.com/go-git/go-git/blob/v5.19.3/go.mod
[v6-module]: https://github.com/go-git/go-git/blob/v6.0.0-beta.1/go.mod
[v5-compat]: https://github.com/go-git/go-git/blob/v5.19.3/COMPATIBILITY.md
[v5-repo]: https://github.com/go-git/go-git/blob/v5.19.3/repository.go
[v5-options]: https://github.com/go-git/go-git/blob/v5.19.3/options.go
[v5-config]: https://github.com/go-git/go-git/blob/v5.19.3/config/config.go
[v5-remote]: https://github.com/go-git/go-git/blob/v5.19.3/remote.go
[v5-tag]: https://github.com/go-git/go-git/blob/v5.19.3/plumbing/object/tag.go
[v5-tree]: https://github.com/go-git/go-git/blob/v5.19.3/plumbing/object/tree.go
[v5-file]: https://github.com/go-git/go-git/blob/v5.19.3/plumbing/object/file.go
[v5-blob]: https://github.com/go-git/go-git/blob/v5.19.3/plumbing/object/blob.go
[v5-mode]: https://github.com/go-git/go-git/blob/v5.19.3/plumbing/filemode/filemode.go
[v5-storage]: https://github.com/go-git/go-git/blob/v5.19.3/storage/filesystem/storage.go
[v5-objects]: https://github.com/go-git/go-git/blob/v5.19.3/storage/filesystem/object.go
[v5-cache]: https://github.com/go-git/go-git/blob/v5.19.3/plumbing/cache/object_lru.go
[v5-prune]: https://github.com/go-git/go-git/blob/v5.19.3/prune.go
[v5-auth]: https://github.com/go-git/go-git/blob/v5.19.3/plumbing/transport/ssh/auth_method.go
[v5-ssh]: https://github.com/go-git/go-git/blob/v5.19.3/plumbing/transport/ssh/common.go
[crypto-client]: https://github.com/golang/crypto/blob/v0.56.0/ssh/client.go
[crypto-keys]: https://github.com/golang/crypto/blob/v0.56.0/ssh/keys.go
[go-ed25519]: https://pkg.go.dev/crypto/ed25519#GenerateKey
[lfs-spec]: https://github.com/git-lfs/git-lfs/blob/main/docs/spec.md
[v6-compat]: https://github.com/go-git/go-git/blob/v6.0.0-beta.1/COMPATIBILITY.md
[v6-worktree]: https://github.com/go-git/go-git/blob/v6.0.0-beta.1/x/plumbing/worktree/worktree.go
[v6-hash]: https://github.com/go-git/go-git/blob/v6.0.0-beta.1/plumbing/hash.go
[v6-negotiate]: https://github.com/go-git/go-git/blob/v6.0.0-beta.1/plumbing/transport/negotiate.go
[v6-tests]: https://github.com/go-git/go-git/blob/v6.0.0-beta.1/repository_test.go
[v6-test-servers]: https://github.com/go-git/go-git/blob/v6.0.0-beta.1/internal/server/servers.go
[v6-example]: https://github.com/go-git/go-git/blob/v6.0.0-beta.1/_examples/sha256/main.go
[v6-config]: https://github.com/go-git/go-git/blob/v6.0.0-beta.1/config/config.go
[v6-http]: https://github.com/go-git/go-git/blob/v6.0.0-beta.1/plumbing/transport/http/handshake.go
[v6-v2-tests]: https://github.com/go-git/go-git/blob/v6.0.0-beta.1/plumbing/transport/v2_fetch_test.go
