# OPAQUE authentication: RFC 9807 and browser/Go feasibility

Research date: **2026-11-12**. Planning only; no implementation, dependency selection, or change to the agreed design is authorized by this note. Scope: the single administrator, CLI-issued one-time bootstrap/recovery grants, browser registration, embedded server-rendered dashboard with lightweight JavaScript, and externally terminated HTTPS described in [the design](../design.md).

**Frontend correction (2026-11-13):** the current [design](../design.md#frontend-delivery-and-go-integration) selects the existing `web/` SvelteKit client-rendered SPA, statically built and embedded in Go, instead of the server-rendered dashboard assumed by this historical research. The OPAQUE protocol and lifecycle requirements remain applicable; load crypto lazily for authentication flows rather than through the shared SPA bundle.

## Findings and recommendation

**Implementation-stage update, 2026-11-13:** subsequent experiments demonstrated compatibility and the maintainer approved the pinned Go/browser dependencies with explicit risk acceptance. See [final gate findings](opaque-gate-findings.md), [accepted risks](opaque-accepted-risks.md) and [license obligations](dependency-license-compatibility.md). This initial research note remains a historical record; its unexecuted-test and pending-approval descriptions refer to the original research stage. Experiment code was removed after documenting the results.

- **Keep the explicit RFC 9807 requirement.** OPAQUE is an augmented password-authenticated key exchange, not a synonym for an unstructured bearer session token. Its registration and login messages do not transmit the password. The RFC was published in July 2025 as an **IRTF Informational RFC**, not an Internet Standards Track standard. [R1]
- **A plausible native-Go/browser pairing exists:** `github.com/bytemare/opaque` **v0.18.0** on the server and `@serenity-kit/opaque` **1.x** (Rust `opaque-ke` 4.x compiled to WASM) in the browser. Both explicitly target RFC 9807, and source exposes overlapping Ristretto255/3DH/SHA-512 configurations. **This is a candidate, not demonstrated interoperability:** no browser-to-Go exchange was executed in this research, and no published test of this exact pair was established. [G1–G4, J1–J4]
- **Security acceptance is a blocker, not a checkbox.** Bytemare explicitly says it has not been independently audited. Serenity has a favorable 2023 security review and `opaque-ke` has a 2021 audit history, but neither constitutes an audit of the November 2026 RFC implementation plus this Go integration. A newly published Rust dependency timing advisory needs artifact-specific investigation before shipping, especially for the P-256 variant. [G1, A1–A3, K1–K2]
- **Recommended next step, once separately authorized:** an isolated compatibility/security feasibility gate, not application implementation. Pin exact artifacts, resolve the dependency advisory, prove browser registration and login against the Go server, measure browser KSF cost, and explicitly accept or remediate the unaudited Go implementation risk. Do not silently substitute a draft implementation or ordinary password POST if this gate fails.

## 1. What crosses the network

RFC 9807 specifies OPAQUE-3DH using an OPRF, credential recovery, and authenticated key exchange. It uses the RFC 9497 **OPRF mode**, not merely any VOPRF implementation. Both parties must agree on algorithms and parameters before registration and login. [R1 §§2, 3, 7]

### Bootstrap/registration

Application authorization and OPAQUE registration are separate layers:

1. A local CLI issues a short-lived random bootstrap grant for the one admin slot. The server stores only its hash, purpose, expiry, and consumption state, as required by the existing design. The grant authorizes enrollment; it is **not the password and not an OPAQUE key**.
2. The browser obtains the registration page and locally accepts the new password. `CreateRegistrationRequest(password)` blinds a password-derived group element with fresh randomness. Only the serialized **blinded element** is sent as `RegistrationRequest`, alongside application enrollment metadata.
3. The server derives the credential's OPRF key from its persistent `oprf_seed` and a unique `credential_identifier`. It returns `RegistrationResponse`: the evaluated blinded element and the server's AKE public key.
4. The browser unblinds/finalizes the OPRF, applies the configured key-stretching function to its output, and creates the `RegistrationRecord`: client public key, masking key, and envelope. The browser uploads that record, **not the password, the unblinded OPRF output, or the client private key**.
5. The application atomically installs the record for the single admin and consumes the enrollment authority. Registration does not itself complete an authenticated login; prefer a subsequent full OPAQUE login before issuing a dashboard session.

Steps 2–4 are the RFC registration flow, three protocol messages typically carried by two HTTP request/response exchanges. Steps 1 and 5 are proposed application policy consistent with the existing design. Registration **requires a confidential, integrity-protected, server-authenticated channel**. The RFC's envelope here is a nonce and authentication tag used for key recovery, not simply an encrypted password blob. [R1 §§3.2, 4.1, 5]

### Login

1. The browser runs `GenerateKE1(password)` locally. `KE1` contains a freshly blinded password-derived element, a client nonce, and an ephemeral public keyshare.
2. The server uses the persisted record and key material to generate `KE2`: evaluated element, masking nonce and masked credential response, server nonce, ephemeral public keyshare, and server transcript MAC. It retains short-lived handshake verification state.
3. The browser unblinds and stretches the OPRF output, recovers its authentication key material, verifies the envelope/server authentication, and generates `KE3`, the client transcript MAC. Successful client processing yields a shared `session_key` and a client-only `export_key`.
4. The server runs `ServerFinish(KE3)` (Bytemare calls its verification API `LoginFinish`). **Only successful client-MAC verification authenticates the browser.** Do not authorize requests or issue cookies merely because KE2 or a provisional session secret was generated. The browser then waits for application confirmation of completion. [R1 §§3.3, 6; G1]

The server sees cryptographic messages derived from the password, not the password or a reusable password-hash POST. The client blind, serialized client state, plaintext password, and recovered secret keys stay in browser memory and must never be uploaded as HTTP state. In particular, Serenity exposes serialized `clientLoginState`/`clientRegistrationState` strings: they are **local secret state**, not safe round-trip fields just because they are base64. Clear references promptly; JavaScript/Go garbage collection and WASM do not guarantee complete secret erasure. [R1 §§6, 9.1; J3; G1]

### Security boundaries

- OPAQUE resists passive password disclosure and precomputation attacks after server compromise; it does **not** eliminate online guessing, offline guessing after sufficient server-secret compromise, phishing pages, XSS, or malicious client code. Use a strong unique admin password, application rate limits, bounded handshake state, generic failures, strict input parsing, and fresh secure randomness. [R1 §§1, 9, 10.7, 10.11; A1]
- **HTTPS remains mandatory for this application, including login.** The mathematical AKE need not rely on PKI during login, but browsers still need authentic HTML/JS/WASM delivery; cookies, enrollment grants, identities, dashboard contents, and subsequent actions need transport protection. A compromised TLS proxy can replace the JS and steal the password before OPAQUE runs. Do not advertise protection against that active compromise. [R1 §§3.2, 8; A1, OPA-01-Q02 and Threat 14]
- Keep the Go backend reachable only through the trusted proxy/local transport, configure the canonical external HTTPS origin, and trust forwarded headers only from that proxy. Do not expose a direct HTTP bypass. This is deployment policy, not something the OPAQUE library supplies.

## 2. Dashboard sessions, CSRF, and lifecycle

The following are **application design recommendations**, not extra RFC messages. RFC 9807 does not define HTTP routes, session cookies, CSRF, bootstrap grants, or recovery authorization. Bytemare likewise explicitly leaves session management and rate limiting to the application. [R1 §8; G1]

### HTTP session establishment

- After successful KE3 verification, generate a **separate CSPRNG session token** (for example, 32 random bytes). Store a server-side session record, preferably indexed by a hash of the token; return it in a host-only `Secure; HttpOnly; SameSite=Strict; Path=/` cookie, preferably with a `__Host-` name and no `Domain` attribute. Expire/revoke it server-side, rotate at authentication, and invalidate on logout.
- Do **not** use the OPAQUE shared `session_key`, `export_key`, envelope, registration record, or KE3 bytes as that bearer token. A successful OPAQUE AKE can serve solely as the authentication event for a conventional HTTPS cookie session. Discard its unused key outputs rather than inventing another dashboard encryption protocol. Never send the client-only export key to the server. [R1 §§3.3, 6, 10.4]
- Bind each pending handshake to a fresh, expiring transaction identifier and the initiating browser/pre-auth context; consume it once. Keep expected MACs and provisional server secrets server-side. Enforce timeouts, count/body-size limits, parallel-tab isolation, and credential-generation checks. Fail closed after restart rather than persisting ephemeral AKE state unnecessarily.
- Protect all state-changing actions, including auth and enrollment transitions, against cross-origin requests: validate the canonical Origin, use CSRF tokens bound to the appropriate pre-auth/enrollment/session context, and disallow permissive credentialed CORS. SameSite cookies are defense in depth, not the entire CSRF design. Serve auth responses `Cache-Control: no-store` and prohibit third-party scripts on password pages. Cookie attribute semantics are specified in [H1]; the concrete policy here is a recommendation.

### One-time enrollment/recovery authority

- Disable ordinary enrollment after successful bootstrap. Recovery grants authorize replacement of the **existing single admin's** record, not creation of a second account or bypass to arbitrary dashboard access.
- Authorize both registration-start and record-installation. Bind purpose, admin slot, credential generation, and pending transaction together; recheck authorization at final commit. Atomically consume the grant and replace/install the record so concurrent requests cannot race to overwrite it. The 2023 Serenity review specifically demonstrated registration races and record-overwrite/account-hijacking mistakes in examples; copying a demonstration server is not an authorization design. [A1, OPA-01-003/004]
- Keep grant-bearing URLs out of proxy/access/error logs, analytics, and Referer propagation. Prefer a URL fragment consumed by same-origin JS and explicitly exchanged in a POST, followed by URL cleanup, or another design that prevents routine URL logging. Never consume the grant merely on GET (link previews can fetch URLs). Grant redemption can establish only narrowly scoped enrollment authority, with its own expiration and CSRF protection.
- An attacker with local CLI recovery authority can replace the password record. That is intentional administrative recovery power and should be documented, not portrayed as an OPAQUE security failure.

### Password change and forgotten-password recovery

- **Password change:** require recent successful OPAQUE authentication using the old password, then execute a fresh browser registration with the new password and newly sampled registration randomness. Atomically replace the old record; bump the credential generation, revoke old sessions and pending logins, and require a new login. Never POST old/new passwords to a server-side password-change handler. [R1 §8]
- **Forgotten password:** use the CLI-issued one-time recovery grant as replacement authorization, then the same browser re-registration flow. This replaces credentials; it cannot recover the old password. Preserve a consistent old credential until the replacement is committed, or explicitly choose a documented recovery lockout policy. Expiry/failure must not leave a half-installed record.
- Suite, identity-binding, KSF, server-key, or OPRF-seed changes can invalidate existing credentials; treat such changes as versioned migration/re-registration work, not routine configuration edits. Re-registration changes client export-key material, so **do not derive the repository-key storage master key from the admin password/export key**. The design already uses an independent master key outside SQLite. Otherwise recovery could strand encrypted repository keys. [R1 §§8, 10.4]

## 3. Persistence, secret storage, and backups

The RFC explicitly requires persistence of the record, credential identifier, applicable client identity, `oprf_seed`, and server private key. Losing the seed/private key while retaining SQLite records is not a usable authentication backup. [R1 §§3.1, 5, 10.10–10.12]

| Material | Proposed handling |
| --- | --- |
| Server OPRF seed | Generate once using a CSPRNG; keep stable, secret, and backed up. Derive per-credential OPRF keys using stable unique identifiers. Do not regenerate on startup. |
| Server AKE private key and corresponding public key | Persist securely with a key/version identifier. Public key is public; private key is secret. Do not confuse this keypair with TLS certificates or repository SSH keys. |
| Admin registration record | Persist client public key, **secret masking key**, and envelope; bind to credential identifier, identity inputs, suite/KSF profile, record format, and credential generation. The masking key is explicitly sensitive under RFC §10.10. |
| Profile metadata | Preserve exact algorithms, KSF costs/salt/output length, identity encodings, context, and format/library versions; required to interpret the record after upgrades/restoration. |
| Bootstrap/recovery tokens | Persist hashes and expiry/purpose/use state only; no plaintext token backups or logs. |
| Web sessions and pending AKE state | Separate from long-term OPAQUE keys. Prefer invalidating sessions on restore; expire pending exchanges rather than restoring them. |

Recommendation: protect long-term OPAQUE secrets separately from the database (or encrypt them under externally held key material with clear key separation), restrict permissions, and encrypt backups. Back up matching database/key generations and test restoration. Do not rotate a missing seed or key automatically to make startup appear successful; fail closed and use deliberate recovery/re-registration. Restoring old database snapshots can revive old passwords, spent recovery grants, and sessions unless recovery/restore policy explicitly invalidates them. These are application operational requirements inferred from the protocol's persistence requirements, not a library-provided backup facility. [R1 §§5, 8, 10.11–10.12]

## 4. Implementation landscape: exact RFC versus drafts

Sources below were inspected directly; no claim of an exhaustive ecosystem census is made.

| Implementation | Evidence as of research date | Fit and qualification |
| --- | --- | --- |
| **Bytemare OPAQUE, Go v0.18.0** | README explicitly targets RFC 9807; maintained by RFC author Daniel Bourdrez; explicitly **not independently audited**. Source contains server/client APIs, deserializers, vectors, fuzz/tamper tests, and Ristretto/P-256 support. MIT. [G1–G4] | Leading **native-Go server candidate**. `go.mod` requires Go **1.26**, three direct Bytemare modules (`ecc`, `hash`, `ksf`) and seven indirect module requirements. No C dependency appears in that manifest. Do not confuse author expertise or release-provenance claims with an independent cryptographic audit. |
| **Serenity `@serenity-kit/opaque`** | Changelog says **1.0.0 upgraded to `opaque-ke` 4.0.0 matching RFC 9807**. Browser Rust/WASM wrapper; MIT. Default Ristretto255; separately published P-256 package. npm's `latest` resolved to **1.1.0** during this research, whereas repository manifests/changelog already say **1.1.1**. [J1–J5] | Leading **browser candidate**, not a Go server library. Pin and inspect the actual npm tarball; do not assume repository fixes are in `latest`. 1.1.1's changelog includes malformed-parameter WASM memory-leak fixes. |
| **Meta `opaque-ke`, Rust** | 4.0.0 release notes explicitly synchronize with RFC 9807; 3.0.0 targeted draft-16; 2.0.0 targeted draft-10. MIT OR Apache-2.0. Current README recommends 4.1.0-pre.2, while later dependency remediation is under **Unreleased**. [K1–K2] | Relevant implementation underlying Serenity. Direct Rust FFI or a WASM runtime inside Go is an **alternative integration architecture**, not existing native-Go support. Adds runtime/FFI/build complexity and still needs review; not recommended just to avoid testing interoperability. Use the RFC's **3DH** instantiation, not the library's optional SIGMA-I or hybrid KEM variants. |
| Cloudflare `opaque-ts` | README explicitly targets **OPAQUE draft-07**. Uses `@cloudflare/voprf-ts`, WebCrypto, and `@noble/hashes` scrypt. BSD-3-Clause. [D1] | **Not eligible as inspected** for the final-RFC requirement. TypeScript/browser convenience does not establish RFC 9807 interoperability. |
| `cretz/gopaque` | README targets **draft-krawczyk-cfrg-opaque-01**, uses Kyber, and explicitly warns the trivial implementation is unreviewed. [D2] | **Not eligible** as an RFC 9807 Go server. |
| `marucjmar/opaque-wasm` | README says **not maintained**, directs users to Serenity; manifest uses `opaque-ke` **2**, whose release notes target draft-10. [D3, K2] | **Not eligible** as an RFC 9807 browser dependency. |
| `stef/libopaque` | C library advertises JS and Go bindings, libsodium/liboprf dependencies, draft-based protocol and additional variants. Inspected README does **not** establish final-RFC conformance and describes a different registration API/flow. [D4] | Final-RFC interoperability **unverified**, not a fallback to assume compatible. Native libraries/cgo plus JS/WASM builds would add integration weight. Its one-step plaintext-password registration variant must never be used here. |

### Audit and current-advisory limits

- **Serenity review:** 7ASecurity conducted 59 working days of review in October–November **2023**, including wrapper/protocol comparison, fuzzing, supply chain, threat model, privacy, and examples. It reported no directly exploitable library vulnerability and five hardening recommendations, mostly concerning examples. The audit is useful evidence but **predates RFC 9807 and Serenity 1.x**; it does not validate the Go server. [A1 pp.3–8, conclusion]
- **`opaque-ke` history:** the maintainer reports an NCC Group June **2021** review of v0.5.0, with fixes incorporated by v1.2.0. The old report URL currently redirected to a generic NCC research page when checked here, so this historical scope is attributed to the maintainer, not a newly reviewed report. No contemporary full RFC-9807 cross-language deployment audit was established. [K1]
- **November 2026 blocker:** RustSec **RUSTSEC-2026-0387**, issued October 23, reports secret-dependent timing in `crypto-bigint` operations intended to be constant-time; patched versions are **>=0.7.6**. Current `opaque-ke` main says its RustCrypto migration removes the affected 0.5 path, but the changelog places this under **Unreleased**. Serenity's inspected manifest still uses `opaque-ke = "4.0.0"` and optional `p256 = "0.13"`. **Do not claim the published WASM is patched.** Determine the precise resolved build graph and whether affected operations are present/reachable in the selected Ristretto or P-256 WASM artifact. Practical exploitability in this application is **unverified**, not established merely by the dependency name. [A2–A3, J2, K2]

## 5. Cryptographic profile and interoperability contract

Use a configuration recommended in **RFC 9807 §7**, not an invented curve/hash combination. Two relevant starting points are:

| Profile | RFC-recommended primitives |
| --- | --- |
| Ristretto candidate | OPRF `ristretto255-SHA512`; 3DH group ristretto255; HKDF-SHA-512; HMAC-SHA-512; SHA-512; Argon2id. |
| P-256 alternative | OPRF `P256-SHA256`; 3DH group P-256; HKDF-SHA-256; HMAC-SHA-256; SHA-256; Argon2id. The RFC also gives a P-256/scrypt profile. |

The RFC's Argon2id example uses a 16-byte zero salt, parallelism 4, **2^21 KiB (2 GiB)** memory, one pass, version 0x13, and output length `Nh`. This is not a realistic default to adopt blindly in browsers. RFC §7 leaves the KSF application-defined; §8 discusses the performance/security tradeoff. Serenity defaults to **64 MiB, three passes, parallelism 4**, and its `rfc-recommended` option actually uses **2^21−1 KiB** because exactly 2^21 crashed in browsers. That option is **not byte-for-byte the RFC example configuration**. A browser-appropriate Argon2id profile can still instantiate RFC 9807, but must be explicitly specified, versioned, and accepted, not mislabeled as the exact example. [R1 §§7–8; J1, J3]

Serenity's README incorrectly describes stretching as happening before the OPRF and mentions a start-function parameter; inspected Rust source instead applies its KSF via the **finish** parameters. Follow RFC §5.2.3/§6.3.2 and the actual pinned API/source: stretch the **OPRF output**, not a DIY hash of the input password. The source uses Argon2id v0x13 with 16 zero salt bytes. [J1, J3; R1]

Agreement must include all of the following, not just a common curve:

- Final RFC transcript/domain-separation rules (`OPAQUEv1-` rather than old draft strings), OPRF mode, exact group encodings and message layout, hash/KDF/MAC, and **3DH**.
- Password bytes/UTF-8 policy, identity values and bytes (or explicit RFC default-to-public-key behavior), stable credential identifier, and identical application context. Serenity's inspected wrapper passes **no custom context** (`None`); Bytemare exposes `Context`. Start the candidate comparison with the matching empty context, or deliberately extend the wrapper rather than changing one side alone. [J3; G2; K2]
- KSF costs, version, salt and output length. KSF execution is client-side; Go server configuration alone cannot fix a mismatched browser KSF. Preserve the client profile used at enrollment.
- Base64url versus binary transport. Serenity uses **URL-safe base64 without padding** around serialized protocol messages. Decode those bytes into Go message deserializers and encode replies consistently; this does not imply its serialized server setup/internal state is a portable Go format. [J3]
- Strict validation and fixed limits for each serialized message, no automatic draft/suite downgrade, and compatibility of existing persisted records across dependency upgrades. [R1 §§7–10; G3; K2]

The RFC's Appendix C contains deterministic real/fake vectors. Bytemare includes tests comparing registration, KE1/KE2/KE3, and key outputs against its vector file. This is valuable evidence of a conformance test suite, **not proof that this research ran it or that a browser/Go pair interoperates**; even the upstream test comment's broad interoperability claim should not substitute for application cross-client tests. [R1 Appendix C; G3]

## 6. Browser/WASM integration weight

Serenity's upstream build uses Rust/wasm-bindgen plus JS build tooling and Rollup, inlining WASM in CJS/ESM outputs. The runtime package has no declared npm runtime dependencies; this **does not mean dependency-free cryptography**: Rust crypto, Argon2, randomness, serialization, and wasm-bindgen are compiled into the artifact. Rebuilding/auditing requires that toolchain and its dependency closure. The Go program can embed and serve the prebuilt JS asset; no runtime Node.js service or SPA framework is inherently required. Await `opaque.ready` before invoking the library. [J2–J5]

**Artifact measurement performed, not a browser benchmark:** downloaded the npm **1.1.0** tarball without installing/executing it. `esm/index.js` is **434,604 bytes**; locally gzip-compressing that file produced **159,395 bytes** (~156 KiB). The registry reports **889,725 bytes unpacked** for the package, including both module formats. These are package/file measurements, **not** a minimized client-only production bundle or its measured HTTP transfer size. The inlined WASM includes server functions too; removing JS exports does not necessarily eliminate server crypto inside WASM. [J5–J6]

Recommendations for the embedded SSR design:

- Load the crypto asset only on enrollment/login/password-change pages; ordinary authenticated SSR pages need not retain it.
- Serve pinned first-party assets from the Go binary, not a runtime CDN; retain MIT and transitive license notices and verify release integrity. Keep build-time tooling distinct from deployment runtime requirements.
- Run expensive synchronous WASM/Argon2 work in a dedicated worker if needed for responsiveness. Measure desktop/mobile browser memory, latency, cancellation and failure behavior before deciding worker design and KSF costs. No claim that the supplied wrapper is already nonblocking is made.
- Validate production CSP/WASM compilation requirements (including any required `wasm-unsafe-eval` allowance), module-worker loading, secure randomness, password-manager behavior, and supported browser versions. Do not broadly enable JavaScript `unsafe-eval` as a workaround.
- If JS/WASM initialization fails or is unavailable, fail with a useful error. A plaintext-password form fallback would violate the explicit requirement.

## 7. Gates before dependency approval

1. **Security/release gate:** select exact Go tag and npm tarball/build commit; reconcile npm 1.1.0 versus repository 1.1.1 fixes; establish artifact provenance and transitive licenses; resolve RUSTSEC-2026-0387 for the selected artifact; decide whether independent review of the unaudited Go implementation/integration is required.
2. **Conformance/interoperability gate:** run final RFC vectors for pinned libraries, then execute browser registration → Go storage → browser login with matching session keys and server KE3 validation. Test wrong password, malformed/truncated/invalid-element messages, changed identities/context/profile, tampered messages, replay, expired/crossed/concurrent transactions, and final-step failure. Verify that password/client state/export key are absent from network traffic and logs.
3. **Lifecycle gate:** single-admin bootstrap races, token reuse/expiry, recovery versus concurrent login/change, atomic record replacement, session revocation, restart, matched backup restore, deliberate missing-key failure, and dependency upgrade compatibility. A recovered old backup must not accidentally resurrect spent grants.
4. **Browser gate:** actual supported browsers under deployed proxy/HTTPS/CSP, worker behavior, realistic memory/latency measurements, and selected Argon2id profile. Record the final artifact sizes separately from this research's package measurement.

**Evidence status:** protocol/specification, source/manifests/changelogs, audit report, advisory, and package sizes were inspected. **No cryptographic implementation, upstream test suite, cross-language login, application integration, browser performance test, or full dependency vulnerability scan was executed.** Consequently there is no approved/tested production browser+Go pairing yet. These gaps do not justify weakening the RFC requirement; they identify the work needed to satisfy it responsibly.

## Primary sources

All accessed 2026-11-12. Pinned source snapshots avoid treating moving `main` branches as version guarantees.

- **[R1]** [RFC 9807](https://www.rfc-editor.org/rfc/rfc9807.html), particularly §§3–8, 9–10 and Appendix C; [RFC information/status/errata entry](https://www.rfc-editor.org/info/rfc9807). OPRF dependency: [RFC 9497](https://www.rfc-editor.org/rfc/rfc9497.html).
- **[G1]** [Bytemare v0.18.0 README](https://github.com/bytemare/opaque/blob/v0.18.0/README.md), RFC claim, audit disclaimer, operational responsibilities, MIT license.
- **[G2]** [Bytemare v0.18.0 configuration source](https://github.com/bytemare/opaque/blob/v0.18.0/opaque.go) and [module manifest](https://github.com/bytemare/opaque/blob/v0.18.0/go.mod).
- **[G3]** [Bytemare vector tests](https://github.com/bytemare/opaque/blob/v0.18.0/tests/vectors_test.go), [vectors](https://github.com/bytemare/opaque/blob/v0.18.0/tests/vectors.json), [test directory](https://github.com/bytemare/opaque/tree/v0.18.0/tests).
- **[G4]** [Bytemare server API](https://github.com/bytemare/opaque/blob/v0.18.0/server.go), [server key material](https://github.com/bytemare/opaque/blob/v0.18.0/skm.go); tag commit `b85ab30b461163e64aa0f50dbfb6f744454c783e` (2026-03-24).
- **[J1]** [Serenity README](https://github.com/serenity-kit/opaque/blob/e7b7664bcd0ad80453a4fa256b08f7f67791f949/README.md), APIs, KSF options and maintainer timing observations (not independently benchmarked here).
- **[J2]** [Serenity Cargo manifest](https://github.com/serenity-kit/opaque/blob/e7b7664bcd0ad80453a4fa256b08f7f67791f949/Cargo.toml), Rust/WASM dependencies.
- **[J3]** [Serenity Rust wrapper](https://github.com/serenity-kit/opaque/blob/e7b7664bcd0ad80453a4fa256b08f7f67791f949/src/lib.rs), suites, KSF, encoding, state, context.
- **[J4]** [Serenity changelog](https://github.com/serenity-kit/opaque/blob/e7b7664bcd0ad80453a4fa256b08f7f67791f949/CHANGELOG.md), RFC transition and subsequent fixes.
- **[J5]** [Serenity Rollup configuration](https://github.com/serenity-kit/opaque/blob/e7b7664bcd0ad80453a4fa256b08f7f67791f949/rollup.config.js), [package manifest](https://github.com/serenity-kit/opaque/blob/e7b7664bcd0ad80453a4fa256b08f7f67791f949/build/ristretto/package.json), [MIT license](https://github.com/serenity-kit/opaque/blob/e7b7664bcd0ad80453a4fa256b08f7f67791f949/LICENSE). Snapshot commit dated 2026-10-23.
- **[J6]** npm [registry metadata for 1.1.0](https://registry.npmjs.org/@serenity-kit/opaque/1.1.0) and [published tarball](https://registry.npmjs.org/@serenity-kit/opaque/-/opaque-1.1.0.tgz). Artifact integrity from registry: `sha512-Y6v/+hRMn0MdMEk5+/ArM0vPIiFfFEbdTZc7oAx+cWyvGODGezAQ/sMjXBVogf7NsS9z9EV0Ve5paZCVULuedw==`.
- **[K1]** [`opaque-ke` README](https://github.com/facebook/opaque-ke/blob/070eac916fa58c71029a13021c127861f58d11d1/README.md), RFC claim, audit history, licensing and current migration notes.
- **[K2]** [`opaque-ke` changelog](https://github.com/facebook/opaque-ke/blob/070eac916fa58c71029a13021c127861f58d11d1/CHANGELOG.md), exact draft/RFC versions, setup migration, unreleased remediation. Snapshot commit dated 2026-11-10.
- **[A1]** [7ASecurity Opaque penetration test/whitebox report (2023)](https://7asecurity.com/reports/pentest-report-opaque.pdf), reviewed directly; especially introduction, methodology, OPA-01-003/004, Threats 14–15, OPA-01-Q02, and conclusion.
- **[A2]** [RustSec RUSTSEC-2026-0387](https://rustsec.org/advisories/RUSTSEC-2026-0387.html), issued 2026-10-23.
- **[A3]** [RustCrypto remediation PR #1365](https://github.com/RustCrypto/crypto-bigint/pull/1365), linked by the advisory; remediation claim also in [K1–K2]. No application exploit reproduced.
- **[D1]** [Cloudflare opaque-ts README](https://github.com/cloudflare/opaque-ts/blob/main/README.md), draft-07, dependencies, BSD-3-Clause.
- **[D2]** [cretz/gopaque README](https://github.com/cretz/gopaque/blob/master/README.md), old draft and security warning.
- **[D3]** [opaque-wasm README](https://github.com/marucjmar/opaque-wasm/blob/master/README.md) and [Cargo manifest](https://github.com/marucjmar/opaque-wasm/blob/master/Cargo.toml), unmaintained status and `opaque-ke` 2 dependency.
- **[D4]** [stef/libopaque README](https://github.com/stef/libopaque/blob/master/README.md), language bindings, dependencies and protocol variants.
- **[H1]** [RFC 6265 §4.1.2](https://www.rfc-editor.org/rfc/rfc6265.html#section-4.1.2), cookie scope/Secure/HttpOnly; [HTTP Working Group cookie specification](https://httpwg.org/http-extensions/draft-ietf-httpbis-rfc6265bis.html), SameSite and cookie prefixes. These describe cookie mechanisms, not an OPAQUE HTTP-session standard.
