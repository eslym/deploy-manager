# OPAQUE accepted risks

## Browser WASM malformed-argument memory leak

- **Accepted by:** the project maintainer, explicitly in conversation on 2026-11-13.
- **Affected candidate:** `@serenity-kit/opaque@1.1.0`, used only in the frontend. Upstream [PR #190](https://github.com/serenity-kit/opaque/pull/190) and the [1.1.1 changelog](https://github.com/serenity-kit/opaque/blob/main/CHANGELOG.md#111) describe the fix; 1.1.1 was not published in the registry at the last investigation.
- **Risk:** repeatedly calling client functions with malformed argument shapes can leak WASM memory until the instance fails, interrupting browser authentication and potentially increasing tab memory consumption. Reloading ordinarily creates a fresh instance. This finding does not establish password disclosure or authentication bypass. Application-specific attacker reachability has not been proven.
- **Scope of acceptance:** this frontend availability/reliability issue only, while the upstream fix is unpublished. It does not authorize use of the affected WASM as a backend, waive the Rust timing advisory, resolve exact dependency/license provenance, accept Bytemare's unaudited implementation risk, or approve production authentication dependencies.
- **Required follow-up:** upgrade to a published release containing PR #190 as soon as it becomes available. Verify the actual release artifact contains the fix; refresh integrity/provenance, dependency/license and advisory evidence, update pinned assets, and rerun interoperability and malformed-argument regressions. Complete Bun integrity and fast protocol checks before browser tests. A version number or repository changelog alone is not proof that the published artifact contains the fix.
- **Interim implementation guidance:** validate wrapper arguments, avoid unbounded retries on crypto errors, and show a useful authentication failure. These mitigations are recommendations, not implemented or verified product protections.

This risk acceptance removes only the requirement to wait for the memory-leak fix before evaluating the current frontend candidate. The other [security gate blockers](opaque-security-follow-up.md) remain in force.

## Published browser WASM build-input uncertainty

- **Accepted by:** the project maintainer, explicitly in conversation on 2026-11-13, after reviewing the unsuccessful public-record investigation and declining upstream contact.
- **Affected candidate:** the exact pinned `@serenity-kit/opaque@1.1.0` browser artifact covered by the [verified signatures/provenance](opaque-provenance-verification.md). This acceptance does not automatically extend to another release or rebuilt artifact.
- **Evidence gap:** the public records do not establish the exact Cargo resolution/checksums, complete enabled features, actual toolchain/build environment, or a transitive license/SBOM record corresponding to the shipped WASM. The release recipe and signed source/workflow provenance are known; those do not establish the missing build inputs. See the [public-record investigation](opaque-upstream-build-records.md).
- **Accepted residual risk:** proceeding with evaluation of this pinned browser candidate despite the missing records and resulting uncertainty about its exact dependency graph and artifact-specific applicability/reachability of `RUSTSEC-2026-0387`. A current reconstruction contains an affected `crypto-bigint` version, but this is neither proof of exploitability nor proof of safety for the published artifact. This is an explicit acceptance of uncertainty, not a finding that the advisory is resolved or patched.
- **No upstream contact:** do not send the drafted maintainer request on the basis of this decision.
- **Limits:** this does not waive applicable license or notice obligations, accept Bytemare's unaudited implementation risk, authorize backend WASM use, or grant overall production dependency approval. Any subsequently established exploitable vulnerability requires reassessment; it is not preaccepted by this uncertainty decision.
- **Follow-up:** retain the pinned bytes and integrity/provenance evidence, reassess when upstream provides build records or a new candidate is evaluated, and keep the separate mandatory memory-leak upgrade obligation above. Do not replace missing build evidence with claims inferred from current semver resolution.

## Unaudited Bytemare OPAQUE implementation

- **Accepted by:** the project maintainer, explicitly in conversation on 2026-11-13.
- **Affected candidate:** `github.com/bytemare/opaque v0.18.0`, used by the native-Go server candidate with the tested dependency graph in `experiments/opaque-gate/go.mod` and `go.sum`.
- **Known risk:** upstream explicitly states that this implementation has not been independently audited. RFC vector success, browser/Bun interoperability and clean vulnerability scans do not establish cryptographic correctness or exclude implementation flaws, timing/side-channel weaknesses, or integration vulnerabilities. See the [pinned upstream disclaimer](https://github.com/bytemare/opaque/blob/v0.18.0/README.md).
- **Scope of acceptance:** the absence of an independent audit and its residual implementation risk are knowingly accepted for this pinned candidate. This satisfies the design's explicit-human-acceptance alternative to independent review for this audit-risk decision; it does not claim an audit occurred or that the library is proven safe.
- **Limits:** this is not overall production dependency approval, acceptance of a subsequently established vulnerability, a waiver of license obligations, or permission to omit the required application authentication/session/lifecycle protections and tests. Other risk acceptances above remain separately scoped.
- **Follow-up:** retain exact version/dependency pins and test evidence, monitor upstream advisories and audit findings, reassess on implementation or cryptographic-profile changes, and review any newly established vulnerability before shipping affected code.
