# Reusing Git LFS utilities in the deployment manager

Research date: 2026-11-12. Planning only; no application implementation or dependency changes.

## Answer and recommended boundary

**Yes, Git LFS exports useful Go utilities, especially pointer decoding and attribute parsing. No, its exported client/transfer stack is not a suitable drop-in, subprocess-free library for this manager.** This is not merely a conclusion about its CLI: several packages can be imported and called without running external programs, but others expose interfaces that cannot actually be implemented outside their package, invoke Git during construction, or hide the transport behind private fields.

Most importantly, the module itself explicitly says: **“does not maintain a stable API or ABI” and “Please do not import this module outside of the Git LFS project.”** Exported does not mean supported as an SDK. Pinning a version would be necessary, not sufficient, for safe reuse. [S1]

Recommended integration boundary:

1. Keep go-git responsible for fetching and reading the selected commit's trees/blobs.
2. Own a small, tested **tracked-attributes evaluator**, with low-level parser/pattern reuse considered separately. Do not blindly adopt go-git's complete attributes matcher: the current stable tagged implementation has selection-relevant defects described below.
3. Put pointer decoding behind a manager-owned adapter. `lfs.DecodePointer` is technically reusable without subprocesses; choose between a pinned unsupported dependency and a small attributed extraction/spec-based implementation after weighing the dependency graph and compatibility burden.
4. Own LFS endpoint approval, HTTP client, credentials, resource limits, integrity verification, cache and staging. Do not adopt `lfsapi`/`tq` as the runtime orchestration layer.
5. **Do not decide LFS transport scope here.** HTTPS basic transfers, SSH authentication followed by HTTPS, and pure SSH are distinct capabilities. Which must be supported remains a user decision. If an SSH capability is selected, use an in-process SSH session with the managed signer and SQLite host-trust callback, not Git LFS's subprocess transport.

These recommendations preserve required LFS hydration and **attributes-driven selection**, rather than scanning for and hydrating every pointer-looking blob. The proposed boundary is an engineering recommendation, not a new confirmed feature scope.

## Sources and versions

Checked the official latest-release metadata and tagged source for **git-lfs v3.8.1** (published 2026-10-22) and stable **go-git v5.19.3** (published 2026-10-04). Git attribute semantics are cited from Git **v2.56.1**. All code links below are tag-pinned; no conclusions depend on an unversioned development branch. Git LFS's module path is `github.com/git-lfs/git-lfs/v3` and its minimum Go directive is `go 1.25.0`. [S1] [S2] [S3] [S4]

## Import and API inventory

Paths in this table are import paths, not command names. “Public” means importable/exported in Go, **not** a stable upstream library contract.

| Capability | Actual package/API | Assessment |
| --- | --- | --- |
| Pointer decode/encode | `github.com/git-lfs/git-lfs/v3/lfs`: `DecodePointer(io.Reader) (*Pointer, error)`, `DecodeFrom(io.Reader) (*Pointer, io.Reader, error)`, `NewPointer(string, int64, []*PointerExtension) *Pointer`, `EncodePointer(io.Writer, *Pointer) (int, error)` | Public, usable low-level calls; no Git subprocess needed on these reader-based paths. Imports the whole `lfs` package, not a tiny standalone pointer package. [S5] |
| Pointer representation | `lfs.Pointer{Version, Oid, Size, OidType, Extensions, Canonical}` | Parser validates syntax; the struct/constructor are not a validation boundary and payload integrity is separate. [S5] |
| Attributes | `github.com/git-lfs/git-lfs/v3/git/gitattr`: `ParseLines(io.Reader) ([]Line, string, error)`, `NewMacroProcessor() *MacroProcessor`, `(*MacroProcessor).ProcessMacros([]Line)`, `ProcessLines([]Line, bool) []PatternLine` | Public, useful low-level parser/pattern/macro primitives. `git/gitattr` is **not** a Go `internal` package despite its location. Need manager-owned hierarchy, state resolution and conformance tests. [S6] [S7] |
| Attribute tree | `gitattr.New(*gitobj.ObjectDatabase, *gitobj.Tree) (*Tree, error)`, `NewFromReader(*MacroProcessor, io.Reader) (*Tree, error)`, `(*Tree).Applied(string) []*Attr` | Public; `New` is tied to a different object DB (`github.com/git-lfs/gitobj/v2`), not go-git storage. Reader constructor does not provide an exported child-tree assembly API. `Applied` returns ordered assignments, not a final attribute map. [S8] |
| go-git attributes | `github.com/go-git/go-git/v5/plumbing/format/gitattributes`: `ReadAttributes(io.Reader, []string, bool) ([]MatchAttribute, error)`, `ParseAttributesLine(string, []string, bool) (MatchAttribute, error)`, `NewMatcher([]MatchAttribute) Matcher`; `Matcher.Match([]string, []string) (map[string]Attribute, bool)` | Public, already in the chosen dependency family. Parser/pattern primitives are candidates; complete matcher is not reliable enough as-is (see probes). [S9] [S10] |
| HTTP API | `github.com/git-lfs/git-lfs/v3/lfsapi.NewClient(lfshttp.Context) *Client`; `Client.NewRequest(string, lfshttp.Endpoint, string, interface{}) (*http.Request, error)`; `Client.Do(*http.Request) (*http.Response, error)` | Public, but constructor calls default endpoint discovery, including external Git; private underlying client prevents clean transport injection. [S11] [S12] [S13] |
| HTTP machinery | `github.com/git-lfs/git-lfs/v3/lfshttp.NewClient(Context) *Client`; `NewContext(*git.Configuration, map[string]string, map[string]string) Context`; `Client.HttpClient(*url.URL, creds.AccessMode) (*http.Client, error)` | Some genuine injection seams exist. Still not an isolated policy-enforcing LFS downloader. See redirect and SSH resolver limitations. [S14] [S15] [S16] |
| Batch | `github.com/git-lfs/git-lfs/v3/tq.Batch(Manifest, Direction, string, *git.Ref, []*Transfer) (*BatchResponse, error)` | Public function but coupled to sealed manifest machinery. `BatchClient.Batch(remote string, bReq *batchRequest)` mentions an **unexported** request type. Not an independently implementable outside-package batch seam. [S17] [S18] |
| Downloads | `tq.NewManifest(*fs.Filesystem, *lfsapi.Client, string, string) Manifest`; `tq.NewTransferQueue(Direction, Manifest, string, ...Option) *TransferQueue` | Public entry points, but concrete basic download adapter is private and manifest includes private methods/types. More than a small HTTP utility. [S18] [S19] |
| Hashing | `github.com/git-lfs/git-lfs/v3/tools.NewLfsContentHash() hash.Hash`, `NewHashingReader(io.Reader) *HashingReader`, `(*HashingReader).Hash() string` | Public, callable low-level helpers, but little value over standard `crypto/sha256`, `io.MultiWriter`/`io.TeeReader`. [S20] |
| Pure SSH | `github.com/git-lfs/git-lfs/v3/ssh.NewSSHTransfer(config.Environment, config.Environment, *SSHMetadata, string) (*SSHTransfer, error)` | Public but immediately starts a local SSH executable; no public in-process dialer/signer/host-key callback seam. [S21] [S22] |

`internal/testutil` is an actual import-restricted package; the useful APIs above are not inside `internal`. Conversely, an exported interface containing private methods or private parameter types can still be unusable as an external substitution seam. `tq.Manifest` contains `batchClient`, `getAdapterNames`, `isCustomAdapter`, and `Upgrade() *concreteManifest`; none form an externally implementable manifest contract. [S18]

### Pointer reuse: useful, with a wrapper

`DecodePointer` validates supported version aliases, lowercase 64-hex SHA-256 OIDs, nonnegative signed-64-bit size, extension syntax and duplicate extension priorities. `Canonical` reports byte-for-byte agreement with the encoder; successful decode is not necessarily canonical. `NewPointer` merely constructs a struct; it does not check an arbitrary caller-supplied OID/size. There is no exported general `Pointer.Validate` in this file. [S5]

Important integration details:

- Use the reader API with **complete, bounded blob bytes**, e.g. `bytes.NewReader`, rather than `DecodePointerFromBlob`: that overload wants a `gitobj.Blob`, not a go-git blob. Git LFS pointers must be **less than 1024 bytes**; the file/blob overloads check this, but `DecodeFrom` reads one buffer and does not first prove the entire input is below that cutoff. It uses a single `reader.Read`, not `io.ReadFull`/`ReadAll`, so arbitrary short-reading streams are not a safe direct input. Check the original blob size and fully buffer within the pointer limit before parsing. [S5] [S23]
- Empty input decodes as an empty object pointer; empty files are explicitly a special case in the specification. Do not make these trigger unnecessary network hydration. [S5] [S23]
- Decode is not proof that downloaded content matches either hash or declared size. Stream verification is still required.
- Extension support needs an explicit compatibility policy. Parsing extension metadata does not execute/reverse extension transformations. Under the no-external-programs constraint, reject unsupported nonempty extension chains rather than silently publishing an intermediate object. The pointer format represents extensions, and the extension specification describes their transformation chain. [S5] [S24]
- Decide and test treatment of an attributes-selected file containing ordinary content, a malformed pointer, or a noncanonical pointer. Do not silently change publication behavior merely because the CLI parser is permissive.

## Attributes-driven selection: prerequisites and candidates

### Required semantic boundary

For reproducible export of a commit, the proposed attribute input is **tracked `.gitattributes` blobs in that same commit**, at the repository root and every ancestor directory of each file. Do not read machine-global/system files, the process's `core.attributesFile`, user home configuration, cached worktree attributes, or `.git/info/attributes`. This is a deliberately narrower, deterministic source scope than native Git's general worktree/index/global lookup. It should be confirmed as the manager's precise scope rather than assumed to mean every Git attributes source. Native Git's documented precedence and source distinctions explain why the restriction matters. [S4]

The evaluator must:

1. Collect attribute blobs directly from the immutable target tree, before hydration; preserve their directory domains and within-file order. Descendant files override ancestor files **per attribute**; later matching lines override earlier ones. An unrelated later `text` assignment must not suppress an earlier `filter=lfs`. [S4]
2. Keep four distinct states: Set, Unset (`-filter`), valued (`filter=lfs`), and Unspecified (`!filter`). Explicit Unspecified is an overriding result, **not** permission to fall back to an ancestor value. Select only final valued `filter=lfs`; bare `filter`, `-filter`, `!filter`, another driver and absent `filter` are not LFS selection. [S4] [S9] [S25]
3. Support root-defined macros, expansion semantics and precedence, and inherited use from nested directories; do not permit nested macro definitions. Account for built-in `binary`, recursive macro behavior/cycle termination and assignment order, rather than treating a macro as a textual include. Git only expands a macro when it is Set; macro definitions may assign Unset/Unspecified states to other attributes. [S26] [S27]
4. Handle quoted/C-escaped patterns, slash/domain anchoring, basename patterns, `**`, directory non-recursion, and forbidden negative patterns. Tests must cover these independently of whichever parsing/matching implementation is selected. [S4]
5. Bound attribute file size, rule counts, hierarchy depth and matcher work; fail clearly on unsupported/invalid rules under an agreed compatibility policy. Read only tracked regular files and preserve the existing symlink/submodule rejection rule. Do not hydrate `.gitattributes` to discover its own rules or run arbitrary Git filters.

The last item is a manager safety recommendation, not a promise made by either library. Native Git's default machine-local sources must remain excluded even if a reused library offers convenience loaders for them.

### go-git v5.19.3: useful primitives, unsafe complete matcher

`ReadAttributes` accepts a directory domain and `allowMacro`; its `Attribute` interface preserves Unset/Unspecified/value distinctions. `ReadPatterns` walks a filesystem and enables macros only at the root. For commit-tree export, prefer explicit reads of tracked blobs, not a scan of mutable workspace files. Do not call `LoadGlobalPatterns` or `LoadSystemPatterns`. [S9] [S28]

The complete `NewMatcher(...).Match(...)` implementation is not a safe shortcut. It walks from high to low priority but **unconditionally overwrites** results from lower-priority matches, and its early-return condition compares the number of requested names with the number of *all* returned attributes rather than checking that the requested names are resolved. Its macro expansion copies one level only. [S10]

Focused external-package probes against the actual tag reproduced:

| Rules (file order) | Query | Observed | Correct selection-relevant outcome |
| --- | --- | --- | --- |
| `*.bin filter=lfs`; `x.bin -filter` | all attributes for `x.bin` | `filter=lfs` | Unset: do not hydrate |
| Same rules | only `filter` | Unset | This simple case works, but does not establish correctness |
| `*.bin filter=lfs`; `x.bin text` | only `filter` | Missing `filter` | `filter=lfs`: hydrate if blob is a valid pointer |
| `[attr]one two`; `[attr]two filter=lfs`; `x.bin one` | all attributes | Missing `filter` | Recursive expansion must reach `filter=lfs` |

Thus **requesting only `filter` is not a workaround**. Parser/pattern reuse remains possible under an application-owned resolution algorithm, but needs its own conformance suite. The parser's `unquote` also strips quotes without decoding C escapes; do not infer full Git quoted-path compatibility from its existence. [S9] [S10]

### Git LFS `git/gitattr`: stronger candidate for parsing, not a ready-made go-git evaluator

`ParseLines` returns `PatternLine`/`MacroLine` interfaces and line-ending information. Each `PatternLine.Pattern()` is a `*wildmatch.Wildmatch`, configured with `Basename`, `SystemCase` and `GitAttributes`. Quoted patterns go through `strconv.Unquote`. `Attr` contains `K`, `V` and `Unspecified`; `-filter` becomes `V="false"`, while `!filter` has `Unspecified=true`. [S6]

`Tree.New` recursively reads nested attributes from a `gitobj` database, and `Applied` appends assignments in root-to-descendant order. An external caller can reduce the sequence with last-assignment-wins per key, preserving `Unspecified`. However, importing this DB model solely for attributes creates a second object-storage integration. `NewFromReader` is useful for one layer but its `Tree.children`/`lines` fields are private. `FindSpecialAttributes` explicitly loads system/user/repository-local attributes and must not be called for the proposed tracked-only scope. [S8]

`MacroProcessor` supports built-in `binary` and expansion of macro assignments, but `ProcessLines` copies stored macro attributes **without recursively processing them**. It also expands an Unspecified macro into Unspecified assignments, which must be reconciled with native Git's Set-only expansion rule. It should not be described as a complete Git-compatible attributes engine without additional work. Reuse parser/pattern primitives behind an owned evaluator, or maintain a narrowly extracted implementation with tests; neither candidate eliminates the need for semantic ownership. [S7] [S26] [S27]

The separate `filepathfilter` package implements include/exclude path filtering, not full attributes state/macros/precedence. Its `Allows` result cannot determine whether effective `filter` is `lfs`. [S29]

## HTTP, authentication and transfer-stack coupling

### What can genuinely be injected?

- `lfshttp.Context` is externally implementable: `GitConfig() *git.Configuration`, `OSEnv() config.Environment`, `GitEnv() config.Environment`. `NewContext` accepts explicit maps; this can avoid reading ambient configuration on selected low-level paths. `config.NewFrom` also takes map values, but is documented **testing only**, and prints supplied Git values while materializing them—bad for production secrets. [S14] [S30]
- `creds.CredentialHelper` is externally implementable (`Fill(Creds) (Creds,error)`, `Reject(Creds) error`, `Approve(Creds) error`), and `lfsapi.Client.Credentials` is public. This is a real way to substitute credential lookup, but does not replace transport or constructor behavior. [S11] [S31]
- `lfsapi.Client.Endpoints` accepts an implementable `EndpointFinder`. **But replacing it after `NewClient` is too late to avoid the default constructor's Git calls**: `NewEndpointFinder` calls `git.GitDir()` when necessary and calls `git.RemoteList()` regardless. The latter executes `git remote`. [S11] [S13] [S32]
- `lfshttp.Client.SSH` looks injectable, but `SSHResolver.Resolve(Endpoint,string)` returns **unexported `sshAuthResponse`**. An unrelated external package cannot declare that method signature with a normal custom implementation. The underlying HTTP client inside `lfsapi.Client` is also private, with no public constructor argument for an application-owned HTTP client. [S11] [S12] [S33]
- A standalone `lfshttp.Client.HttpClient(url,mode)` returns the cached `*http.Client`, so an external caller can technically alter that object's `Transport`. This is not equivalent to `lfsapi` exposing an injected downloader, and does not neutralize the outer library's redirect logic. No sufficiently complete, clean substitution seam was found for the full stack. [S15] [S16]

### External programs and ambient configuration

Source-established call paths (not executed during this research):

| Entry/path | External/ambient behavior |
| --- | --- |
| `config.New` / `NewIn` → delayed Git environment → configuration sources | `git.Configuration.gitConfig` runs `git config --includes ...`; `.lfsconfig` is an optional config source. [S30] [S34] |
| `lfsapi.NewClient` → default endpoint finder | Git discovery as above, even if endpoint finder is replaced afterward. [S11] [S13] [S32] |
| Default credentials → command helper / askpass | `subprocess.ExecCommand("git", "credential", subcommand)`; askpass executes configured helper programs. Helpers can have further subprocess behavior. [S31] |
| `lfshttp` default SSH resolver | Local SSH executable runs remote `git-lfs-authenticate`; its JSON headers/URL feed HTTPS requests. [S33] [S35] |
| `ssh.NewSSHTransfer` | Local SSH executable runs remote `git-lfs-transfer`; constructor starts it immediately. [S21] |
| Manifest configuration / custom adapters | Reads transfer-agent settings and registers process-backed custom transfer adapters. [S18] [S36] |
| Scanner construction | `NewPointerScanner` calls `git.NewObjectScanner`, whose default constructor discovers `GitCommonDir` through external Git before opening a `gitobj` database. Object reads themselves use that database, not a `cat-file` subprocess; `NewObjectScannerFrom` accepts an existing `gitobj` DB. Prefer go-git tree iteration rather than a second storage integration. [S37] [S43] [S44] |

Importing a package that contains `os/exec` is **not** proof that every exported function runs a program. Conversely, replacing credentials alone or using map configuration is **not** proof that an entire client is subprocess-free. Only the bounded low-level paths tested here are recommended for consideration.

### Endpoint/TLS/redirect policy mismatch

The batch API exposes server-supplied object action URLs and per-action headers; basic download is a GET of that URL. Therefore endpoint approval and object-storage/CDN-origin approval are separate checks, before sending any request or credentials. [S38] [S39]

Git LFS's default endpoint finder honors `lfs.url` and per-remote LFS URL settings. Its HTTP constructor permits HTTP as well as HTTPS and reads TLS-verification disabling settings. These are normal CLI configurability features, not the manager's required admin-approved-origin/verified-HTTPS boundary. [S13] [S15]

**Redirect trap:** the underlying `http.Client` uses `CheckRedirect: http.ErrUseLastResponse`, but Git LFS then manually handles 301/302/303/307/308 and follows them in `doWithRedirects`. Merely changing `CheckRedirect` is not proof of a no-redirect policy. [S16] [S40]

An owned transport must reject redirects (including same-origin ones), enforce verified HTTPS and approved endpoint/object origins, gate internal addresses according to explicit admin approval, and avoid credential propagation to unintended origins. Apply this to repository/config proposals, SSH authentication responses and batch action URLs alike; none should self-authorize a destination. The manager also needs deadlines, response/body limits, bounded concurrency, sanitized errors, and careful treatment of proxy/DNS behavior. These are recommendations derived from the stated manager constraints, not claims that upstream supplies that policy.

## Integrity and publication

Git LFS's basic adapter hashes received bytes and compares the result with the transfer OID before renaming its temporary file. Its hashing helper is SHA-256. That is useful reference code, but the basic adapter itself is private. [S19] [S20]

Recommended owned boundary: derive the expected `(SHA-256 OID, byte size)` from the validated selected pointer; validate batch response identities/sizes against those requests; stream into a manager-owned temporary file while bounding bytes, hashing and counting; require **both** exact size and SHA-256 agreement before committing to a cache/staging path. Validate cached content before trusting it for publication under the chosen cache-integrity policy. Handle truncation, overlong bodies, malformed metadata, per-object batch errors and expired actions without publishing partial content. Never treat pointer parse success, HTTP 200, `Content-Length`, or the upload-only `verify` action as download integrity proof. The official basic API's `verify` section is explicitly about uploads. [S38] [S39]

## Pure SSH is not the SSH-authenticated HTTPS flow

The hybrid flow invokes remote `git-lfs-authenticate` to obtain credentials/possibly a URL for HTTPS batch requests. In-process SSH can perform that remote command using the managed signer and SQLite trust checks **if that capability is selected**; the prohibition is on *local* external executables, not on invoking a server-side LFS service over an in-process SSH session. The returned destination still needs admin approval. [S35]

Pure SSH transports object data through remote `git-lfs-transfer`. `ssh.NewSSHTransfer` spawns local SSH and builds a `PktlineConnection` whose reader/writer, packet codec and process fields are all private; `End` waits on its subprocess. There is no exported constructor taking an `io.ReadWriteCloser`, managed signer, `ssh.ClientConfig`, or host-key callback. Therefore these exported methods do **not** offer a useful way to plug in the manager's in-process SSH connection. Reimplement/adapt protocol handling if pure SSH becomes required; do not assume replacing a dialer is enough. [S21] [S22]

This research does not promise either SSH transport, restrict support to HTTPS-only, or authorize automatic fallback between them.

## Focused compile/runtime probes

Scratch module: `/tmp/opencode/lfs-library-research/probe`, module name `example.invalid/lfsprobe`, exact requirements Git LFS `v3.8.1`, go-git `v5.19.3`; toolchain `go1.26.8 linux/amd64`. No project `go.mod` was created or changed. Sources were downloaded from official tagged archives, not cloned using Git. No local Git, SSH or git-lfs executable was invoked for testing.

Commands: `go mod tidy`; `go test -v .`; `go test -c -o /tmp/opencode/lfs-library-research/probe/probe.test .`; then `env PATH=/nonexistent /tmp/opencode/lfs-library-research/probe/probe.test -test.v`. Scratch source and binary are under `/tmp/opencode`; Go used its normal dependency/build caches.

Results: all probe assertions passed, including the four **observed-behavior** matcher cases above (passing assertions intentionally record bugs; they are not a conformance endorsement). Reader-based pointer decoding accepted a canonical pointer and rejected negative size. Git LFS attribute parser/macro primitives plus an explicit last-write reducer preserved `!filter` after a macro-provided `filter=lfs`. The same tests passed with no programs discoverable through `PATH`.

Limitations: no live server, end-to-end hydration, network/authentication, pure-SSH, full attributes conformance or syscall-level subprocess audit was performed. Empty `PATH` corroborates the exercised paths only; it does not prove every imported package function is subprocess-free. High-level client constructors known from source to attempt Git were deliberately not executed.

## Licenses and maintenance cost

Git LFS is MIT licensed, with copyright/permission notice retention required. Its license separately identifies copied Go code in `subprocess`/`tools` under BSD-style terms and warns that dependency modules carry their own licenses. go-git is Apache-2.0. Reuse/extraction must retain applicable notices and review the actual selected transitive dependency licenses; a module-level MIT label is not a complete dependency audit. [S41] [S42]

The Git LFS module graph includes `gitobj`, `wildmatch`, credential/netrc and authentication machinery, compression, internationalization and other CLI-related dependencies. The isolated pointer import compiled, but drew considerably more dependencies than a three-field pointer parser suggests. The inspected `go.mod` is the authoritative inventory; imports plus module graph/minimum Go changes must be rechecked on every upgrade. [S1]

**Bottom line:** useful utilities exist and can save parser/pattern work. Prefer low-level, version-pinned, wrapped or narrowly extracted reuse with tests—not Git LFS's client/config/scanner/transfer orchestration. Before implementation, settle precise tracked-attributes compatibility, malformed/extension-pointer behavior, LFS authentication and transport scope, and enforce the already-required endpoint policy in manager-owned code.

## Primary references

[S1]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/go.mod#L1-L50
[S2]: https://github.com/git-lfs/git-lfs/releases/tag/v3.8.1
[S3]: https://github.com/go-git/go-git/releases/tag/v5.19.3
[S4]: https://github.com/git/git/blob/v2.56.1/Documentation/gitattributes.adoc#L19-L100
[S5]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/lfs/pointer.go#L20-L311
[S6]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/git/gitattr/attr.go#L17-L197
[S7]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/git/gitattr/macro.go#L3-L85
[S8]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/git/gitattr/tree.go#L20-L225
[S9]: https://github.com/go-git/go-git/blob/v5.19.3/plumbing/format/gitattributes/attributes.go#L21-L215
[S10]: https://github.com/go-git/go-git/blob/v5.19.3/plumbing/format/gitattributes/matcher.go#L3-L78
[S11]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/lfsapi/lfsapi.go#L15-L101
[S12]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/lfsapi/client.go#L13-L55
[S13]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/lfsapi/endpoint_finder.go#L25-L135
[S14]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/lfshttp/lfshttp.go#L19-L59
[S15]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/lfshttp/client.go#L58-L172
[S16]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/lfshttp/client.go#L533-L570
[S17]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/tq/api.go#L19-L114
[S18]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/tq/manifest.go#L24-L294
[S19]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/tq/basic_download.go#L290-L341
[S20]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/tools/iotools.go#L52-L84
[S21]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/ssh/connection.go#L15-L87
[S22]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/ssh/protocol.go#L14-L60
[S23]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/docs/spec.md#L7-L66
[S24]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/docs/extensions.md
[S25]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/docs/spec.md#L126-L170
[S26]: https://github.com/git/git/blob/v2.56.1/Documentation/gitattributes.adoc#L1286-L1320
[S27]: https://github.com/git/git/blob/v2.56.1/attr.c#L1092-L1132
[S28]: https://github.com/go-git/go-git/blob/v5.19.3/plumbing/format/gitattributes/dir.go#L23-L137
[S29]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/filepathfilter/filepathfilter.go#L71-L158
[S30]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/config/config.go#L62-L182
[S31]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/creds/creds.go#L29-L355
[S32]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/git/git.go#L431-L447
[S33]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/lfshttp/ssh.go#L84-L193
[S34]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/git/config.go#L177-L247
[S35]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/docs/api/authentication.md#L3-L33
[S36]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/tq/custom.go
[S37]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/lfs/gitscanner_catfilebatch.go#L69-L130
[S38]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/docs/api/batch.md#L5-L161
[S39]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/docs/api/basic-transfers.md#L11-L97
[S40]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/lfshttp/client.go#L313-L408
[S41]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/LICENSE.md#L1-L55
[S42]: https://github.com/go-git/go-git/blob/v5.19.3/LICENSE
[S43]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/git/object_scanner.go#L38-L63
[S44]: https://github.com/git-lfs/git-lfs/blob/v3.8.1/git/git.go#L857-L895
