# Isolated OPAQUE feasibility gate (incomplete; no approval)

This directory is a disposable, standalone experiment module. It is not imported by or approved for the application. The gate remains **NO-GO / incomplete**: the pinned artifacts have not yet been made to interoperate, and the required browser-to-Go, negative-case, and browser resource measurements have not been completed. Do not treat upstream tests or npm's audit summary as approval.

## Reproduction snapshot

Observed 2026-11-13 in the supplied environment (Go 1.26.8, Node 26.13.1, Bun 1.4.4):

* Go module pin: `github.com/bytemare/opaque v0.18.0`, module checksum `h1:R0a5Xayu26uE/+ge2j0YZKotFgMuy5F9KkQTxiBPWX8=`. Its transitive graph is pinned in `go.sum`; `go list -m -json all` records versions and checksums. Module source states MIT; review each transitive module's license before approval.
* Browser package: published npm `@serenity-kit/opaque@1.1.0`, integrity and full lockfile in `package-lock.json`; package declares MIT and no runtime npm dependencies. This is the actual registry artifact, not a build inferred from repository manifests. `npm view ... versions` returned versions only through 1.1.0; querying 1.1.1 returned E404, despite repository-side 1.1.1 claims in prior research. ESM SHA-256: `1e8d56383a77948e453085b6d4f52dcb9645f8bfff32395738b63d3a2e2fcb52`; CJS SHA-256: `903c6cfa9ee052a04102e21ea4f2f36da753572618765bb21a1a5cccd1b00442`.
* Browser source declares no runtime package deps, but the published artifact embeds WASM crypto code; no audited, reproducible Rust dependency/build provenance was established from that tarball. `publishConfig.provenance: true` is package metadata, not proof that this particular tarball has verified provenance. Need inspect registry attestations and published tarball contents directly before approval.
* `bun audit` on this minimal isolated package reported zero advisories for its JS dependency graph. This does not cover the embedded Rust/WASM graph. Rust and Cargo are absent, so the WASM dependency closure and RustSec RUSTSEC-2026-0387 reachability/remediation remain unresolved blockers. The Go implementation explicitly states it has not been independently audited; package audit is not a cryptographic audit.
* `go run golang.org/x/vuln/cmd/govulncheck@latest github.com/bytemare/opaque/...` found zero reachable symbol vulnerabilities, but reported 1 package-level and 32 module-level findings in the selected module graph / Go 1.26.8 toolchain (including standard-library findings and x/crypto SSH findings); govulncheck says these were not called by the scanned source. This is not a clean dependency graph, and each finding requires reachability/applicability review before approval. The experiment module itself currently contains no Go packages; this scan targets the pinned upstream package.

Reproduce setup from this directory with `go mod download`, `bun install --frozen-lockfile`, `bun audit`, and `bunx playwright install chromium`. Bun is the JS package manager; registry metadata/tarball inspection is separate. The copied upstream Go source is in the module cache; run its pinned upstream suite with `cd "$(go env GOPATH)/pkg/mod/github.com/bytemare/opaque@v0.18.0" && go test ./...` (tested here; vector test package passed). This executes upstream tests, not application integration vectors. The local experiment module currently has no Go packages, so `go test ./...` there correctly reports no packages and exits nonzero.

Playwright and Chromium were installed only into user cache / this experiment's npm dependencies. No browser protocol test was completed; browser availability alone is not evidence of interoperability. There are no browser timing/memory values to report.

## What is still required

1. Independently identify and run the RFC 9807 Appendix C vectors through the selected implementation(s), with source/test provenance recorded.
2. Implement a reproducible HTTP harness whose actual Go endpoint uses Bytemare and whose actual browser page loads the exact installed Serenity package. Exercise registration, login, wrong password, malformed/truncated/invalid-element and tampered messages; verify KE3 rejection and matching session keys, wire bytes, and absence of password/client secret state in requests/logs.
3. Record exact suite, 3DH, KSF costs, identity encoding, context, message encoding, and browser init/peak-memory/password operation measurements in the actual browser/version and hardware used. Do not assert Serenity's documented Mac results as measurements from this environment.
4. Establish artifact-specific Rust advisory status, full transitive licenses, npm provenance/signatures, and a reproducible published-artifact audit strategy. Obtain authorized human dependency approval and explicit acceptance of unaudited Go risk or independent review.

### Decision

**NO-GO / gate not passed.** The Go dependency and published npm 1.1.0 artifact are pinned for investigation only. The npm registry discrepancy is confirmed; security status of the embedded WASM is not. Upstream Bytemare vectors pass, but no cross-language exchange, actual browser negative tests, or browser resource measurements were performed. Human approval and unaudited-risk acceptance have not been granted. This work does not unblock production authentication or authorize closing issue #2.
