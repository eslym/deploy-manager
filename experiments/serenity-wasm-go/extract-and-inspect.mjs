import { readFile, writeFile } from "node:fs/promises";
import { createHash } from "node:crypto";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

const here = fileURLToPath(new URL(".", import.meta.url));
const artifact = resolve(here, "../opaque-gate/node_modules/@serenity-kit/opaque/esm/index.js");
const source = await readFile(artifact, "utf8");
const start = source.indexOf("function wasmData(imports)");
const quote = source.indexOf("'", start);
const end = source.indexOf("'", quote + 1);
if (start < 0 || quote < 0 || end < 0) throw new Error("could not locate the bundled Serenity WASM data");
const bytes = Buffer.from(source.slice(quote + 1, end), "base64");
const output = resolve(here, "opaque.wasm");
await writeFile(output, bytes);
console.log(`artifact: ${artifact}`);
console.log(`wasm: ${output} (${bytes.length} bytes, sha256 ${createHash("sha256").update(bytes).digest("hex")})`);
