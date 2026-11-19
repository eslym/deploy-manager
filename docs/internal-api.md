# Internal HTTP API — maintainer reference

This documents the implemented HTTP boundary between the embedded SPA and Go,
plus GitHub/Gitea webhook ingestion. It is **not a public API**, an automation
interface, or a compatibility/versioning promise. There are no API tokens.
Change the frontend and backend together when changing these contracts.

The route inventory lives in [`App.Handler`](../lib/bootstrap.go). Application
logic, authorization, and persistence belong in Go, not SvelteKit server routes.
This reference describes current handlers; [design.md](design.md) remains the
requirements source, not evidence that a planned behavior is implemented.
Use [GLOSSARY.md](../GLOSSARY.md) for deployment/release terminology.

## Contents

- [Shared HTTP behavior](#shared-http-behavior)
- [Authentication and account lifecycle](#authentication-and-account-lifecycle)
- [Projects and cached refs](#projects-and-cached-refs)
- [Deployments and jobs](#deployments-and-jobs)
- [Repository keys and SSH host trust](#repository-keys-and-ssh-host-trust)
- [LFS destination settings](#lfs-destination-settings)
- [Webhook configuration and ingestion](#webhook-configuration-and-ingestion)
- [Maintaining this reference](#maintaining-this-reference)

## Shared HTTP behavior

### Routing, transport, and authorization

- Paths below are relative to the application origin. `{id}` is the relevant
  project, deployment, or repository-key ID, not its display name. Generated
  resource IDs are 32 lowercase hexadecimal characters; commits are full
  40-character SHA-1 IDs.
- All dashboard endpoints require a valid administrator session, including reads.
  Exceptions are the OPAQUE setup/recovery/login flows and logout's ability to
  clear an already absent session. Webhooks authenticate by signature instead.
- Dashboard mutations require a same-origin `Origin` header and POST. Origin
  checking plus SameSite cookies provides CSRF protection; there is no separate
  CSRF-token field. Reads use session authorization without the mutation Origin
  check.
- HTTPS is the default. `AllowHTTPOrigin` explicitly permits authentication and
  dashboard traffic on the configured HTTP origin only. It does **not** permit
  HTTP webhook ingestion. Setup/recovery registration additionally supports the
  narrowly checked `InsecureLocalHTTP` loopback mode; this does not enable HTTP
  login or protected dashboard requests by itself.
- HTTPS may be direct TLS or `X-Forwarded-Proto: https` from a trusted proxy
  (loopback peers are trusted). Forwarded headers from other peers are not enough.
  When `CanonicalOrigin` is configured, a different request Host returns **421**.
- `projectAuthorized` in [`projects.go`](../lib/projects.go) is the shared
  dashboard guard: **426** for insecure transport, **401** for no valid session,
  **403** for a rejected mutation Origin, **429** for its mutation rate limit.
  The limiter is shared with authentication, keyed by the connection peer IP,
  and configured by `AuthRequestsPerMinute` (default 20/minute).

### Bodies, responses, and errors

Send JSON bodies with `Content-Type: application/json`. Successful JSON responses
use that media type; **204** has no response body. Times are JSON RFC 3339
timestamps, not Unix integers. List responses are bare arrays unless stated
otherwise. Optional fields marked below may be omitted.

Errors generally use Go's `http.Error`: `text/plain; charset=utf-8` with a trailing
newline, **not** a JSON error envelope. Read the status before parsing JSON.
Human-readable error strings are not machine-readable codes.

| Status | Typical meaning |
| --- | --- |
| 400 | Invalid JSON, field validation, unavailable selected Git object/ref |
| 401 | Missing/expired session or failed login/webhook signature |
| 403 | Origin, transport, or request-policy rejection in protocol/webhook handlers |
| 404 | Unknown route/resource, or webhook not configured |
| 405 | Wrong method for a registered path |
| 409 | Conflict: target/state changed, destination occupied, trust or dependency restriction |
| 410 | Expired, consumed, or invalid registration grant/transaction |
| 413 | Body or storage/resource limit exceeded where explicitly handled |
| 415 | JSON media type required by OPAQUE/key creation |
| 421 / 426 | Wrong configured Host / HTTPS required |
| 429 | Dashboard mutation rate limit or login/session capacity limit |
| 500 / 502 / 503 | Internal failure / upstream authentication failure in Git operations / unavailable capacity or recovery required |

These are shared categories, not a guarantee that every handler uses each code.
For example, OPAQUE's shared decoder and logout return **403**, not **429**, when
their shared rate check rejects a request. Many bounded JSON decoders report an
oversized/invalid body as **400**, not **413**. Most mutation decoders reject
unknown fields, but enforcement is not uniform; only OPAQUE and repository-key
creation explicitly require a single JSON value with no trailing data.

`/api/`, `/opaque/`, and `/webhooks/` responses have `Cache-Control: no-store`.
Unknown backend paths never fall back to the SPA. Frontend shell fallback is
restricted to recognized GET/HEAD navigations accepting HTML; missing assets and
backend errors keep their real error statuses. A maintenance-locked daemon uses
[`MaintenanceHandler`](../lib/maintenance.go) instead of application routing:
**503** plain text for all paths, without loading the SPA or accessing app data.

## Authentication and account lifecycle

Sources: [`bootstrap.go`](../lib/bootstrap.go),
[`auth.go`](../lib/auth.go), [`lifecycle.go`](../lib/lifecycle.go).
See also [bootstrap-admin.md](bootstrap-admin.md).

Passwords never appear in HTTP JSON. `value` is an unpadded base64url encoding
of the serialized OPAQUE message/record, produced or consumed by the pinned
browser crypto implementation. The single credential identifier is
`administrator`; there is no username field. Protocol bodies are limited to
65,536 bytes and `value` to 60,000 encoded characters. Unknown fields, missing
values, and trailing JSON are rejected.

| Method and path | Request JSON | Success |
| --- | --- | --- |
| POST `/opaque/register/start` | `{"grant":"<setup-or-recovery-token>","value":"<registration-request>"}` | 200 `{"value":"<registration-response>","transaction":"<handle>"}` |
| POST `/opaque/register/finish` | `{"transaction":"<handle>","value":"<registration-record>"}` | 204; creates/replaces credentials, does not sign in |
| POST `/opaque/login/start` | `{"value":"<KE1>"}` | 200 `{"transaction":"<handle>","value":"<KE2>"}` |
| POST `/opaque/login/finish` | `{"transaction":"<handle>","value":"<KE3>"}` | 204 plus session cookie after KE3 verification |
| GET `/api/session` | None | 200 `{"authenticated":true}`; otherwise 401 (not `false`) |
| POST `/api/logout` | No body required | 204; invalidates current session if present and expires cookie |
| POST `/opaque/password/change/start` | `{"value":"<new-password-registration-request>"}` | 200 `{"value":"<registration-response>"}` |
| POST `/opaque/password/change/finish` | `{"value":"<new-password-registration-record>"}` | 204; replaces credentials and revokes sessions |

### Flow and state details

1. **Setup/recovery:** local CLI commands issue a short-lived, single-use grant.
   The browser reads it from the `/register` or `/recover` URL fragment and
   removes the fragment before submission. Never put grants in query parameters,
   resource URLs, logs, or documentation examples with real values.
2. Both use `/opaque/register/*`. The server chooses setup before an
   administrator exists and recovery afterward. Start validates the grant and
   creates an expiring registration transaction bound to its purpose, credential
   generation, and Origin. Finish sends the transaction, **not the grant**.
   Success consumes the grant; recovery replaces the existing account rather
   than creating another administrator.
3. **Login:** start/finish use KE1/KE2/KE3. A pending login is bound to Origin,
   peer IP, credential generation, and expiry (default 2 minutes). Finish consumes
   the handle even on verification failure. Only verified KE3 creates an
   independent random bearer session cookie; OPAQUE outputs are not session tokens.
4. **Password change:** first complete a new OPAQUE login to obtain a recently
   authenticated session (default window 5 minutes), then perform registration
   with the new password. Unlike setup/recovery, password change uses neither a
   grant nor a transaction field. Both change endpoints check recent
   authentication. Recovery and password change atomically advance credential
   generation, revoke sessions/pending logins, and require login again.

TLS sessions use `__Host-deploy-manager-session`, `Path=/`, `Secure`, `HttpOnly`,
and `SameSite=Strict`. Explicit HTTP-origin mode uses `deploy-manager-session`
without `Secure`. Default session lifetime is 12 hours. Sessions and pending
handshakes live in memory and are lost on process restart; credential records
and hashed grants are persistent. Repository-key encryption is independent of
password changes.

## Projects and cached refs

Source: [`projects.go`](../lib/projects.go), with deletion in
[`deletion.go`](../lib/deletion.go).

### Response shapes

**Project:** `id`, `name`, `url`, `lastFetched` (timestamp or null),
`gitCacheBytes`, `lfsCacheBytes`; optional `warning` and `keyId`.
The creation response uses this shape but leaves cache-size counters at zero;
list again for measured sizes.

**Ref:** `{"name":"main","kind":"branch","commit":"<full-sha1>"}`.
`kind` is `branch` or `tag`; names do not include `refs/heads/` or `refs/tags/`.

| Method and path | Request / success | Important behavior |
| --- | --- | --- |
| GET `/api/projects` | 200 Project[] | Ordered by creation time then ID; measures Git/LFS cache sizes. |
| POST `/api/projects` | `{"name":"site","url":"https://git.example/site.git"}`; 201 Project | Synchronously clones without checkout and saves cached refs; not a queued job. |
| GET `/api/projects/{id}/refs` | 200 Ref[] | Cached only, sorted by kind/name; does not fetch. |
| POST `/api/projects/{id}/refresh` | No body required; 200 `{"lastFetched":"<timestamp>","warning":""}` | Synchronously fetches; failed refresh preserves cached refs and last successful fetch information. |
| POST `/api/projects/{id}/key` | `{"keyId":"<key-id>"}`; 204 | SSH projects only; changes assigned managed key, does not fetch. |
| POST `/api/projects/{id}/cache/recreate` | `{"confirm":true}`; 204 | Clears Git and verified LFS caches; see limitation below. |
| POST `/api/projects/{id}/delete` | `{"confirm":true}`; 204 | Blocked with 409 while deployments exist; removes project caches/settings. |

Project creation accepts `keyId` for an `ssh://` URL or conventional SCP-style
SSH origin (for example, `git@github.com:owner/repo.git`) and requires it for SSH.
HTTPS is anonymous only and rejects `keyId` and credential-bearing URLs.
SCP-style origins use the default SSH port and preserve their relative repository
path semantics for Git LFS authentication. Local/file URLs and other unsupported
Git transports are not accepted. Names must be nonblank and at most 100 bytes.
Create bodies are limited to 8 KiB. Unknown/changed SSH keys block clone/fetch
pending independent trust approval; creation may return **409** while recording
a host warning.

**Cache-clearing limitation:** despite its route name, cache recreation currently
clears caches without immediately cloning them again. A later branch fetch or
Hard re-deploy can rebuild the Git cache; ordinary tag/commit Deploy relies on
cached objects and can fail after clearing. Live files are untouched, but
unreachable commits and unavailable LFS payloads can be lost permanently.
Incomplete clearing returns **503** and may block further cache use until restart
reconciliation. See [the README cache guidance](../README.md#caches-and-job-history).

Project/deployment deletion records durable cleanup work. A **503** can mean the
deletion was accepted but cleanup is blocked and will retry, not that nothing
changed. Inspect current state before repeating destructive actions.

## Deployments and jobs

Sources: [`deployments.go`](../lib/deployments.go),
[`rollback.go`](../lib/rollback.go), [`automation.go`](../lib/automation.go),
[`publication_move.go`](../lib/publication_move.go),
[`publication_paths.go`](../lib/publication_paths.go), and
[`deletion.go`](../lib/deletion.go).

### Response shapes

**Deployment:** `id`, `projectId`, `name`, `commit`, `targetKind`,
`automationPaused`, `status`, `publicationPath`, `createdAt`, `updatedAt`;
optional `targetName`, `liveCommit`, `error`.
`commit` is the stored resolved target state; `liveCommit` identifies published
content. Do not infer current live content solely from `commit` or `status`.
`publicationPath` in responses is the complete server-side path.

**Job:** `id`, `deploymentId`, `commit`, `force`, `status`, `log`, `createdAt`,
`updatedAt`. `force` indicates Hard re-deploy. Logs are sanitized text, not a
structured event stream.

**Queued response:** `{"jobId":"<job-id>","status":"queued"}`.
This confirms queue acceptance, not successful publication.

### Route inventory

| Method and path | Request / success | Important behavior |
| --- | --- | --- |
| GET `/api/deployments` | 200 Deployment[] | All deployments; no project filter or pagination. |
| GET `/api/deployments/root` | 200 `{"publicationRoot":"<server-path>"}` | Configured publication root for folder selection. |
| POST `/api/deployments` | Create body below; 201 `{"id":"<id>","jobId":"<job-id>","status":"queued","publicationPath":"<server-path>"}` | Saves deployment and automatically queues initial publication. |
| POST `/api/deployments/{id}/deploy` | No body required; 202 Queued response | Ordinary Deploy; branch fetches latest, tag/commit uses saved commit. |
| POST `/api/deployments/{id}/hard-redeploy` | No body required; 202 Queued response | Fetches; resolves branch/tag again; forces recreation even if already live. Commit targets stay fixed. |
| POST `/api/deployments/{id}/target` | Target body below; 202 Queued response | Changes configuration and queues publication; unchanged target returns 409. |
| GET `/api/deployments/{id}/jobs` | 200 Job[] | Newest first, at most 100; no pagination. Unknown deployment IDs currently yield an empty array. |
| GET `/api/deployments/{id}/history` | Query below; 200 `{"commits":[...],"page":1,"hasMore":false}` | Branch only; cached first-parent history, no fetch. |
| POST `/api/deployments/{id}/rollback` | `{"commit":"<full-sha1>","permanent":false}`; 202 Queued response | Branch only; ancestry validation happens in the worker after fetching. |
| POST `/api/deployments/{id}/pause` | No body required; 200 `{"automationPaused":true}` | Branch only; repeated pause succeeds. |
| POST `/api/deployments/{id}/resume` | No body required; 202 `{"automationPaused":false,"jobId":"<job-id>","status":"queued"}` | Branch only; immediately queues latest tip; already enabled returns 409. |
| POST `/api/deployments/{id}/move` | Move body below; 200 `{"publicationPath":"<new-server-path>"}` | Changes destination, not Git target/content; synchronous durable move, no job. |
| POST `/api/deployments/{id}/delete` | `{"confirm":true}`; 204 | Supersedes work and removes published files; blocked cleanup can return 503. |

Create body (8 KiB limit):

```json
{
  "projectId": "<project-id>",
  "name": "Production",
  "publicationPath": "production",
  "targetKind": "branch",
  "targetName": "main"
}
```

Target body (4 KiB limit) uses `targetKind`, `targetName`, and/or `commit`:

- `branch` / `tag`: `targetName` must select an existing cached ref (up to 255
  bytes); the server resolves `commit` rather than trusting a supplied one.
- `commit`: send `{"targetKind":"commit","commit":"<full-sha1>"}` with an object
  already available in the project cache. Omitted `targetKind` defaults to
  `commit`. Use lowercase full SHA-1 IDs, not abbreviated history labels.
- Create additionally requires a nonblank name of at most 100 bytes.

`publicationPath` in **create/move requests** is a single folder name, not the
complete response path: 1–100 ASCII characters matching
`[a-zA-Z0-9][a-zA-Z0-9._-]*`, excluding names shaped like generated resource IDs.
No path separators, traversal, symlinks, occupied paths, or reserved destinations
are allowed. Omission on create retains the historical ID-named destination.
The display name is independent of the publication folder.

Move body (4 KiB limit):

```json
{
  "publicationPath": "new-production",
  "expectedPublicationPath": "/srv/deploy-manager/publications/production",
  "confirm": true
}
```

`expectedPublicationPath` must match the current complete path; stale review
returns **409**. Confirmation acknowledges that the external web server document
root may need updating. An unpublished deployment only changes its binding;
a published one moves the verified directory atomically. A disconnected client
does not cancel the durable move. **503** after an interrupted move requires
reconciliation, not a blind repeat.

### History, rollback, and automation

History query parameters: `page` (default 1, range 1–100000), `limit` (default 20,
range 1–100), `search` (trimmed, at most 256 bytes). Search is case-insensitive
over full commit ID, subject, and author name; pagination applies to matches.
Each commit has `id`, `shortId` (12 characters), `subject`, `author`, and `date`.
Use `id` for actions. Browsing only follows first parents; actual rollback
eligibility includes any ancestor of the configured branch, not just displayed
first-parent commits.

Rollback fetches successfully before validating ancestry, and revalidates
against newer local branch state before publication. **202** does not prove
eligibility. It supersedes older queued/in-flight publications without changing
the configured branch. Omitted `permanent` defaults to false.

- Temporary rollback does not change the pause flag. For an enabled deployment,
  the next matching webhook/manual Deploy may publish the tip; finishing rollback
  itself does not immediately queue catch-up.
- Permanent rollback persists pause when the request is accepted, even if the
  rollback job later fails. Existing live files remain on ordinary preparation
  failure; pause stays set.
- Paused matching webhook work fetches only: no export, LFS hydration, or
  publication. Manual Deploy still publishes the tip without clearing pause.
  Resume clears pause and queues deployment.
- Changing a paused deployment to another branch preserves pause but queues its
  one-off publication. Switching to tag/commit removes branch-only pause.

### Job lifecycle and publication guarantees

Poll jobs and deployments after a queued response; there is no SSE/WebSocket or
job-by-ID route. Jobs use `queued` → `running` and terminal `succeeded`, `noop`,
`failed`, or `superseded`. A paused automatic fetch may finish as `noop` without
publishing. Deployment statuses include `queued`, `publishing`, `live`, `failed`,
`recovery_required`, `deleting`, and `deletion_failed`; these are distinct from
job statuses and from the persistent pause flag.

Publication is serialized per deployment with bounded global concurrency.
Workers reject stale generations after target changes, rollback, or deletion.
Ordinary Deploy records `noop` if the selected commit is already safely live;
Hard re-deploy recreates files. A hard tag refresh persists the new resolved pin
before export, so later preparation failure can leave `commit` different from
`liveCommit`.

Preparation/export/LFS failures keep the current publication. Switching uses
atomic directory operations plus durable intent/restart reconciliation;
ambiguous outcomes block further publication rather than blindly repeating a
switch. Replaced directories are cleaned after the reader grace period (default
60 seconds), not kept for rollback. Rollback recreates Git content and requires
available Git objects/LFS payloads.

Jobs/logs are persistent and retained for the configured period (default 30
days). Restart marks interrupted work failed; accepted durable automatic branch
work can be requeued as new work. Transient fetches alone get bounded retries;
authentication and host-trust failures are not automatically retried.

## Repository keys and SSH host trust

Sources: [`repository_keys.go`](../lib/repository_keys.go),
[`ssh_hosts.go`](../lib/ssh_hosts.go).
See [repository-keys.md](repository-keys.md) for supported import formats and
operational guidance.

| Method and path | Request / success |
| --- | --- |
| GET `/api/repository-keys` | 200 Key[] |
| POST `/api/repository-keys` | `{"name":"reader","generate":true}` **or** `{"name":"reader","privateKey":"<unprotected-key-text>"}`; 201 Key |
| POST `/api/repository-keys/{id}/delete` | No body required; 204; 409 if assigned to any project |
| GET `/api/ssh-hosts` | 200 Host[] |
| POST `/api/ssh-hosts/approve` | `{"host":"git.example","port":22,"fingerprint":"SHA256:<observed>","confirm":true}`; 204 |
| POST `/api/ssh-hosts/revoke` | `{"host":"git.example","port":22,"fingerprint":"SHA256:<observed>","confirm":true}`; 204 |

**Key:** `id`, `name`, `algorithm`, `bits`, `fingerprint`, `publicKey`, `createdAt`.
Private keys are never returned. Generation and import are mutually exclusive;
imports have a 64 KiB private-key limit (request limit 64 KiB + 2048 bytes).
Creation requires JSON media type and rejects unknown/trailing fields/data.
Private keys are encrypted at rest with an external master key.

**Host:** `host`, `port`, `fingerprint`, `warning`, `trusted`. The list includes
stored trusted hosts and hosts with warnings; it reflects observations/trust, not
a live SSH scan. `warning` may be empty for an approved host. Approve only after
verifying the fingerprint independently. Trust is hostname-and-port scoped;
**409** means the observed key changed or no longer matches the reviewed
observation. Approval does not itself retry clone/fetch; retry the failed
operation afterward.

Revocation requires `confirm: true` and the currently displayed host, port, and
fingerprint. It clears that host's trusted key and returns **204**; stale or
already-untrusted entries return **409**. The host remains listed with a warning.
Subsequent SSH connections are blocked until the observed fingerprint is
independently verified and approved again.

## LFS destination settings

Sources: [`lfs_settings.go`](../lib/lfs_settings.go),
[`lfs_transport.go`](../lib/lfs_transport.go).

| Method and path | Request / success |
| --- | --- |
| GET `/api/projects/{id}/lfs` | 200 `{"settings":{"endpoint":"","objectOrigins":[],"allowInternal":false},"suggestedEndpoint":"<url-or-empty>"}` |
| POST `/api/projects/{id}/lfs` | Body below (16 KiB limit); 204 |

Existing settings may additionally include `internalAddresses`, a map of origins
to approved internal IP-address arrays. `suggestedEndpoint` is a convenience
derived from the HTTPS repository URL, **not approval**.

```json
{
  "endpoint": "https://git.example/site.git/info/lfs",
  "objectOrigins": ["https://git.example", "https://objects.example"],
  "allowInternal": false,
  "confirmation": "I independently verified these LFS destinations"
}
```

Approve 1–16 unique HTTPS origins, including the endpoint's origin. Origins have
no paths or credentials; the endpoint has no credentials, query, or fragment.
Setting `allowInternal: true` additionally requires `internalConfirmation` with
the same exact confirmation phrase and safe resolution of internal addresses.
Invalid destinations/confirmation return **400**. Repository suggestions and
endpoint overrides are not silently trusted; downloads verify HTTPS, reject
redirects, and validate payload size and SHA-256. Saving settings does not deploy.

## Webhook configuration and ingestion

Source: [`webhooks.go`](../lib/webhooks.go).
`{provider}` below expands to **two registered routes**, `github` and `gitea`,
not an arbitrary provider parameter.

### Dashboard configuration (session + mutation Origin rules)

| Method and path | Request / success |
| --- | --- |
| GET `/api/projects/{id}/{provider}-webhook` | 200 `{"configured":true,"url":"/webhooks/<provider>/<project-id>"}` (`configured` may be false) |
| POST `/api/projects/{id}/{provider}-webhook` | `{"rotate":true}` (1 KiB limit); 201 `{"url":"/webhooks/<provider>/<project-id>","secret":"<new-secret>"}` |

The URL is relative to the application origin. Rotation also initializes an
unconfigured webhook. Each project/provider has an independent encrypted secret.
The new printable secret is returned only at rotation; GET never reveals it.
Providers must sign using the **literal returned secret string**, not bytes
obtained by hex-decoding it. Update provider configuration after rotation.

### Provider ingestion (signature, no dashboard session/Origin)

`POST /webhooks/github/{id}` and `POST /webhooks/gitea/{id}` return **204** after
durable acceptance, including duplicate/ignored deliveries. HTTPS is required
even in explicit HTTP-origin dashboard mode.

| Provider | Delivery ID | Event | HMAC-SHA256 signature over exact raw body |
| --- | --- | --- | --- |
| GitHub | `X-GitHub-Delivery` | `X-GitHub-Event` | `X-Hub-Signature-256: sha256=<64-hex-digits>` |
| Gitea | `X-Gitea-Delivery` | `X-Gitea-Event` | `X-Gitea-Signature: <64-hex-digits>` |

Body size is bounded by `WebhookMaxBodyBytes`. Invalid headers/push payloads
return **400**, bad signatures **401**, unconfigured webhooks **404**, oversized
bodies **413**, and queue/durable-acceptance failure **503**.

Push bodies use `ref`, `after`, and `deleted`; extra provider fields are ignored.
`after` must be a full SHA-1 ID, including the all-zero deletion marker.
GitHub push validation requires a branch ref. Gitea also accepts tag push refs
but does not publish tag-target deployments from them. Non-push events are
accepted/recorded without branch publication.

Deliveries are deduplicated by project, provider, and delivery ID. Only matching,
nondeleted branch updates queue automatic deployment work; branch deletion
leaves live files unchanged. Pending updates coalesce, and workers fetch the
latest tip rather than treating `after` as an instruction to publish that commit.
Paused deployments fetch without preparing/publishing; other deployments remain
independent. A **503** acknowledgement failure may occur after work was committed:
providers should retry with the same delivery ID so deduplication can apply.

## Maintaining this reference

When changing a route, update its handler, SPA caller, tests, and this reference
together. Do not turn undocumented incidental behavior into a new public
compatibility guarantee. In particular, avoid assuming every error is JSON or
every mutation is safely repeatable after a network failure.

Useful behavior checks:

| Area | Tests |
| --- | --- |
| Registration, login, credential lifecycle | [`bootstrap_test.go`](../lib/bootstrap_test.go), [`registration_transaction_test.go`](../lib/registration_transaction_test.go) |
| Routing/embedding/backend errors | [`assets_test.go`](../lib/assets_test.go) |
| Projects, cache, SSH | [`projects_test.go`](../lib/projects_test.go), [`projects_smart_test.go`](../lib/projects_smart_test.go), [`ssh_projects_test.go`](../lib/ssh_projects_test.go) |
| Deploy/jobs/tag refresh | [`deployments_test.go`](../lib/deployments_test.go), [`tags_app_test.go`](../lib/tags_app_test.go) |
| Rollback/pause/restart | [`rollback_app_test.go`](../lib/rollback_app_test.go), [`rollback_restart_test.go`](../lib/rollback_restart_test.go), [`automation_test.go`](../lib/automation_test.go) |
| Publication path/move/delete | [`publication_paths_test.go`](../lib/publication_paths_test.go), [`publication_move_test.go`](../lib/publication_move_test.go), [`deletion_test.go`](../lib/deletion_test.go) |
| Keys/LFS/webhooks | [`repository_keys_test.go`](../lib/repository_keys_test.go), [`lfs_test.go`](../lib/lfs_test.go), [`webhooks_test.go`](../lib/webhooks_test.go), [`gitea_webhooks_test.go`](../lib/gitea_webhooks_test.go) |

Combined-binary browser coverage and its build prerequisites are described in
[`tests/README.md`](../tests/README.md). For storage/process recovery rather than
HTTP contracts, use [process-coordination.md](process-coordination.md) and the
[operations guides](operations/).
