# OPAQUE browser artifact provenance verification

Research date: **2026-11-13**. This is evidence for the isolated OPAQUE feasibility gate only. It does not approve a dependency, accept risk, or authorize production authentication. See [security follow-up](opaque-security-follow-up.md) for the remaining security and dependency-graph blockers.

## Result

Final status: the maintainer subsequently approved the pinned dependency selection with [accepted risks](opaque-accepted-risks.md); see [gate findings](opaque-gate-findings.md). The experiment scripts have been removed at the maintainer's request. Reproduction commands below refer to [the archived experiment](https://github.com/eslym/deploy-manager/tree/a069572/experiments/opaque-gate), not this checkout; the initial NO-GO below is historical.

The exact registry tarball `@serenity-kit/opaque@1.1.0` was downloaded and its SHA-512 verified against npm `dist.integrity` before signature/provenance checks. Both the npm package signature and SLSA/Sigstore provenance now pass cryptographic verification:

- npm ECDSA P-256/SHA-256 signature over `@serenity-kit/opaque@1.1.0:sha512-Y6v/+hRMn0MdMEk5+/ArM0vPIiFfFEbdTZc7oAx+cWyvGODGezAQ/sMjXBVogf7NsS9z9EV0Ve5paZCVULuedw==` verifies against key ID `SHA256:DhQ8wR5APBvFHLF/+Tc+AYvPOdTpcIDqOhxsBHRwC7U`, obtained from the npm registry's HTTPS key endpoint.
- Cosign verifies the SLSA v1 DSSE signature, Fulcio certificate chain, Rekor inclusion proof/checkpoint and transparency-log trust using its embedded Sigstore roots. The certificate identity is exactly `https://github.com/serenity-kit/opaque/.github/workflows/release.yml@refs/heads/main`, issuer `https://token.actions.githubusercontent.com`; repository/ref/source SHA claims were constrained to `serenity-kit/opaque`, `refs/heads/main`, and `ca1cb22f03b9e456159ce367e2975124f87f5ad8`.
- The SLSA subject is `pkg:npm/%40serenity-kit/opaque@1.1.0` with SHA-512 matching the downloaded 323,964-byte tarball. The attested invocation is run `21564684589`, attempt 1.

Trust decision: npm metadata, package signature, and key were fetched over HTTPS from `registry.npmjs.org`; the signature proves consistency with the registry-published signing key, not independent publisher identity. For provenance, trust is rooted in Sigstore's official roots and the GitHub Actions OIDC identity, narrowly constrained to the expected repository/workflow/ref/source. Rekor inclusion and checkpoint were verified by cosign. These checks establish provenance claims for these bytes; they do **not** establish artifact correctness or reproducibility.

## Reproduce

From `experiments/opaque-gate/`, after installing the official Sigstore Cosign binary and placing it on `PATH` (or setting `COSIGN` to its executable):

```sh
bun provenance-verify.mjs
```

The script makes no browser request or launch. It aborts unless npm tarball integrity, npm signature, expected provenance subject/source claims, and cosign verification all succeed. It uses only Bun for JavaScript execution and `node:crypto` provided by Bun; no npm installation or parent package/lockfile changes are required. Cosign is an external verifier and should itself be obtained from the official Sigstore release channel with its release provenance/checksum verified according to local policy.

Exact command used in this investigation:

```text
COSIGN=/tmp/opencode/cosign bun provenance-verify.mjs
```

Result: **exit 0**. Output reported `npmSignature.verified: true`, `slsa.verified: true`, `subjectMatchesDownloadedTarball: true`, the identity/ref/source commit above, and invocation `https://github.com/serenity-kit/opaque/actions/runs/21564684589/attempts/1`.

The npm and Sigstore checks are performed by `experiments/opaque-gate/provenance-verify.mjs`. The script uses cosign's `verify-blob-attestation --type slsaprovenance1` with the expected issuer and identity claims; this is cryptographic verification, not merely decoding the bundle.

## Exact build-input investigation and remaining blockers

The public Actions artifacts endpoint for run `21564684589` returned an empty artifact list. The attested source commit's release workflow/manifests do not provide a committed `Cargo.lock`, and this run exposes no lockfile/build-input artifact. Thus the release's exact Cargo dependency resolution, target/features/toolchain, and generated WASM build inputs remain unknown. A present-day Cargo resolution must not be imputed to the February 2026 artifact. The artifact-specific transitive license closure and `RUSTSEC-2026-0387` operation reachability consequently remain unresolved; the verified provenance does not close those gaps.

`@serenity-kit/opaque@1.1.1` remains unpublished in the registry at the time of this check, so the 1.1.0 artifact does not gain subsequent source fixes by provenance verification. No maintainer was contacted and no unrelated Go-WASM alternative or closed issue was pursued. The OPAQUE dependency decision remains **NO-GO** pending exact build-graph evidence or a separately authorized/retested candidate, plus the other blockers in the security follow-up.

## Primary sources

- [npm package metadata](https://registry.npmjs.org/@serenity-kit/opaque/1.1.0), [npm signing keys](https://registry.npmjs.org/-/npm/v1/keys), [npm attestation bundle](https://registry.npmjs.org/-/npm/v1/attestations/%40serenity-kit%2Fopaque%401.1.0), and [published tarball](https://registry.npmjs.org/@serenity-kit/opaque/-/opaque-1.1.0.tgz).
- [Sigstore Cosign verification documentation](https://docs.sigstore.dev/cosign/verifying/verify/), [attested GitHub workflow](https://github.com/serenity-kit/opaque/blob/ca1cb22f03b9e456159ce367e2975124f87f5ad8/.github/workflows/release.yml), and [Actions run](https://github.com/serenity-kit/opaque/actions/runs/21564684589).
- [OPAQUE security follow-up](opaque-security-follow-up.md), [gate findings](opaque-gate-findings.md), and [confirmed design](../design.md#opaque-authentication-and-account-lifecycle).
