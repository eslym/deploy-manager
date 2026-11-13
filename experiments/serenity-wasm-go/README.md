# Serenity WASM in a Go host: ABI probe

This isolated probe is not application code and does not implement authentication. It extracts the embedded WASM from the already integrity-audited `opaque-gate` install, then uses wazero to compile the exact bytes and print imports/exports.

From this directory, after `bun run audit-artifact` succeeds in `../opaque-gate`:

```sh
bun run inspect
go run . opaque.wasm
```

The generated `opaque.wasm` is intentionally not checked in. This probe does not instantiate the module or call exports: successful compilation establishes only valid WASM, not host compatibility or OPAQUE correctness. Serenity's wasm-bindgen glue expects a JavaScript object/externref ABI; the imported browser/Node shims are not WASI imports.
