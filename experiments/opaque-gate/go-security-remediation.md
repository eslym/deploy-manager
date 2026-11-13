# Isolated Go advisory remediation

This change is limited to the OPAQUE feasibility experiment. It does not approve these dependencies for production or change the application dependency graph.

## Changes

- Raised the module's minimum Go version in `go.mod` from 1.26.8 to 1.26.11, the stable patched toolchain used for validation. Go's module `go` directive enforces this minimum; no redundant `toolchain` directive is needed.
- Upgraded `golang.org/x/crypto` from v0.49.0 to stable v0.59.0 and `golang.org/x/sys` from v0.42.0 to stable v0.50.0. Both were actually resolved and tested; no dependency was deferred for a routine update.
- Left OPAQUE v0.18.0 and browser artifacts unchanged.

## Vulnerability scan

Using the installed `govulncheck` with `GOTOOLCHAIN=go1.26.11`:

| Scan scope | Result |
| --- | --- |
| `govulncheck -scan=symbol ./...` | No vulnerabilities found (no vulnerable reachable symbols) |
| `govulncheck -scan=package ./...` | No vulnerabilities found (no vulnerable imported packages) |
| `govulncheck -scan=module` | No vulnerabilities found (no vulnerable modules in the graph) |

The verbose symbol scan enumerated the Go 1.26.11 standard library and 12 modules, including `golang.org/x/crypto@v0.59.0` and `golang.org/x/sys@v0.50.0`, and found no vulnerabilities. Consequently, none of the previously listed GO-2026-6355, GO-2026-6354, GO-2026-6303, GO-2026-5932, GO-2026-5033, GO-2026-5024, GO-2026-5023, GO-2026-5021, GO-2026-5020, GO-2026-5019, GO-2026-5018, GO-2026-5017, GO-2026-5016, GO-2026-5015, GO-2026-5014, GO-2026-5013, GO-2026-5006, or GO-2026-5005 are reported for the upgraded graph. Remaining Go advisories reported by these scans: **none**. This database result is time-sensitive and does not replace cryptographic review.

## Validation and reproduction

Run from `experiments/opaque-gate`:

```sh
GOTOOLCHAIN=go1.26.11 go test ./...
GOTOOLCHAIN=go1.26.11 go vet ./...
GOTOOLCHAIN=go1.26.11 bun run test-vectors
bun run audit-artifact
bun run test
bun run test-browser-gate
GOTOOLCHAIN=go1.26.11 govulncheck -show verbose,traces ./...
GOTOOLCHAIN=go1.26.11 govulncheck -scan=symbol ./...
GOTOOLCHAIN=go1.26.11 govulncheck -scan=package ./...
GOTOOLCHAIN=go1.26.11 govulncheck -scan=module
GOTOOLCHAIN=go1.26.11 bun run browser
```

All passed in this run. The Go package has no local test files; the pinned Bytemare upstream RFC vector suite passed. Bun artifact integrity matched the pinned registry digest and ESM/CJS hashes; the Bun fast protocol test and browser-gate rejection test passed. The subsequent Chromium 158 run completed registration and KE3-verified login. The Bun artifact/protocol checks passed again inside the browser wrapper. Browser timings and resource observations remain a single-machine sample, not a security calibration.

## Scope and decision

The remediation is only for this isolated harness. The browser package's Rust/WASM dependency closure and advisory reachability remain unresolved, Bytemare remains unaudited, and this does not complete the security/interoperability gate or authorize production dependency approval. Keep the production decision **NO-GO**.
