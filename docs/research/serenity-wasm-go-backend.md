# Running Serenity's published OPAQUE WASM from Go

Research and experiment date: **2026-11-13**. This answers whether embedding `@serenity-kit/opaque`'s WASM in a Go backend is a more concrete route than native-Go Bytemare. It is isolated feasibility research only: no dependency approval, production authentication implementation, or change to [the confirmed design](../design.md).

## Conclusion

**No—not as a direct Go/WASM server integration.** The exact published package is valid WebAssembly and includes server operations, but it is a wasm-bindgen JavaScript module, not a WASI module or a Go-callable C ABI. The probe compiled the WASM using wazero, then found **46 imports**, including JavaScript object/externref, exception, typed-array, and randomness shims (`__wbindgen_*`, `__wbg_getRandomValues`, `__wbg_crypto`); exports include `createServerSetup`, `startServerLogin`, and `finishServerLogin`. Wazero compilation is possible, but instantiation requires implementing the JavaScript ABI and its object/exception/memory conventions. It is not a meaningful drop-in “run the npm package in Go” capability.

The less exotic options remain: use the package from its normal JS runtime (which design excludes as a production service process), write and audit a substantial JavaScript host compatibility layer, or build a **different** Rust/WASI or C-ABI wrapper around `opaque-ke`. The latter is not the exact published artifact, needs a new build/provenance/dependency review, and still brings Rust advisory and secret-lifecycle concerns. On current evidence, this adds integration and supply-chain risk rather than making the Bytemare Go + Serenity browser pairing more concrete. The recommendation remains **NO-GO for production approval** pending the existing security gates.

## Reproducible experiment

Files: [`experiments/serenity-wasm-go/`](../../experiments/serenity-wasm-go/). It is a stand-alone wazero probe, separate from the earlier Bytemare compatibility experiment. It extracts the 1.1.0 WASM payload from the already installed, audited package, reports its SHA-256, and compiles/inspects module imports and exports.

The package integrity check was completed **before executing the Bun extractor**:

```text
cd experiments/opaque-gate && bun run audit-artifact
tarballMatchesRegistryIntegrity: true
esm/index.js sha256: 1e8d56383a77948e453085b6d4f52dcb9645f8bfff32395738b63d3a2e2fcb52
```

Then, from `experiments/serenity-wasm-go`:

```text
$ bun run inspect
wasm: .../opaque.wasm (309954 bytes, sha256 32255973125fe9ac1fdf54d7f152361b302299f4b6678a1cb1c9baa978f0d6db)
$ go run . opaque.wasm
WASM valid: yes
Imports (46):
  ./opaque_bg.js.__wbg_Error_...
  ./opaque_bg.js.__wbg_crypto_...
  ./opaque_bg.js.__wbg_getRandomValues_...
  ./opaque_bg.js.__wbindgen_init_externref_table
  ... (42 further JavaScript imports; command prints all)
Exports (16):
  __externref_table_alloc ... __wbindgen_start
  createServerRegistrationResponse
  createServerSetup
  finishClientLogin
  finishClientRegistration
  finishServerLogin
  getServerPublicKey
  startClientLogin
  startClientRegistration
  startServerLogin
```

`go run . opaque.wasm` compiled the module and listed metadata; it did **not instantiate it or call an OPAQUE operation**. The generated WASM is not checked in. This establishes a concrete blocker at host ABI integration, not a cryptographic result. `wazero` is pinned at v1.9.0 in the isolated module.

Validation: `go test ./...` and `go vet ./...` pass (there are no Go test files; this is an inspection utility). `govulncheck ./...` reports zero reachable symbol vulnerabilities, plus one imported-package finding (GO-2026-6604, Windows-only `os.Root.MkdirAll`, fixed in Go 1.26.9) and 14 standard-library module findings associated with the Go 1.26.8 toolchain, none reached by the probe. These findings apply to the probe/toolchain report, not to a production authentication server; use a patched Go toolchain for any future work. Existing Bun HTTP interoperability tests also pass, but exercise Bytemare—not a Go-hosted WASM module.

## What the architecture distinction means

1. **Exact published WASM, Go-only host:** not directly usable as-is. The JS glue in the npm artifact owns wasm-bindgen's externref/string/object conversions, error propagation, and host randomness plumbing. A Go implementation would have to faithfully recreate those semantics, supply fresh cryptographically secure randomness, and correctly manage memory and references across calls. The randomness imports are especially security-sensitive: an absent/failing source must fail closed, never silently substitute deterministic or weak entropy. The probe deliberately does not attempt this compatibility layer.
2. **Embed a JS engine in Go and load package glue:** could avoid recreating parts of the JS ABI, but the engine must itself support WebAssembly or be bridged to a WASM runtime; JavaScript support alone is insufficient. This adds an interoperability surface inside the server process. The design excludes a separate frontend server or runtime Node.js installation, not every possible embedded JS engine, so this option needs evaluation rather than being ruled out by that requirement. No engine was prototyped or costed here.
3. **New Rust/WASI or C ABI wrapper:** potentially a cleaner host boundary, but a distinct cryptographic artifact, not the published npm WASM. It requires carefully specified input/output encodings, entropy injection, setup and key persistence, error behavior, memory/secret handling, concurrency semantics, pinned reproducible build inputs, and security review. WASI availability alone does not prove browser API parity. It does not remove `opaque-ke`'s dependency/advisory concerns.

The npm package exposes server functions, but its server setup is a library-specific opaque value, not an established Go/Bytemare serialization contract. A wrapper would need to decide stable storage and upgrade behavior for setup/private key and records. The ordinary browser-to-Go integration harness already demonstrates that Serenity's browser client interoperates with Bytemare's native RFC implementation; using Serenity server WASM is not needed to solve the browser/backend wire-compatibility question.

## Comparative risk and remaining gates

The [executed feasibility experiment](../../experiments/opaque-gate/README.md) records the browser-to-Go interoperability run and its scope; the earlier [research note](opaque-authentication.md) predates those runs. Native Bytemare keeps the server inside the intended Go process and uses Go's library API, avoiding a bespoke cross-runtime ABI. It is still explicitly unaudited and carries a Go dependency/toolchain review burden; that is not an endorsement. Serenity/WASM hosting would share the same need to establish RFC behavior and artifact security while adding JS ABI or wrapper code, another runtime/build chain, and more difficult secret/memory ownership. It does not resolve the published artifact's inherited Rust dependency/advisory questions. The exact 1.1.0 package remains pinned; repository-side later fixes must not be assumed present.

This probe did not test server setup persistence, registration/login through Go-hosted WASM, KE3 validation, wrong-password/tamper behavior, entropy failure, concurrency, memory costs, startup cost, or a browser. Those tests were not meaningful without first implementing a secure host ABI; passing the earlier Bytemare tests cannot be attributed to WASM. No cryptographic correctness is inferred from module compilation or symbol names.

Before any dependency approval, retain all gates in `opaque-authentication.md`: resolve artifact-specific RustSec RUSTSEC-2026-0387 applicability and provenance/license closure; independently decide audit risk; run RFC vectors; test the exact browser/server pair's negative cases and lifecycle; and measure supported-browser KSF costs. If pursuing a Rust wrapper, add its source/toolchain/build reproducibility and explicit ABI/entropy/memory threat review as additional gates, rather than treating it as a shortcut.

## Primary sources

- [Serenity `@serenity-kit/opaque` 1.1.0 published artifact and registry integrity](https://registry.npmjs.org/@serenity-kit/opaque/1.1.0); [published tarball](https://registry.npmjs.org/@serenity-kit/opaque/-/opaque-1.1.0.tgz). The experiment verifies this integrity through the existing `experiments/opaque-gate/artifact-audit.mjs` before extraction.
- [Serenity source wrapper at attested release commit `ca1cb22f03b9e456159ce367e2975124f87f5ad8`](https://github.com/serenity-kit/opaque/blob/ca1cb22f03b9e456159ce367e2975124f87f5ad8/src/lib.rs) and [Rollup configuration](https://github.com/serenity-kit/opaque/blob/ca1cb22f03b9e456159ce367e2975124f87f5ad8/rollup.config.js): wrapper APIs and bundled wasm-bindgen JS/WASM packaging.
- [Wazero v1.9.0](https://github.com/tetratelabs/wazero/tree/v1.9.0): Go WebAssembly runtime used for compile-time module/import/export inspection.
- [OPAQUE feasibility and RFC/security sources](opaque-authentication.md), particularly §§5–7 and references to RFC 9807, Bytemare v0.18.0, Serenity 1.1.0, and RUSTSEC-2026-0387.
