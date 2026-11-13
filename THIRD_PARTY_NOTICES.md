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

Public repository support adds `github.com/go-git/go-git/v5 v5.19.3` and its
transitive Go module graph. That graph has not been individually inventoried
for distribution here; retain and review all applicable upstream license and
notice files as part of the still-open #26 closure. This addition does not
complete the application's third-party license inventory.

The accepted Serenity WASM malformed-argument memory-leak risk also remains
subject to the mandatory upgrade tracked by
[#25](https://github.com/eslym/deploy-manager/issues/25): verify the published
fix in the actual asset, refresh integrity/provenance and license/advisory
evidence, and rerun the browser interoperability and malformed-argument tests.
No claim is made here that either follow-up is complete or that the browser
artifact has a complete dependency SBOM.
