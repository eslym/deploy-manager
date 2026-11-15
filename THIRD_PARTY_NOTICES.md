# Third-party notices and distribution caveats

## Current codebase (2026-11-16)

The current version/file index is [third_party/DEPENDENCIES.md](third_party/DEPENDENCIES.md).
It covers all direct and indirect `go.mod` requirements, Go's runtime/toolchain
notice, and a conservative attribution set for the embedded SPA: Svelte,
SvelteKit, clsx, devalue, esm-env, Tailwind CSS, Vite helpers, and Serenity OPAQUE.
Module-root `LICENSE`/`LICENCE`, `NOTICE`, `COPYING`, and `PATENTS` files are
retained verbatim where supplied. This is not a claim that every module in
`go.mod` or the entire contents of every frontend package ship in the executable.

The SPA build bundles the pinned `@serenity-kit/opaque@1.1.0` ESM package,
including its WASM, through Bun/Vite. It no longer serves a standalone
`lib/assets/opaque.js`; bundling transforms JavaScript, so a historical hash of
that removed file is not the hash of the shipped chunk. The package's MIT text
is retained at `lib/assets/OPAQUE-LICENSE` and in the frontend notice directory. The
published npm tarball's recorded SHA-512 integrity is
`Y6v/+hRMn0MdMEk5+/ArM0vPIiFfFEbdTZc7oAx+cWyvGODGezAQ/sMjXBVogf7NsS9z9EV0Ve5paZCVULuedw==`. The
approved Go implementation is `github.com/bytemare/opaque v0.18.0`; the tested
Go baseline is Go 1.26.11 with `golang.org/x/crypto v0.59.0` and
`golang.org/x/sys v0.50.0`.

The combined build also embeds a readable notice bundle from
[`web/static/third-party-notices.txt`](web/static/third-party-notices.txt), served
at `/third-party-notices.txt`. Include this bundle and the retained legal files
with source/binary distributions; this convenience endpoint does not itself
resolve outstanding distribution obligations.

To refresh after a dependency change, install the pinned Go modules and run
`bun install --frozen-lockfile` in `web/`, then run from the repository root:

```sh
bun scripts/update-notices.mjs
bun scripts/update-notices.mjs --check
```

Use the Go toolchain pinned by `go.mod`; rebuild the frontend and Go binary to
embed updated notices. The script checks installed frontend versions against
`web/bun.lock` and preserves historical retained files rather than deleting them.
It collects module/package-root legal files only, not a recursive legal audit
or an exact link/bundle SBOM. Additional nested, generated, native, and vendored
attributions remain part of #26. The frontend attribution set must be reviewed
when imports or the build change; it is not inferred solely from the lockfile.

This inventory is not a complete license clearance for distribution. The
historical transitive Rust/WASM dependency graph and corresponding license
closure for the published Serenity asset are unavailable. The maintainer's
acceptance of that evidence gap does not waive applicable license and notice
obligations. Follow issue [#26](https://github.com/eslym/deploy-manager/issues/26)
to close the inventory and preserve every applicable upstream license,
copyright, attribution, and patent notice before distributing source or
binaries. Go transitive modules include MIT and Go Authors BSD-3-Clause
licensed works; their upstream notices remain applicable as well.

Typed query mappings add `entgo.io/ent v0.14.5`; its upstream Apache-2.0
license is retained at `third_party/licenses/entgo.io/ent@v0.14.5/LICENSE`.
Ent's generated runtime schema package depends on `ariga.io/atlas
v0.32.1-0.20250325101103-175b25e1c1b9`; its upstream license is retained at
`third_party/licenses/ariga.io/atlas@v0.32.1-0.20250325101103-175b25e1c1b9/LICENSE`.
The resolved Ent/Atlas graph is pinned by `go.mod`/`go.sum`; applicable source
license and notice files for newly resolved modules are retained under
`third_party/licenses/`. The Ent code generator itself is pinned to v0.14.5 in
the schema's `go:generate` directive and is invoked only by maintainers; no
generator executable or source checkout is needed to build or run the program.
Ent schema reconciliation is not invoked; goose remains the sole migration
executor. `github.com/go-openapi/inflect v0.19.0` includes an MIT/Expat license
in its module archive's `LICENCE` file, retained verbatim at
`third_party/licenses/github.com/go-openapi/inflect@v0.19.0/LICENCE`.
The repository-wide issue #26 distribution inventory remains separate.

## Retained dependency-family records

The records below describe earlier dependency additions. Use the current index
above for selected versions and exact notice paths; older version directories
are retained as historical source notices, not evidence those versions ship.

Public repository support adds `github.com/go-git/go-git/v5 v5.19.3`. The
source-provided license and notice files for the newly added Go modules are
retained verbatim under `third_party/licenses/go-git/`, using module/version
directories. The pinned modules and copied files are:

| Module | Retained upstream files |
| --- | --- |
| `dario.cat/mergo v1.0.0` | `LICENSE` |
| `github.com/Microsoft/go-winio v0.6.4` | `LICENSE` |
| `github.com/ProtonMail/go-crypto v1.1.6` | `LICENSE` |
| `github.com/cloudflare/circl v1.6.3` | `LICENSE` |
| `github.com/cyphar/filepath-securejoin v0.6.1` | `LICENSE.BSD`, `LICENSE.MPL-2.0`, `COPYING.md` |
| `github.com/emirpasic/gods v1.18.1` | `LICENSE` |
| `github.com/go-git/gcfg v1.5.1-0.20230307220236-3a3c6141e376` | `LICENSE` |
| `github.com/go-git/go-billy/v5 v5.9.2` | `LICENSE` |
| `github.com/go-git/go-git/v5 v5.19.3` | `LICENSE` |
| `github.com/golang/groupcache v0.0.0-20241129210726-2c02b8208cf8` | `LICENSE` |
| `github.com/jbenet/go-context v0.0.0-20150711004518-d14ea06fba99` | `LICENSE` |
| `github.com/kevinburke/ssh_config v1.2.0` | `LICENSE` |
| `github.com/pjbgf/sha1cd v0.7.0` | `LICENSE` |
| `github.com/sergi/go-diff v1.3.2-0.20230802210424-5b0b94c5c0d3` | `LICENSE` |
| `github.com/skeema/knownhosts v1.3.1` | `LICENSE`, `NOTICE` |
| `github.com/xanzy/ssh-agent v0.3.3` | `LICENSE` |
| `golang.org/x/net v0.62.0` | `LICENSE` |
| `gopkg.in/warnings.v0 v0.1.2` | `LICENSE` |

This adds the upstream texts for the go-git graph, but is not a legal review or
a complete distribution inventory. Issue #26 remains open for full dependency
and asset license closure, attribution review, and distribution packaging.

The accepted Serenity WASM malformed-argument memory-leak risk also remains
subject to the mandatory upgrade tracked by
[#25](https://github.com/eslym/deploy-manager/issues/25): verify the published
fix in the actual asset, refresh integrity/provenance and license/advisory
evidence, and rerun the browser interoperability and malformed-argument tests.
No claim is made here that either follow-up is complete or that the browser
artifact has a complete dependency SBOM.

Git LFS utility reuse is pinned to `github.com/git-lfs/git-lfs/v3 v3.8.1` and
is limited to the pointer parser and committed-attribute parser/pattern
primitives behind application-owned adapters. The LFS client, config discovery,
credentials, subprocesses, and transfer orchestration are not imported. The
upstream MIT notice (and its separately stated Go Authors BSD notice) is
retained verbatim at
`third_party/licenses/git-lfs/github.com/git-lfs/git-lfs/v3@v3.8.1/LICENSE.md`.
License files for the newly selected transitive module graph are retained under
the corresponding module/version directories there:

| Module | Retained upstream license file |
| --- | --- |
| `github.com/alexbrainman/sspi v0.0.0-20210105120005-909beea2cc74` | `LICENSE` |
| `github.com/avast/retry-go v3.0.0+incompatible` | `LICENSE` |
| `github.com/dpotapov/go-spnego v0.0.0-20210315154721-298b63a54430` | `LICENSE` |
| `github.com/git-lfs/gitobj/v2 v2.1.1` | `LICENSE.md` |
| `github.com/git-lfs/go-netrc v0.0.0-20250218165306-ba0029b43d11` | `LICENSE` |
| `github.com/git-lfs/pktline v0.0.0-20210330133718-06e9096e2825` | `LICENSE.md` |
| `github.com/git-lfs/wildmatch/v2 v2.0.1` | `LICENSE.md` |
| `github.com/hashicorp/go-uuid v1.0.2` | `LICENSE` |
| `github.com/jcmturner/aescts/v2 v2.0.0` | `LICENSE` |
| `github.com/jcmturner/dnsutils/v2 v2.0.0` | `LICENSE` |
| `github.com/jcmturner/gofork v1.0.0` | `LICENSE` |
| `github.com/jcmturner/goidentity/v6 v6.0.1` | `LICENSE` |
| `github.com/jcmturner/gokrb5/v8 v8.4.2` | `LICENSE` |
| `github.com/jcmturner/rpc/v2 v2.0.3` | `LICENSE` |
| `github.com/jmhodges/clock v1.2.0` | `LICENSE` |
| `github.com/klauspost/compress v1.20.4` | `LICENSE` |
| `github.com/leonelquinteros/gotext v1.7.2` | `LICENSE` |
| `github.com/mattn/go-isatty v0.0.24` | `LICENSE` |
| `github.com/olekukonko/ts v0.0.0-20171002115256-78ecb04241c0` | `LICENCE` |
| `github.com/pkg/errors v0.9.1` | `LICENSE` |
| `github.com/rubyist/tracerx v0.0.0-20170927163412-787959303086` | `LICENSE` |
| `github.com/ssgelm/cookiejarparser v1.0.1` | `LICENSE.md` |

This records upstream files for the selected graph, not a legal review or a
complete distribution inventory. Continue the dependency and advisory review
tracked by #26 before distribution.

Database initialization embeds reviewed SQL migrations executed in-process by
`github.com/pressly/goose/v3 v3.30.1` (MIT). The existing `database/sql`
SQLite driver remains `github.com/mattn/go-sqlite3`, upgraded from v1.14.24 to
v1.14.53; its bundled engine reports SQLite 3.54.1, which includes the WAL-reset
fix introduced in 3.51.3. Goose's linked dependency notices are retained under
`third_party/licenses/goose/`:

| Module | Retained upstream license file |
| --- | --- |
| `github.com/pressly/goose/v3 v3.30.1` | `LICENSE` |
| `github.com/mattn/go-sqlite3 v1.14.53` | `LICENSE` |
| `github.com/mfridman/interpolate v0.2.0` | `LICENSE.txt` |
| `github.com/sethvargo/go-retry v0.5.0` | `LICENSE` |
| `go.uber.org/multierr v1.11.0` | `LICENSE.txt` |

This is the selected migration/runtime dependency scope, not a complete license
inventory of the entire module graph. The exact runtime SQLite version is
checked at startup so a build using an older system SQLite fails closed.
