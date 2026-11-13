# OPAQUE feasibility security follow-up

Research date: **2026-11-13**. This supplements (does not replace) [the OPAQUE feasibility research](opaque-authentication.md) and isolated [gate README](../../experiments/opaque-gate/README.md). This is evidence only: no dependency approval, production authentication implementation, risk acceptance, or change to the confirmed design is implied. The rejected Go-hosted Serenity WASM alternative (#24) was not pursued.

## Summary and decision

**Bytemare audit-risk decision (2026-11-13):** the maintainer now explicitly accepts the known risk of using unaudited `github.com/bytemare/opaque v0.18.0`. See [the scoped acceptance](opaque-accepted-risks.md#unaudited-bytemare-opaque-implementation). Earlier statements below requesting that acceptance are historical. Overall dependency approval and other gate/compliance requirements remain outstanding.

**Maintainer risk-acceptance update (2026-11-13):** missing exact published-WASM build records and the resulting dependency/advisory uncertainty are now explicitly accepted for the pinned frontend candidate; no upstream request is authorized. See [accepted risks](opaque-accepted-risks.md#published-browser-wasm-build-input-uncertainty). Historical descriptions of these evidence gaps below remain accurate, but they are now accepted residual uncertainty rather than awaiting this specific human decision. This does not waive license obligations, accept Bytemare's unaudited risk, or approve production dependencies.

**Subsequent verification update:** npm package and Sigstore/SLSA provenance signatures have now been cryptographically verified, including the expected workflow identity, subject digest and transparency proof. See [verification commands and trust assumptions](opaque-provenance-verification.md). The initial signature-verification gaps described below are historical, not remaining blockers. Go remediation is also complete as recorded below. Exact WASM build inputs, license closure, advisory applicability and audit-risk approval remain unresolved; the decision is still NO-GO.

**Still NO-GO for dependency approval and application authentication.** The published `@serenity-kit/opaque@1.1.0` tarball hash matches npm metadata, and its registry-provided SLSA statement identifies a successful upstream GitHub release workflow and source commit. However, local cryptographic verification of the npm package-signature/Sigstore bundle was not completed; the prior audit script only checked a subject digest and expressly reported this limitation. More importantly, neither the artifact nor its attestation includes the exact Cargo dependency resolution/build inputs needed to map RustSec findings and license obligations to the shipped WASM. A present-day source reconstruction is not evidence of the February 2026 release's graph.

| Question | Result |
|---|---|
| Does the tarball match npm's immutable integrity metadata? | **Yes**, SHA-512 `Y6v/+hRMn0MdMEk5+/ArM0vPIiFfFEbdTZc7oAx+cWyvGODGezAQ/sMjXBVogf7NsS9z9EV0Ve5paZCVULuedw==`; reproducible check passes. |
| Is there signed provenance metadata naming the artifact, source, workflow, and run? | **Yes.** Registry attestation subject matches; source `ca1cb22f03b9e456159ce367e2975124f87f5ad8`, `.github/workflows/release.yml`, successful run 21564684589 attempt 1. |
| Was signature/provenance cryptographically verified locally? | **Yes, subsequently.** npm signature and Cosign verification passed; see the [verification record](opaque-provenance-verification.md). |
| Is the exact embedded Rust/WASM graph known? | **No.** No lockfile/SBOM/build-resolved dependency manifest was attached. |
| Is `RUSTSEC-2026-0387` proven absent or unreachable in the artifact? | **No.** Current source reconstruction resolves affected `crypto-bigint 0.5.5`; artifact-specific reachability cannot be concluded. |
| Is malformed-WASM memory-leak fix included? | **No.** Upstream 1.1.1 changelog lists it; registry returned 404 for 1.1.1 and 1.1.0 is the published artifact under review. |
| Are the Go toolchain findings fixed in the checked-out experiment? | **Yes.** Subsequent remediation raised the minimum Go version to 1.26.11 and upgraded `x/crypto` to v0.59.0 and `x/sys` to v0.50.0. Symbol, package and module scans report no vulnerabilities; protocol/browser regressions passed. See the [remediation record](../../experiments/opaque-gate/go-security-remediation.md). |

## Published artifact, provenance, build and licenses

On this checkout, `bun run audit-artifact` succeeded. It downloaded the registry tarball, compared its SHA-512 with `dist.integrity`, extracted ESM/CJS entries and checked their pinned SHA-256 hashes. It reports MIT, no declared runtime JS dependencies, tarball size 323,964 bytes, and ESM/CJS sizes and hashes as recorded in the experiment README. These checks establish byte identity with the registry's metadata, not publisher identity or artifact correctness.

The npm registry attestation endpoint returned both an npm publish attestation and a SLSA provenance bundle. Decoding the SLSA statement reports the exact npm package subject and digest, repository `https://github.com/serenity-kit/opaque`, workflow `.github/workflows/release.yml` on `refs/heads/main`, source commit `ca1cb22f03b9e456159ce367e2975124f87f5ad8`, and invocation `https://github.com/serenity-kit/opaque/actions/runs/21564684589/attempts/1`. GitHub's public Actions API reports this run completed successfully on 2026-02-01, `workflow_dispatch`, with that head SHA and workflow path. This is meaningful provenance metadata, but without checking the Sigstore certificate identity, DSSE signature, Rekor inclusion proof/checkpoint, and trust roots against authoritative roots, it is not a verified attestation. An npm package signature is likewise not verified merely because registry metadata includes one.

The source commit's release workflow and manifests are useful to understand the build process, but not sufficient to reconstruct what Cargo resolved during that run: no `Cargo.lock` was committed at the source commit, and the attestation does not enumerate Cargo dependencies. The prior reconstructed current graph (103 packages) is explicitly **not exact-artifact evidence**. It found `opaque-ke 4.0.1 → elliptic-curve 0.13.8 → crypto-bigint 0.5.5`; it must not be represented as the artifact's build graph. `@serenity-kit/opaque@1.1.0` declares MIT at the JS package level; a complete WASM/transitive license bill of materials cannot be certified without its exact resolution. A source rebuild with a newly resolved graph would be a distinct artifact, not remediation or verification of the published package.

**What would close this evidence gap:** an upstream-attested build input bundle (or equivalent independently verifiable record) containing the exact Cargo.lock used by the run, target/features, Rust/Cargo and wasm-bindgen/tool versions, build command/environment, and generated asset digest; ideally an SPDX/CycloneDX SBOM with license expressions. Alternatively, reproducibly rebuild the exact source commit with those inputs and demonstrate byte-for-byte artifact equality, while verifying the Sigstore bundle with trusted Sigstore tooling and roots. If equality fails, the rebuilt output is a separate candidate and needs fresh artifact, interop, performance, and security review. Do not contact maintainers or publish requests from this task.

## RustSec operation reachability and memory leak

**Maintainer decision, 2026-11-13:** the frontend-only malformed-argument memory-leak risk is explicitly accepted while the fix is unpublished, with a required upgrade as soon as a fixed release is published. See [scope and upgrade requirements](opaque-accepted-risks.md). This does not accept the separate Rust timing advisory or unaudited implementation risk.

RustSec [RUSTSEC-2026-0387](https://rustsec.org/advisories/RUSTSEC-2026-0387.html), issued 2026-10-23, describes timing variability in `crypto-bigint` operations intended to be constant-time, including subtraction, zero counting, division, Montgomery multiplication, equality, and inversion; patched release is `>=0.7.6`. The advisory itself cautions that practical exploitation depends on downstream algorithm, compiler, CPU, and observation conditions. No exploit was reproduced.

The reconstructed graph cited above shows that a contemporary resolution of this source can contain the affected pre-0.7.6 crate, so merely selecting Ristretto/default features does **not** establish the advisory absent. Conversely, crate presence alone does not prove an affected operation is linked into, or reachable with secrets in, the Ristretto WASM artifact. The missing lockfile prevents a reliable exact-version mapping, and a call-graph/constant-time conclusion would additionally require artifact/source correspondence and feature/target-specific analysis. The published package therefore remains blocked on this point; do not claim either confirmed exploitability or safe non-reachability.

The repository changelog reports a malformed-parameter WASM memory-leak fix in 1.1.1. Registry lookup for `@serenity-kit/opaque@1.1.1` returned 404 during this investigation; 1.1.0 is the artifact exercised by the gate. Malformed protocol parameters are attacker-controlled input at the wrapper boundary, so the fix is relevant as robustness/availability hardening even though this is not evidence of a secret disclosure. It is an additional concrete reason not to treat the 1.1.0 bundle as containing later source fixes. A separately rebuilt fixed source would require a fresh pinned artifact and complete gate; the browser package must not be silently swapped.

## Go advisories and remediation status

Ran `govulncheck ./...` in `experiments/opaque-gate` using the checked-out host Go 1.26.8. It exited 3 and reported **10 standard-library vulnerabilities**; explicit examples include GO-2026-6859 (`net/http`, fixed 1.26.11), GO-2026-6809 (`crypto/x509`, fixed 1.26.10), and eight more in `net/http`, `mime/multipart`, and `crypto/tls` fixed in 1.26.9 or later. It also reports 3 vulnerabilities in imported packages and 20 vulnerabilities in required modules not appearing called by scanned code. This is a new scan result, not a claim that the Go module graph was remediated. The previous run on Go 1.26.11 noted zero reachable symbol/imported-package vulnerabilities but remaining module-only findings; it must be rerun from this exact tree with the selected patched Go toolchain before relying on it.

The initial investigation made no module/toolchain changes. **Subsequent remediation completed the upgrade and validation:** Go 1.26.11, `golang.org/x/crypto v0.59.0`, and `golang.org/x/sys v0.50.0`. Symbol-, package-, and module-scope scans report no vulnerabilities. Go test/vet, pinned upstream vectors, Bun integrity/protocol checks, browser gating and Chromium interoperability passed. Commands and results are in the [remediation record](../../experiments/opaque-gate/go-security-remediation.md). No production dependency is affected or approved by these experiment-only changes.

## Verification and remaining human decision

Executed in this worktree:

```text
git status --short --branch: clean at integration/opaque-gate-2, 8266661
experiments/opaque-gate: bun run audit-artifact — PASS (tarball integrity and embedded JS hashes)
experiments/opaque-gate: govulncheck ./... — FAIL/Findings (Go 1.26.8; 10 stdlib, 3 imported-package, 20 module-only)
GitHub Actions API for run 21564684589 — completed/success, workflow_dispatch, source SHA matches SLSA statement
```

The initial investigation did not launch a browser or modify dependencies. The subsequent Go remediation changed the isolated dependency graph and reran Chromium only after Bun integrity/protocol checks passed, as recorded above. No production application files were changed.

**Resolved:** registry tarball identity against npm integrity; existence and decoded content of provenance claims; upstream run identity/status; the 1.1.1 publication gap; Go toolchain/dependency advisories in the isolated harness, with regression validation.

**Remaining approval/compliance work:** overall dependency approval remains outstanding, and applicable license/notice obligations still apply. The maintainer has explicitly accepted the unaudited Bytemare implementation risk for v0.18.0. Exact WASM build closure and artifact-specific RustSec reachability remain unknown but their uncertainty is accepted within the pinned frontend scope; do not describe them as patched or proven safe. The frontend memory leak is separately accepted with an upgrade obligation. Issue #2 and dependent authentication work remain blocked pending the remaining gate decisions and requirements; no upstream request or rebuild is authorized by these acceptances.

## Primary sources

- [npm 1.1.0 metadata](https://registry.npmjs.org/@serenity-kit/opaque/1.1.0), [registry attestation endpoint](https://registry.npmjs.org/-/npm/v1/attestations/%40serenity-kit%2Fopaque%401.1.0), and [published tarball](https://registry.npmjs.org/@serenity-kit/opaque/-/opaque-1.1.0.tgz).
- [Upstream release workflow](https://github.com/serenity-kit/opaque/blob/ca1cb22f03b9e456159ce367e2975124f87f5ad8/.github/workflows/release.yml), [attested source commit](https://github.com/serenity-kit/opaque/commit/ca1cb22f03b9e456159ce367e2975124f87f5ad8), [GitHub Actions run 21564684589](https://github.com/serenity-kit/opaque/actions/runs/21564684589), and [upstream changelog](https://github.com/serenity-kit/opaque/blob/main/CHANGELOG.md).
- [RUSTSEC-2026-0387](https://rustsec.org/advisories/RUSTSEC-2026-0387.html) and [RustCrypto fix PR #1365](https://github.com/RustCrypto/crypto-bigint/pull/1365).
- [Sigstore bundle verification](https://docs.sigstore.dev/cosign/verifying/verify/), [SLSA provenance verification](https://github.com/slsa-framework/slsa-verifier), and [npm provenance verification guidance](https://docs.npmjs.com/generating-provenance-statements/).
- Prior source and protocol references: [OPAQUE research](opaque-authentication.md), [gate reproducibility/evidence](../../experiments/opaque-gate/README.md), and [confirmed design §101–108](../design.md#opaque-authentication-and-account-lifecycle).
