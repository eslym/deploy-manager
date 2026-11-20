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
  Its failures now use the outcome envelope described below.
  The limiter is shared with authentication, keyed by the connection peer IP,
  and configured by `AuthRequestsPerMinute` (default 20/minute).

### Bodies, responses, and errors

Send JSON bodies with `Content-Type: application/json`. Every registered
authentication, dashboard, webhook-configuration, and webhook-ingestion handler
uses the outcome envelope, as do shared authorization/security guards and
maintenance responses. Unknown paths under `/api/`, `/opaque/`, and
`/webhooks/` return `request.not_found` (404); unsupported methods on a
registered path return `request.method_not_allowed` (405) with `Allow`.
`HEAD` follows the matching `GET` route, including its status and headers, while
the HTTP server suppresses the response body. Frontend shell errors and requests
outside those application prefixes retain ordinary HTTP error responses.
Times are JSON RFC 3339 timestamps, not Unix integers. Resource arrays are in
envelope `data`. Optional fields marked below may be omitted.

Lifecycle mutations use named typed SDK operations and require explicit
confirmation where the route requires it. A non-2xx, invalid response, or
transport failure does not prove that a mutation was rejected. In particular,
`operation.cleanup_pending` with `operationAccepted: true` means durable
deletion was accepted and cleanup remains pending; `publication.recovery_required`
means inspect authoritative state and do not repeat the move automatically.
Callers may refresh status but must not automatically resubmit mutations.

Outcomes have exactly `code`, `message`, and `data`; `data` is `null` when the
operation has no response data. Messages are for display, never branching.
JSON outcomes use `application/json` and `Cache-Control: no-store`.

| Status          | Typical meaning                                                                                                  |
| --------------- | ---------------------------------------------------------------------------------------------------------------- |
| 400             | Invalid JSON, field validation, unavailable selected Git object/ref                                              |
| 401             | Missing/expired session or failed login/webhook signature                                                        |
| 403             | Origin, transport, or request-policy rejection in protocol/webhook handlers                                      |
| 404             | Unknown route/resource, or webhook not configured                                                                |
| 405             | Wrong method for a registered path                                                                               |
| 409             | Conflict: target/state changed, destination occupied, trust or dependency restriction                            |
| 410             | Expired, consumed, or invalid registration grant/transaction                                                     |
| 413             | Body or storage/resource limit exceeded where explicitly handled                                                 |
| 415             | JSON media type required by OPAQUE/key creation                                                                  |
| 421 / 426       | Wrong configured Host / HTTPS required                                                                           |
| 429             | Dashboard mutation rate limit or login/session capacity limit                                                    |
| 500 / 502 / 503 | Internal failure / upstream authentication failure in Git operations / unavailable capacity or recovery required |

The status belongs to the outcome, not its message. In particular, handler
validation may use `request.invalid` (400) for malformed or oversized JSON,
whereas explicitly bounded uploads can return `request.too_large` (413).
OPAQUE's shared decoder and logout use **403** for their shared request-rate
limit; protected dashboard mutations use **429**. Unknown-field and trailing
JSON validation is route-specific.

The following is a semantic-code index, not a substitute for the route catalog
below. Statuses shown with multiple values depend on the route or failure stage;
`message` is display text, never a control-flow value.

| Codes                                                                                                                                                                                                                                                                                                            | Status                                                                |
| ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | --------------------------------------------------------------------- |
| `auth.registration_started`, `auth.registration_completed`, `auth.login_started`, `auth.login_completed`, `auth.session_valid`, `auth.logged_out`, `auth.password_change_started`, `auth.password_changed`                                                                                                       | 200                                                                   |
| `auth.session_required`, `auth.login_failed`, `auth.recent_login_required`                                                                                                                                                                                                                                       | 401                                                                   |
| `auth.protocol_invalid`                                                                                                                                                                                                                                                                                          | 400                                                                   |
| `auth.registration_expired`                                                                                                                                                                                                                                                                                      | 410                                                                   |
| `auth.registration_conflict`                                                                                                                                                                                                                                                                                     | 409                                                                   |
| `auth.capacity_exceeded`                                                                                                                                                                                                                                                                                         | 429                                                                   |
| `deployment.listed`, `deployment.root_loaded`, `deployment.paused`, `deployment.moved`, `deployment.deleted`                                                                                                                                                                                                     | 200                                                                   |
| `deployment.created`                                                                                                                                                                                                                                                                                             | 201                                                                   |
| `deployment.queued`, `deployment.hard_redeploy_queued`, `deployment.target_changed`, `deployment.rollback_queued`, `deployment.resumed`                                                                                                                                                                          | 202                                                                   |
| `deployment.history_loaded`, `job.listed`, `project.listed`, `project.refs_listed`, `project.refreshed`, `project.key_assigned`, `project.cache_cleared`, `ssh_host.listed`, `ssh_host.approved`, `ssh_host.revoked`, `lfs.settings_loaded`, `lfs.settings_saved`, `webhook.settings_loaded`, `webhook.accepted` | 200                                                                   |
| `deployment.target_unchanged`, `deployment.already_resumed`, `project.has_deployments`, `repository_key.in_use`, `ssh_host.observation_changed`, `publication.path_conflict`, `request.conflict`                                                                                                                 | 409                                                                   |
| `project.created`, `repository_key.created`                                                                                                                                                                                                                                                                      | 201                                                                   |
| `project.deleted`, `repository_key.deleted`                                                                                                                                                                                                                                                                      | 200                                                                   |
| `git.target_unavailable`                                                                                                                                                                                                                                                                                         | 400 for invalid/unavailable selected targets during create, refresh, deploy, or target change; 409 for rollback with an unavailable branch; 502 for an unclassified remote refresh failure |
| `git.authentication_failed`                                                                                                                                                                                                                                                                                     | 502 for repository authentication failure during refresh               |
| `operation.upstream_failed`                                                                                                                                                                                                                                                                                      | 502 for a classified transient Git HTTP failure during refresh         |
| `ssh_host.approval_required`                                                                                                                                                                                                                                                                                     | 409 when a refresh first observes an unapproved host; 502 when a previously approved host becomes untrusted during refresh |
| `webhook.signature_invalid`                                                                                                                                                                                                                                                                                      | 401                                                                   |
| `webhook.not_configured`, `request.not_found`                                                                                                                                                                                                                                                                    | 404                                                                   |
| `webhook.secret_rotated`                                                                                                                                                                                                                                                                                         | 201                                                                   |
| `request.invalid`, `request.invalid_json`, `auth.protocol_invalid`                                                                                                                                                                                                                                               | 400                                                                   |
| `request.policy_rejected`                                                                                                                                                                                                                                                                                        | 403                                                                   |
| `request.json_required`                                                                                                                                                                                                                                                                                          | 415                                                                   |
| `request.too_large`                                                                                                                                                                                                                                                                                              | 413                                                                   |
| `request.method_not_allowed`                                                                                                                                                                                                                                                                                     | 405                                                                   |
| `request.https_required`                                                                                                                                                                                                                                                                                         | 403 for webhook ingestion; 426 for dashboard/authentication transport |
| `request.origin_rejected`                                                                                                                                                                                                                                                                                        | 403                                                                   |
| `request.host_rejected`                                                                                                                                                                                                                                                                                          | 421                                                                   |
| `request.rate_limited`                                                                                                                                                                                                                                                                                           | 403 or 429                                                            |
| `operation.capacity_exceeded`                                                                                                                                                                                                                                                                                    | 413 for repository cache/transfer capacity; 503 for unavailable work queue capacity |
| `operation.cleanup_pending`, `publication.recovery_required`, `project.cache_recovery_required`, `service.maintenance_required`                                                                                                                                                                                  | 503                                                                   |
| `service.unavailable`                                                                                                                                                                                                                                                                                            | 503                                                                   |
| `service.internal_error`                                                                                                                                                                                                                                                                                         | 500                                                                   |

### Authoritative success route table

This is the shipped method/status/code/message/data contract for every application
success route. `<provider>` is only `github` or `gitea`; “Existing” payloads are
described in the route sections below. `null` is JSON null, not an omitted field.

| Method and path | Status | Code | Message | `data` |
| --- | ---: | --- | --- | --- |
| POST `/opaque/register/start` | 200 | `auth.registration_started` | Registration started. | Existing value and transaction |
| POST `/opaque/register/finish` | 200 | `auth.registration_completed` | Registration completed. Sign in to continue. | `null` |
| POST `/opaque/login/start` | 200 | `auth.login_started` | Sign-in started. | Existing value and transaction |
| POST `/opaque/login/finish` | 200 | `auth.login_completed` | Signed in. | `null`; session cookie unchanged |
| POST `/opaque/password/change/start` | 200 | `auth.password_change_started` | Password change started. | Existing value |
| POST `/opaque/password/change/finish` | 200 | `auth.password_changed` | Password changed. Sign in again. | `null` |
| GET `/api/session` | 200 | `auth.session_valid` | Signed in. | `{"authenticated":true}` |
| POST `/api/logout` | 200 | `auth.logged_out` | Signed out. | `null` |
| GET `/api/projects` | 200 | `project.listed` | Projects loaded. | Project array |
| POST `/api/projects` | 201 | `project.created` | Project created. | Project |
| GET `/api/projects/{id}/refs` | 200 | `project.refs_listed` | Cached refs loaded. | Ref array |
| POST `/api/projects/{id}/refresh` | 200 | `project.refreshed` | Repository refreshed. | Existing `lastFetched` and `warning` |
| POST `/api/projects/{id}/key` | 200 | `project.key_assigned` | Repository key assigned. | `null` |
| POST `/api/projects/{id}/cache/recreate` | 200 | `project.cache_cleared` | Repository caches cleared. | `null` |
| POST `/api/projects/{id}/delete` | 200 | `project.deleted` | Project deleted. | `null` |
| GET `/api/projects/{id}/lfs` | 200 | `lfs.settings_loaded` | LFS settings loaded. | Existing settings and suggestion |
| POST `/api/projects/{id}/lfs` | 200 | `lfs.settings_saved` | LFS settings saved. | `null` |
| GET `/api/projects/{id}/<provider>-webhook` | 200 | `webhook.settings_loaded` | Webhook settings loaded. | Existing configured and URL fields |
| POST `/api/projects/{id}/<provider>-webhook` | 201 | `webhook.secret_rotated` | Webhook secret rotated. | Existing URL and secret; only here |
| GET `/api/deployments` | 200 | `deployment.listed` | Deployments loaded. | Deployment array |
| GET `/api/deployments/root` | 200 | `deployment.root_loaded` | Publication root loaded. | Existing `publicationRoot` |
| POST `/api/deployments` | 201 | `deployment.created` | Deployment created. Publication queued. | Existing id, jobId, status, publicationPath |
| POST `/api/deployments/{id}/deploy` | 202 | `deployment.queued` | Deployment queued. | Existing queued response |
| POST `/api/deployments/{id}/hard-redeploy` | 202 | `deployment.hard_redeploy_queued` | Hard re-deploy queued. | Existing queued response |
| POST `/api/deployments/{id}/target` | 202 | `deployment.target_changed` | Target changed. Publication queued. | Existing queued response |
| GET `/api/deployments/{id}/jobs` | 200 | `job.listed` | Jobs loaded. | Job array |
| GET `/api/deployments/{id}/history` | 200 | `deployment.history_loaded` | Commit history loaded. | Existing commits, page, hasMore |
| POST `/api/deployments/{id}/rollback` | 202 | `deployment.rollback_queued` | Rollback queued. | Existing queued response |
| POST `/api/deployments/{id}/pause` | 200 | `deployment.paused` | Automatic deployment paused. | Existing `automationPaused` |
| POST `/api/deployments/{id}/resume` | 202 | `deployment.resumed` | Automatic deployment resumed. Publication queued. | Existing automationPaused, jobId, status |
| POST `/api/deployments/{id}/move` | 200 | `deployment.moved` | Publication path changed. | Existing `publicationPath` |
| POST `/api/deployments/{id}/delete` | 200 | `deployment.deleted` | Deployment deleted. | `null` |
| GET `/api/repository-keys` | 200 | `repository_key.listed` | Repository keys loaded. | Key array with current `projectCount` on every key; never private material |
| POST `/api/repository-keys` | 201 | `repository_key.created` | Repository key created. | Key; never private material |
| POST `/api/repository-keys/{id}/delete` | 200 | `repository_key.deleted` | Repository key deleted. | `null` |
| GET `/api/ssh-hosts` | 200 | `ssh_host.listed` | SSH hosts loaded. | Host array |
| POST `/api/ssh-hosts/approve` | 200 | `ssh_host.approved` | SSH host approved. | `null` |
| POST `/api/ssh-hosts/revoke` | 200 | `ssh_host.revoked` | SSH host trust revoked. | `null` |
| POST `/webhooks/<provider>/{id}` | 200 | `webhook.accepted` | Webhook accepted. | `null` |

Acknowledgement intentionally does not distinguish duplicate, ignored, and
actionable webhook deliveries; it confirms durable handling, not publication.
Cache clearing does not claim that a fresh clone was created. A successful
WebSocket handshake is HTTP 101, not a JSON success; its first frame is
`events.ready`.

### Actionable failure status differences

Errors use the same envelope and ordinarily carry `data: null`. Status depends
on route and failure stage as follows; entries with alternatives are intentional.

| Code | Status | Route-specific distinction / safe meaning |
| --- | --- | --- |
| `request.invalid_json` | 400 | Malformed JSON where decoding is distinguished from field validation. |
| `request.invalid` | 400 | Invalid fields/confirmation; shared decoder may also classify oversized JSON this way. |
| `request.policy_rejected` | 403 | Policy or confirmation rejected, including unsafe internal LFS destinations. |
| `request.origin_rejected` | 403 | Request Origin is not allowed. |
| `request.not_found` | 404 | Resource or application route not found. |
| `request.method_not_allowed` | 405 | Unsupported method; preserves `Allow`. |
| `request.too_large` | 413 or existing 400 | Explicit body bounds return 413; shared decoder can retain 400. |
| `request.json_required` | 415 | JSON media type is required. |
| `request.host_rejected` | 421 | Configured application Host mismatch. |
| `request.https_required` | 426 or 403 | Dashboard/auth transport returns 426; webhook/protocol policy may return 403. |
| `request.rate_limited` | 429 or 403 | Dashboard mutation limiter is 429; OPAQUE protocol/logout limiter is 403. |
| `auth.session_required` | 401 | Missing, expired, or revoked session; do not claim which. |
| `auth.login_failed` | 401 | Safe sign-in failure; no account/credential distinction. |
| `auth.protocol_invalid` | 400 | Invalid OPAQUE exchange or request. |
| `auth.registration_expired` | 410 | Registration grant/transaction unavailable; start again. |
| `auth.registration_conflict` | 409 | Registration is no longer available. |
| `auth.recent_login_required` | 401 | Re-authenticate before password change; not global session loss. |
| `auth.capacity_exceeded` | 429 or 503 | Authentication/session capacity, route-specific status. |
| `request.conflict` | 409 | State changed; refresh before retrying. |
| `deployment.target_unchanged` | 409 | Selected target is unchanged. |
| `deployment.already_resumed` | 409 | Automatic deployment is already enabled. |
| `publication.path_conflict` | 409 | Destination occupied or expected path stale. |
| `project.has_deployments` | 409 | Delete dependent deployments first. |
| `repository_key.in_use` | 409 | Key is assigned to a project. |
| `ssh_host.approval_required` | 409 or 502 | First unapproved host is 409; formerly trusted host becoming untrusted during refresh is 502. |
| `ssh_host.observation_changed` | 409 | Host observation differs from reviewed details. |
| `git.target_unavailable` | 400, 409, or 502 | Invalid target on create/refresh/deploy/target change is 400; unavailable rollback branch 409; unclassified remote refresh failure may remain 502. |
| `git.authentication_failed` | 502 | Repository authentication failed. |
| `operation.upstream_failed` | Existing upstream status | Classified transient Git HTTP refresh failure; message remains safe. |
| `webhook.signature_invalid` | 401 | Webhook authentication failed. |
| `webhook.not_configured` | 404 | Webhook is not configured. |
| `operation.capacity_exceeded` | 413 or 503 | Repository cache/transfer capacity is 413; unavailable work queue is 503. |
| `operation.cleanup_pending` | 503 | Accepted deletion; `data` is `{"operationAccepted":true,"recoveryRequired":true}`. |
| `publication.recovery_required` | 503 | Inspect/reconcile state; do not automatically repeat. |
| `project.cache_recovery_required` | 503 | Repository cache recovery is required. |
| `service.maintenance_required` | 503 | Static maintenance result, independent of storage. |
| `service.unavailable` | 503 | Service is temporarily unavailable. |
| `service.internal_error` | 500 | Operation could not be completed; diagnostics are withheld. |

Recovery-required results report accepted work only when acceptance is known.
Otherwise `data` is `null` and callers must read state. Never infer “nothing
changed” from non-2xx. Messages do not contain raw upstream, filesystem, parser,
credential, or key diagnostics. SSH handshake library errors remain untyped at
the adapter boundary, but are narrowed to a fixed safe HTTP outcome; the cause
is never serialized.

`project.deleted` and `webhook.accepted` are also used for durable completion
and acknowledgement respectively; `operation.cleanup_pending` means deletion
was accepted even though cleanup remains. Server errors use
`service.internal_error` without exposing storage, parser, or upstream
diagnostics. The browser SDK additionally defines local-only `sdk.network`,
`sdk.invalid_response`, and `sdk.cancelled`; these are not server codes.

`/api/`, `/opaque/`, and `/webhooks/` responses have `Cache-Control: no-store`.
Unknown backend paths never fall back to the SPA. Frontend shell fallback is
restricted to recognized GET/HEAD navigations accepting HTML; missing assets and
backend errors keep their real error statuses. A maintenance-locked daemon uses
[`MaintenanceHandler`](../lib/maintenance.go) instead of application routing:
**503** `{"code":"service.maintenance_required","message":"Service unavailable. Maintenance is required.","data":null}` for all paths, without loading the SPA or accessing app data. HEAD has no body.

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

| Method and path                       | Request JSON                                                             | Success                                                                                                   |
| ------------------------------------- | ------------------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------- |
| POST `/opaque/register/start`         | `{"grant":"<setup-or-recovery-token>","value":"<registration-request>"}` | 200 `auth.registration_started`; `data` is `{"value":"<registration-response>","transaction":"<handle>"}` |
| POST `/opaque/register/finish`        | `{"transaction":"<handle>","value":"<registration-record>"}`             | 200 `auth.registration_completed`; `data: null`; creates/replaces credentials, does not sign in           |
| POST `/opaque/login/start`            | `{"value":"<KE1>"}`                                                      | 200 `auth.login_started`; `data` is `{"transaction":"<handle>","value":"<KE2>"}`                          |
| POST `/opaque/login/finish`           | `{"transaction":"<handle>","value":"<KE3>"}`                             | 200 `auth.login_completed`; `data: null` plus session cookie after KE3 verification                       |
| GET `/api/session`                    | None                                                                     | 200 `auth.session_valid`; `data: {"authenticated":true}`                                                  |
| POST `/api/logout`                    | No body required                                                         | 200 `auth.logged_out`; `data: null`; invalidates current session if present and expires cookie            |
| POST `/opaque/password/change/start`  | `{"value":"<new-password-registration-request>"}`                        | 200 `auth.password_change_started`; `data` is `{"value":"<registration-response>"}`                       |
| POST `/opaque/password/change/finish` | `{"value":"<new-password-registration-record>"}`                         | 200 `auth.password_changed`; `data: null`; replaces credentials and revokes sessions                      |

For example, success is `{"code":"auth.login_completed","message":"Signed in.","data":null}`. Errors use the same shape. Authentication codes include `auth.session_required` (401), `auth.login_failed` (401), `auth.protocol_invalid` (400), `auth.registration_expired` (410), `auth.registration_conflict` (409), `auth.recent_login_required` (401), and `auth.capacity_exceeded` (existing 429). Shared request and service codes are `request.https_required`, `request.json_required`, `request.policy_rejected`, `request.origin_rejected`, `request.rate_limited`, `service.unavailable`, and `service.internal_error`, retaining their existing statuses. Authentication errors do not disclose account existence, password validity, or grant-consumption details.

### Browser SDK (authentication, project, repository, and deployment operations)

Authentication, project, repository, and deployment callers use the
framework-independent [`web/src/lib/sdk/index.ts`](../web/src/lib/sdk/index.ts)
barrel. It owns
same-origin credentials, JSON encoding, envelope and endpoint-data validation,
safe fallback errors, cancellation, and session-required signaling. Named
operations such as `sdk.auth.loginStart`, `sdk.auth.session`,
`sdk.auth.recoveryFinish`, and `sdk.auth.passwordChangeFinish` accept an optional
`AbortSignal`.
Project operations are grouped under `sdk.projects`: `list`, `create`,
`refs(id)`, `refresh(id)`, `assignKey(id, { keyId })`, and `clearCache(id)`.
They return the validated outcome envelope; callers read resource payloads from
`data` and may pass an `AbortSignal`. Project paths and full IDs are handled by
the SDK. Deployment operations such as `sdk.deployments.list`,
`sdk.deployments.deploy`, and `sdk.deployments.jobs` also accept an optional
`AbortSignal`. Mutations, including OPAQUE exchanges, are never retried
automatically. SDK errors are
discriminated as `server`, `network`, `invalid_response`, or `cancelled`;
proxy HTML and raw exception text are not displayed as trusted messages.
Only server outcomes carry server-returned codes. SDK-local failures carry
`sdk.network`, `sdk.invalid_response`, or `sdk.cancelled`, respectively; these
codes are client-generated and must not be interpreted as server outcomes.
Unknown nonempty server codes may be surfaced with valid envelope/data, but do
not match known-code behavior. Only `auth.session_required` from a protected
operation triggers session-loss handling; `auth.login_failed` and
`auth.recent_login_required` do not.

OPAQUE remains a dynamic import on sign-in, registration/recovery, and password
change flows. Do not import its crypto package from the SDK or ordinary
dashboard modules. Authentication flows continue to remove setup/recovery
grants from the URL before submission and never send them in URLs.

Repository management operations are defined in
[`web/src/lib/sdk/repository.ts`](../web/src/lib/sdk/repository.ts) and wired
through the same barrel: `sdk.repositoryKeys.list/create/delete`,
`sdk.sshHosts.list/approve/revoke`, `sdk.lfs.settings/save`, and
`sdk.webhooks.settings/rotate`. These methods validate endpoint data, encode
resource IDs, accept an optional `AbortSignal`, and never retry mutations.
Webhook rotation returns its printable secret only in that authorized response;
the settings read and live notifications never include it.
Project and repository routes use named SDK operations and outcome envelopes,
including `sdk.projects.delete`. Lifecycle deployment actions use
`sdk.deployments.history`, `rollback`, `pause`, `resume`, `move`, and `delete`.
These operations accept an optional `AbortSignal`; cancellation does not cancel
durably accepted server work and mutations are never automatically retried.

### Live deployment updates

`GET /api/events` is a read-only native WebSocket endpoint. The browser uses
the existing HttpOnly session cookie; query credentials and bearer
subprotocols are not accepted. Before upgrade, Go requires the exact configured
Origin, canonical Host, an authorized session, and the configured HTTPS policy.
Cleartext `ws:` is accepted only for the explicitly enabled canonical HTTP
origin. Failed handshakes return the normal JSON outcome envelope; browser code
uses `GET /api/session` to distinguish session loss from a generic socket
failure.

The server sends a ready frame after registering the socket:

```json
{
  "code": "events.ready",
  "message": "Live updates connected.",
  "data": { "epoch": "<daemon-id>", "sequence": 0 }
}
```

Committed deployment/job changes send invalidation frames, never resource
bodies or logs:

```json
{
  "code": "events.invalidate",
  "message": "Dashboard data changed.",
  "data": {
    "epoch": "<daemon-id>",
    "sequence": 1,
    "resources": [
      { "kind": "jobs", "deploymentId": "<id>" },
      { "kind": "deployments", "deploymentId": "<id>" }
    ]
  }
}
```

The daemon-wide sequence increases per committed batch. It is diagnostic only;
the random epoch changes on restart and there is no replay. Successful
application POSTs emit invalidations only after their handler has returned a
successful response; worker notifications follow committed state transitions.
Invalidations cover projects/refs/history, deployments/jobs, repository keys,
SSH hosts, LFS settings, and webhook settings, including dependent project and
deployment reads. They contain metadata only; reads never emit events.

The singleton SDK owns connection state, resource subscriptions, HTTP refreshes,
and disposal. It refreshes all active reads on ready/reconnect, epoch change,
sequence gap, unknown code, or malformed frame, and retains notifications that
arrive during an in-flight read. Reconnect uses jittered exponential delays from
1 to 30 seconds, reset only after 30 healthy seconds. If no usable socket is
available after 10 seconds, visible active reads refresh every 15 seconds while
reconnecting; fallback stops on ready. Hidden tabs suspend fallback, and
foreground return refreshes active reads once. Offline status suppresses
reconnect attempts until the browser returns online. Failed handshakes probe
`GET /api/session`; only a verified 401 or socket close 4401 ends the session.
Session loss clears protected resource values and stops sockets and timers.
Network failure alone is not treated as logout. Each authenticated layout must
dispose its subscriptions on teardown. Healthy connected dashboards do not
poll.

The endpoint limits connection attempts to 20 per minute per peer IP, active
sockets to 8 per session and 64 per daemon, outbound queues to 64 frames, text
frames to 16 KiB, and client messages to 1 KiB (all client application
messages are rejected). It checks session validity at least every 5 seconds,
uses 25-second protocol pings and bounded writes, closes slow clients rather
than dropping invalidations, revokes sockets on logout/password replacement,
and closes them on shutdown. Deployment/job notifications follow committed
queue, worker, and terminal state updates; clients always fetch authoritative
HTTP state, including sanitized job logs. The SDK resynchronizes active views
on each ready frame, ignores duplicate/older invalidations, and closes malformed
streams for bounded-backoff reconnect. Reconnect delay is jittered, capped at
30 seconds, and resets after 30 seconds of a healthy connection.
Job invalidations are coalesced per deployment to at most four batches per
second, with a pending terminal refresh flushed after the coalescing interval.
Verified webhook queue commits invalidate the affected deployment/job views.

Vite forwards `/api` WebSocket upgrades to the Go backend without rewriting the
browser Origin or Host. Production reverse proxies must use HTTP/1.1 upstream,
forward `Upgrade` and `Connection: upgrade`, preserve the browser `Origin` and
canonical `Host`, and set their WebSocket idle/read timeout longer than the
25-second ping interval (60 seconds or more is recommended). Do not weaken
canonical Host, trusted-proxy, or Origin validation to accommodate a proxy.

For ordinary HTTP traffic, proxy `/api/`, `/opaque/`, and `/webhooks/` to the
same Go origin without path rewriting or caching; preserve `Host` and browser
`Origin`, and provide `X-Forwarded-Proto: https` only from a trusted TLS
terminator. Permit the configured webhook and OPAQUE request body limits.
Forward WebSocket upgrades on `/api/events` as above. Do not expose an alternate
origin that bypasses canonical Host, Origin, transport, or session checks.

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

| Method and path                          | Request / success                                                                                 | Important behavior                                                                                                                                                 |
| ---------------------------------------- | ------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| GET `/api/projects`                      | 200 `project.listed`; `data` is Project[]                                                         | Ordered by creation time then ID; measures Git/LFS cache sizes. Optional `keyId=<32-lowercase-hex-id>` filters server-side to current assignments; valid unknown/unused keys return an empty array. |
| POST `/api/projects`                     | `{"name":"site","url":"https://git.example/site.git"}`; 201 `project.created`; `data` is Project  | Synchronously clones without checkout and saves cached refs; not a queued job.                                                                                     |
| GET `/api/projects/{id}/refs`            | 200 `project.refs_listed`; `data` is Ref[]                                                        | Cached only, sorted by kind/name; does not fetch.                                                                                                                  |
| POST `/api/projects/{id}/refresh`        | No body required; 200 `project.refreshed`; `data` is `{"lastFetched":"<timestamp>","warning":""}` | Synchronously fetches; failed refresh preserves cached refs and last successful fetch information.                                                                 |
| POST `/api/projects/{id}/key`            | `{"keyId":"<key-id>"}`; 200 `project.key_assigned`; `data` is null                                | SSH projects only; changes assigned managed key, does not fetch.                                                                                                   |
| POST `/api/projects/{id}/cache/recreate` | `{"confirm":true}`; 200 `project.cache_cleared`; `data` is null                                   | Clears Git and verified LFS caches; this does not clone or fetch a replacement repository. Recovery-required errors mean inspect/restart recovery before retrying. |
| POST `/api/projects/{id}/delete`         | `{"confirm":true}`; 200 `project.deleted`; `data: null`                                           | Blocked with `project.has_deployments` / 409; accepted cache cleanup failures return `operation.cleanup_pending`.                                                  |

Project failure outcomes use `request.invalid` (400), `request.not_found` (404),
`git.target_unavailable` (400/502), `git.authentication_failed` (502),
`ssh_host.approval_required` (409/502), `operation.capacity_exceeded` (413 or
the existing upstream failure status), `operation.upstream_failed` (existing
upstream status), and `project.cache_recovery_required` (503) where applicable.
Failures do not echo upstream Git text in the outcome message. A failed repository
refresh preserves the last successful cached refs and fetch time; inspect its
warning before retrying. Cache recovery-required means do not assume retry safety.

`GET /api/projects?keyId=<id>` applies the optional repository-key filter before
loading project details. The ID must be a 32-character lowercase hexadecimal
resource ID; malformed or repeated `keyId` parameters return `request.invalid`
(400). A valid key with no current project assignments and a well-formed unknown
key both return `project.listed` with `data: []`. Omitting `keyId` preserves the
unfiltered listing and its creation-time/ID ordering. This remains the ordinary
session-authorized project-list route; no key-specific endpoint is introduced.

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

| Method and path                            | Request / success                                                                                                     | Important behavior                                                                                                                                            |
| ------------------------------------------ | --------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| GET `/api/deployments`                     | 200 `deployment.listed`; `data: Deployment[]`                                                                         | All deployments; no project filter or pagination.                                                                                                             |
| GET `/api/deployments/root`                | 200 `deployment.root_loaded`; `data: {"publicationRoot":"<server-path>"}`                                             | Configured publication root for folder selection.                                                                                                             |
| POST `/api/deployments`                    | Create body below; 201 `deployment.created`; `data` contains `id`, `jobId`, `status: "queued"`, and `publicationPath` | Saves deployment and automatically queues initial publication.                                                                                                |
| POST `/api/deployments/{id}/deploy`        | No body required; 202 `deployment.queued`; `data: Queued response`                                                    | Ordinary Deploy; branch fetches latest, tag/commit uses saved commit.                                                                                         |
| POST `/api/deployments/{id}/hard-redeploy` | No body required; 202 `deployment.hard_redeploy_queued`; `data: Queued response`                                      | Fetches; resolves branch/tag again; forces recreation even if already live. Commit targets stay fixed.                                                        |
| POST `/api/deployments/{id}/target`        | Target body below; 202 `deployment.target_changed`; `data: Queued response`                                           | Changes configuration and queues publication; unchanged target returns `deployment.target_unchanged` / 409.                                                   |
| GET `/api/deployments/{id}/jobs`           | 200 `job.listed`; `data: Job[]`                                                                                       | Newest first, at most 100; no pagination. Unknown deployment IDs currently yield an empty array.                                                              |
| GET `/api/deployments/{id}/history`        | Query below; 200 `deployment.history_loaded`; `data` contains commits/page/hasMore                                    | Branch only; cached first-parent history, no fetch.                                                                                                           |
| POST `/api/deployments/{id}/rollback`      | `{"commit":"<full-sha1>","permanent":false}`; 202 `deployment.rollback_queued`; `data: Queued response`               | Branch only; ancestry validation happens in the worker after fetching.                                                                                        |
| POST `/api/deployments/{id}/pause`         | No body required; 200 `deployment.paused`; `data: {"automationPaused":true}`                                          | Branch only; repeated pause succeeds.                                                                                                                         |
| POST `/api/deployments/{id}/resume`        | No body required; 202 `deployment.resumed`; `data` contains `automationPaused`, `jobId`, and `status: "queued"`       | Branch only; immediately queues latest tip; already enabled returns 409.                                                                                      |
| POST `/api/deployments/{id}/delete`        | `{"confirm":true}`; 200 `deployment.deleted`; `data: null`                                                            | Supersedes work and removes published files; blocked cleanup returns 503 `operation.cleanup_pending` with `operationAccepted: true` after durable acceptance. |
| POST `/api/deployments/{id}/move`          | Move body below; 200 `deployment.moved`; `data: {"publicationPath":"<absolute-path>"}`                                | Requires confirmation and expected current publication path; durable atomic move/reconciliation protections apply.                                            |
| POST `/api/projects/{id}/delete`           | `{"confirm":true}`; 200 `project.deleted`; `data: null`                                                               | Blocked with `project.has_deployments`; accepted cache cleanup pending uses `operation.cleanup_pending`.                                                      |

Deployment outcomes use the common `code`, `message`, and `data` envelope.
Lifecycle error codes distinguish ordinary path conflicts from recovery-required
move outcomes and accepted cleanup-pending deletion. Never infer that nothing
changed from a non-2xx response.

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

Move body (4 KiB limit; confirmation required):

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

The queued outcome confirms durable acceptance, not successful publication.
Read deployments and jobs through `sdk.deployments.list` and
`sdk.deployments.jobs`; WebSocket invalidations refresh active HTTP reads, with
visibility-aware fallback polling only while the socket is unavailable. There
is no job-by-ID route. Jobs use `queued` → `running` and terminal `succeeded`, `noop`,
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

| Method and path                         | Request / success                                                                                                                                                          |
| --------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| GET `/api/repository-keys`              | 200 `repository_key.listed`; `data: Key[]`                                                                                                                                 |
| POST `/api/repository-keys`             | `{"name":"reader","generate":true}` **or** `{"name":"reader","privateKey":"<unprotected-key-text>"}`; 201 `repository_key.created`; `data: Key`                            |
| POST `/api/repository-keys/{id}/delete` | No body required; 200 `repository_key.deleted`; 409 `repository_key.in_use` if assigned                                                                                    |
| GET `/api/ssh-hosts`                    | 200 `ssh_host.listed`; `data: Host[]`                                                                                                                                      |
| POST `/api/ssh-hosts/approve`           | `{"host":"git.example","port":22,"fingerprint":"SHA256:<observed>","confirm":true}`; 200 `ssh_host.approved`; stale observations return 409 `ssh_host.observation_changed` |
| POST `/api/ssh-hosts/revoke`            | `{"host":"git.example","port":22,"fingerprint":"SHA256:<observed>","confirm":true}`; 200 `ssh_host.revoked`; stale observations return 409 `ssh_host.observation_changed`  |

**Key:** `id`, `name`, `algorithm`, `bits`, `fingerprint`, `publicKey`,
`createdAt`, `projectCount`. Every key includes an integer `projectCount`,
including zero for unused keys. It counts current project–key assignments only;
deployments and historical assignments do not affect it. It is refreshed from
current assignments on each list request. Private keys are never returned.
Generation and import are mutually exclusive;
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
fingerprint. It clears that host's trusted key; stale or already-untrusted
entries return **409**. The host remains listed with a warning.
Subsequent SSH connections are blocked until the observed fingerprint is
independently verified and approved again.

## LFS destination settings

Sources: [`lfs_settings.go`](../lib/lfs_settings.go),
[`lfs_transport.go`](../lib/lfs_transport.go).

| Method and path               | Request / success                                                                                                                             |
| ----------------------------- | --------------------------------------------------------------------------------------------------------------------------------------------- |
| GET `/api/projects/{id}/lfs`  | 200 `lfs.settings_loaded`; `data: {"settings":{"endpoint":"","objectOrigins":[],"allowInternal":false},"suggestedEndpoint":"<url-or-empty>"}` |
| POST `/api/projects/{id}/lfs` | Body below (16 KiB limit); 200 `lfs.settings_saved`; `data: null`                                                                             |

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

| Method and path                              | Request / success                                                                                                                |
| -------------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------- |
| GET `/api/projects/{id}/{provider}-webhook`  | 200 `webhook.settings_loaded`; `data: {"configured":true,"url":"/webhooks/<provider>/<project-id>"}` (`configured` may be false) |
| POST `/api/projects/{id}/{provider}-webhook` | `{"rotate":true}` (1 KiB limit); 201 `webhook.secret_rotated`; data contains URL and new secret                                  |

The URL is relative to the application origin. Rotation also initializes an
unconfigured webhook. Each project/provider has an independent encrypted secret.
The new printable secret is returned only at rotation; GET never reveals it.
Providers must sign using the **literal returned secret string**, not bytes
obtained by hex-decoding it. Update provider configuration after rotation.

### Provider ingestion (signature, no dashboard session/Origin)

`POST /webhooks/github/{id}` and `POST /webhooks/gitea/{id}` return **200**
`webhook.accepted` with `data: null` after durable acceptance, including
duplicate/ignored deliveries. This acknowledges handling, not publication.
HTTPS is required even in explicit HTTP-origin dashboard mode.

| Provider | Delivery ID         | Event            | HMAC-SHA256 signature over exact raw body     |
| -------- | ------------------- | ---------------- | --------------------------------------------- |
| GitHub   | `X-GitHub-Delivery` | `X-GitHub-Event` | `X-Hub-Signature-256: sha256=<64-hex-digits>` |
| Gitea    | `X-Gitea-Delivery`  | `X-Gitea-Event`  | `X-Gitea-Signature: <64-hex-digits>`          |

Body size is bounded by `WebhookMaxBodyBytes`. Invalid headers/push payloads
return **400**, bad signatures `webhook.signature_invalid` (**401**),
unconfigured webhooks `webhook.not_configured` (**404**), oversized bodies
**413**, and queue/durable-acceptance failure **503**.

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

The authenticated route assertions are intentionally spread over real fixtures:
`outcome_contract_test.go` checks routing/security envelopes,
`projects_smart_test.go` and `projects_test.go` cover project lifecycle,
`automation_test.go`, `rollback_app_test.go`, and `tags_app_test.go` cover
deployment/job/history actions, `repository_keys_test.go` and
`ssh_projects_test.go` cover keys and trust, `lfs_test.go` covers LFS settings,
`webhooks_test.go` / `gitea_webhooks_test.go` cover both provider contracts, and
`registration_transaction_test.go` covers OPAQUE lifecycle. Keep concrete
status/code/data assertions in these tests; this reference is not a substitute.

| Area                                      | Tests                                                                                                                                                                                                      |
| ----------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| Registration, login, credential lifecycle | [`bootstrap_test.go`](../lib/bootstrap_test.go), [`registration_transaction_test.go`](../lib/registration_transaction_test.go)                                                                             |
| Routing/embedding/backend errors          | [`assets_test.go`](../lib/assets_test.go)                                                                                                                                                                  |
| Projects, cache, SSH                      | [`projects_test.go`](../lib/projects_test.go), [`projects_smart_test.go`](../lib/projects_smart_test.go), [`ssh_projects_test.go`](../lib/ssh_projects_test.go)                                            |
| Deploy/jobs/tag refresh                   | [`deployments_test.go`](../lib/deployments_test.go), [`tags_app_test.go`](../lib/tags_app_test.go)                                                                                                         |
| Rollback/pause/restart                    | [`rollback_app_test.go`](../lib/rollback_app_test.go), [`rollback_restart_test.go`](../lib/rollback_restart_test.go), [`automation_test.go`](../lib/automation_test.go)                                    |
| Publication path/move/delete              | [`publication_paths_test.go`](../lib/publication_paths_test.go), [`publication_move_test.go`](../lib/publication_move_test.go), [`deletion_test.go`](../lib/deletion_test.go)                              |
| Keys/LFS/webhooks                         | [`repository_keys_test.go`](../lib/repository_keys_test.go), [`lfs_test.go`](../lib/lfs_test.go), [`webhooks_test.go`](../lib/webhooks_test.go), [`gitea_webhooks_test.go`](../lib/gitea_webhooks_test.go) |

Combined-binary browser coverage and its build prerequisites are described in
[`tests/README.md`](../tests/README.md). For storage/process recovery rather than
HTTP contracts, use [process-coordination.md](process-coordination.md) and the
[operations guides](operations/).
