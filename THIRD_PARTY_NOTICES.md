# Third-party notices and distribution caveats

Administrator bootstrap embeds the exact `@serenity-kit/opaque@1.1.0` ESM
Ristretto browser asset at `lib/assets/opaque.js`. Its SHA-256 is
`1e8d56383a77948e453085b6d4f52dcb9645f8bfff32395738b63d3a2e2fcb52`; the
package's MIT license text is retained at `lib/assets/OPAQUE-LICENSE`. The
published npm tarball's recorded SHA-512 integrity is
`Y6v/+hRMn0MdMEk5+/ArM0vPIiFfFEbdTZc7oAx+cWyvGODGezAQ/sMjXBVogf7NsS9z9EV0Ve5paZCVULuedw==`. The
approved Go implementation is `github.com/bytemare/opaque v0.18.0`; the tested
Go baseline is Go 1.26.11 with `golang.org/x/crypto v0.59.0` and
`golang.org/x/sys v0.50.0`.

This inventory is not a complete license clearance for distribution. The
historical transitive Rust/WASM dependency graph and corresponding license
closure for the published Serenity asset are unavailable. The maintainer's
acceptance of that evidence gap does not waive applicable license and notice
obligations. Follow issue [#26](https://github.com/eslym/deploy-manager/issues/26)
to close the inventory and preserve every applicable upstream license,
copyright, attribution, and patent notice before distributing source or
binaries. Go transitive modules include MIT and Go Authors BSD-3-Clause
licensed works; their upstream notices remain applicable as well.

Public repository support adds `github.com/go-git/go-git/v5 v5.19.3`. The
source-provided license and notice files for the newly added Go modules are
retained verbatim under `third_party/licenses/go-git/`, using module/version
directories. The pinned modules and copied files are:

| Module | Retained upstream files |
| --- | --- |
| `dario.cat/mergo v1.0.0` | `LICENSE` |
| `github.com/Microsoft/go-winio v0.6.2` | `LICENSE` |
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
