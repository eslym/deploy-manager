# OPAQUE candidate dependency license compatibility

Research date: **2026-11-13**. This is a scoped license/notice investigation for the isolated OPAQUE feasibility candidate, not dependency approval, legal advice, or a project licensing decision. The project has no product Go module yet; the Go versions below are the exact experiment graph in `experiments/opaque-gate/go.mod`/`go.sum` (Go 1.26.11). Existing uncommitted risk-acceptance edits were preserved. The design remains the authority for product scope.

## Conclusion

**The inspected Go candidate graph is MIT-compatible, with mandatory preservation of third-party copyright and license notices; it must not be represented as wholly relicensable MIT.** The packages carry MIT or the Go Authors' BSD 3-Clause license. The latter is permissive and compatible with distributing an MIT application, but its notices/conditions remain applicable to copied or binary-distributed portions. `golang.org/x/*` source trees also carry `PATENTS` files; retain the relevant upstream legal materials with redistributed source/binaries rather than assuming the application's MIT license replaces them.

**The browser candidate cannot receive a complete license-compatibility approval from current evidence.** The exact, verified npm tarball says MIT and contains an MIT `LICENSE`, but its embedded WASM was built from Rust dependencies whose exact historical resolution is unavailable. `opaque-ke`'s upstream manifest is dual-licensed Apache-2.0 OR MIT and its requested Argon2 crate is dual-licensed MIT OR Apache-2.0; those known expressions are compatible with MIT distribution when their conditions are honored. That does not establish the full closure or the notices in the exact WASM. The accepted uncertainty about missing build inputs does not waive copyright/license obligations. Keep this browser closure explicitly unresolved pending an authenticated exact graph/SBOM or a separately approved, fully inventoried artifact.

This is not a finding of any GPL/AGPL/LGPL/MPL dependency in the scoped candidates. No such license was evidenced by the scoped local Go module tree or the inspected npm tarball; the unknown browser graph means absence there is **not** established.

## Candidate production dependency inventory (experiment scope only)

The versions and checksums are from the local module cache after resolving the experiment's `go.mod`; each listed `LICENSE` was inspected in that exact module source. All listed modules are in `go list -m all`'s graph (some are graph-only/transitive rather than necessarily imported by the harness). SPDX labels below identify the standard text, not a claim that every upstream file supplies SPDX metadata.

| Module/version | License and local evidence | Intended scope | MIT redistribution note |
|---|---|---|---|
| `github.com/bytemare/opaque v0.18.0` | MIT; module-cache `LICENSE` | Candidate Go server | Include copyright and full MIT text. |
| `github.com/bytemare/ecc v0.9.0` | MIT; `LICENSE` | Candidate transitive | Same. |
| `github.com/bytemare/hash v0.6.2` | MIT; `LICENSE` | Candidate transitive | Same. |
| `github.com/bytemare/hash2curve v0.5.4` | MIT; `LICENSE` | Candidate transitive | Same. |
| `github.com/bytemare/ksf v0.5.0` | MIT; `LICENSE` | Candidate transitive | Same. |
| `github.com/bytemare/secp256k1 v0.3.0` | MIT; `LICENSE` | Candidate transitive | Same. |
| `filippo.io/edwards25519 v1.2.0` | BSD-3-Clause; `LICENSE` | Candidate transitive | Preserve copyright, conditions, and disclaimer. |
| `filippo.io/nistec v0.0.4` | BSD-3-Clause; `LICENSE` | Candidate transitive | Same. |
| `github.com/gtank/ristretto255 v0.2.0` | BSD-3-Clause; `LICENSE`; multiple credited authors | Candidate transitive | Preserve all copyright notices and BSD terms. |
| `golang.org/x/crypto v0.59.0` | BSD-3-Clause; `LICENSE`, `PATENTS` | Candidate transitive | Preserve license and applicable patent notice materials. |
| `golang.org/x/sys v0.50.0` | BSD-3-Clause; `LICENSE`, `PATENTS` | Candidate transitive | Same. |
| `golang.org/x/net v0.62.0` | BSD-3-Clause; `LICENSE`, `PATENTS` | Transitive module graph | Same. |
| `golang.org/x/term v0.48.0` | BSD-3-Clause; `LICENSE`, `PATENTS` | Transitive module graph | Same. |
| `golang.org/x/text v0.44.0` | BSD-3-Clause; `LICENSE`, `PATENTS` | Transitive module graph | Same. |

The exact graph is the experiment graph only. Future product resolution, platform-specific build selection, and vendored/generated third-party code need their own inventory. Go module source trees should be inspected recursively for additional copyright/notice materials when selecting actual files to vendor or embed; this table is not a claim that every module file was redistributed.

## Exact published browser artifact

The registry tarball was downloaded to temporary storage and inspected without installing npm dependencies. Its file list is `package/LICENSE`, `README.md`, `package.json`, ESM/CJS JS entries, declarations, and a small bin entry. `package.json` declares `"license":"MIT"`; the archive contains the package `LICENSE` with MIT terms. The verified tarball identity and SLSA provenance are documented in [opaque-provenance-verification.md](opaque-provenance-verification.md): SHA-512 `Y6v/+hRMn0MdMEk5+/ArM0vPIiFfFEbdTZc7oAx+cWyvGODGezAQ/sMjXBVogf7NsS9z9EV0Ve5paZCVULuedw==`, 323,964 bytes. The JS bundle embeds the WASM payload. The package-level MIT declaration and included file establish the package's declared license and notice, **not** the complete license closure of compiled Rust code.

At the exact attested source commit, `Cargo.toml` requests `opaque-ke = 4.0.0` with the `argon2` feature and `argon2 = 0.5`; it does not commit `Cargo.lock`. Upstream's `opaque-ke` Cargo manifest declares `Apache-2.0 OR MIT`; the RustCrypto `argon2` crate declares `MIT OR Apache-2.0`. These are dual-license choices, not cumulative requirements: either license option can be used under its terms. Apache-2.0 use requires its license text and preservation of applicable notices/attributions; MIT requires its copyright and permission notice. Do not translate either dependency's license into an assertion that the entire application or WASM is MIT-only.

The exact historical Cargo graph, checksums, features and toolchain for the published WASM are not available in the public authenticated records already investigated. A current reconstruction is expressly not the 2026-02-01 published artifact's dependency closure. Therefore other crate licenses, embedded upstream notices, and any additional attribution/patent conditions cannot be conclusively enumerated for those bytes. See [upstream build-record investigation](opaque-upstream-build-records.md) and the scoped acceptance in [opaque-accepted-risks.md](opaque-accepted-risks.md#published-browser-wasm-build-input-uncertainty). That acceptance concerns evidentiary uncertainty only and explicitly does not waive license obligations.

## Separate tools and scaffold (not production auth dependencies)

| Item | Scope / disposition | License handling |
|---|---|---|
| Wazero Go-WASM backend probe | Rejected alternative, not a selected backend and not in the candidate's `go.mod`. | Do not count it as a product dependency. If its probe source/binary is distributed, inspect its exact pinned version and notices separately. |
| Playwright | Browser-test tooling under `experiments/opaque-gate`, not product runtime. | Do not count it as a product dependency. If test tooling or its browsers are redistributed, inventory the exact package and browser/runtime license notices independently. |
| Sigstore Cosign | External verifier used to validate provenance; no verifier binary is embedded in the candidate artifact. | Tool-use is not a product dependency. If a release workflow or package redistributes Cosign, its own license/notices become relevant. |
| `web/` scaffold and `web/bun.lock` | Existing SvelteKit/Bun scaffold; design says the future dashboard is embedded and does not adopt this as its final frontend. | Deliberately not audited here; lockfile is not itself a complete third-party source/license inventory. Revisit only if scaffold assets/dependencies are selected or shipped. |

No go-git or Git LFS product dependencies are examined here; they are unselected for this authentication license task.

## Distribution and retention checklist

Before shipping a binary or embedded dashboard:

1. Freeze exact dependency versions, checksums, target/features and generated assets for the shipped build; produce and retain an SPDX or CycloneDX inventory with license expressions and source provenance.
2. For every redistributed MIT component, preserve its copyright notice and full permission notice in a third-party notices bundle. For BSD-3-Clause components, also preserve all conditions and disclaimers, and do not use contributors' names to endorse derived products.
3. Include Apache-2.0 license text and required attribution/notice materials for any Apache-licensed redistributed component; review `NOTICE` files and any required modification notices. Keep dual-license choices explicit.
4. Preserve relevant `PATENTS`/patent notices and additional source-level notices from Go modules and vendored/generated code. Ensure notices remain accessible in source distributions and accompany binaries (for example, a bundled `THIRD_PARTY_NOTICES` file or a clearly linked notices page/documentation).
5. For the Serenity WASM, close the actual embedded artifact's Rust dependency and notice inventory; do not substitute the current resolver graph for the historic artifact. If closure cannot be established, do not claim overall license approval for that browser asset.
6. Keep the project's future MIT license separate from third-party licensing. MIT compatibility permits distributing a MIT-licensed application with these permissively licensed dependencies while satisfying their terms; it does not relicense third-party code or authorize removing notices.

This checklist is operational guidance, not a legal guarantee. No project `LICENSE`, package license field, or risk-acceptance record was changed.

## Primary evidence

- Exact experiment graph and checksums: [`experiments/opaque-gate/go.mod`](../../experiments/opaque-gate/go.mod), [`go.sum`](../../experiments/opaque-gate/go.sum), and Go module source `LICENSE`/`PATENTS` files in the pinned module archives/cache.
- Primary upstream Go sources: [Bytemare OPAQUE v0.18.0](https://github.com/bytemare/opaque/tree/v0.18.0), [filippo.io/edwards25519 v1.2.0](https://github.com/FiloSottile/edwards25519/tree/v1.2.0), [filippo.io/nistec v0.0.4](https://github.com/FiloSottile/nistec/tree/v0.0.4), [ristretto255 v0.2.0](https://github.com/gtank/ristretto255/tree/v0.2.0), and [golang.org/x/crypto](https://go.googlesource.com/crypto/).
- Exact npm package metadata/tarball: [registry metadata](https://registry.npmjs.org/@serenity-kit/opaque/1.1.0), [published tarball](https://registry.npmjs.org/@serenity-kit/opaque/-/opaque-1.1.0.tgz), and [verified provenance record](opaque-provenance-verification.md).
- Attested Rust source: [`Cargo.toml` at source commit](https://github.com/serenity-kit/opaque/blob/ca1cb22f03b9e456159ce367e2975124f87f5ad8/Cargo.toml); [opaque-ke crate manifest](https://github.com/novifinancial/opaque-ke/blob/main/opaque-ke/Cargo.toml); [RustCrypto argon2 crate manifest](https://github.com/RustCrypto/password-hashes/blob/master/argon2/Cargo.toml). These manifests establish declared license expressions, not resolved membership in the exact published build.
- Authoritative license texts: [MIT](https://opensource.org/license/mit), [BSD 3-Clause](https://opensource.org/license/bsd-3-clause), and [Apache License 2.0](https://www.apache.org/licenses/LICENSE-2.0).
- Scope/uncertainty records: [confirmed design](../design.md), [OPAQUE security follow-up](opaque-security-follow-up.md), [accepted risks](opaque-accepted-risks.md), and [published-artifact build records](opaque-upstream-build-records.md).
