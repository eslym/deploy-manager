# Static site deployment manager — agreed design

Design confirmed by the user on 2026-11-12; frontend architecture corrected by the maintainer on 2026-11-13 to use the existing `web/` SPA embedded in Go. This records agreed requirements, not authorization to implement.

## Scope

- Go application with a web administration dashboard and SQLite storage.
- Use stable go-git v5 rather than external Git or SSH executables, within the compatibility scope below.
- One trusted administrator with authentication; no teams or roles.
- Administrator registration and password login must use OPAQUE as specified by RFC 9807, without transmitting the password to the server. This means the OPAQUE augmented PAKE protocol, not merely opaque session tokens. Browser/Go implementation compatibility and security maturity require research before selecting dependencies. HTTPS remains required through the reverse proxy.
- Publish static Git content to a manager-owned local filesystem root. HTTP hosting, builds, remote transfers, content editing, shells, and web-server configuration are out of scope.

## Git and publication

- Each project represents one repository, initially cloned without a checkout; multiple deployments may share that repository.
- Each deployment has a persistent name, Git target, and stable publication path.
- Branch targets automatically follow matching verified push webhooks. Support Gitea and GitHub. Branch deletion leaves the live site unchanged.
- Commit targets remain fixed. Tag targets resolve once and remain fixed until explicitly refreshed.
- Hard re-deploy fetches, re-resolves branch/tag targets, and recreates publication even if its commit has not changed. Commit targets remain fixed.
- Publish the complete tracked repository root, including dotfiles, excluding Git metadata. Reject symlinks and submodules.
- Prepare content separately and switch publication atomically. Failures leave the current publication untouched.
- Do not retain previous release directories for rollback. Keep replaced directories only for a short configurable reader grace period before cleanup. Recreating an old version requires its Git objects and any LFS payloads to remain available.
- Serialize publication per deployment, coalesce pending branch updates to the latest state, and bound concurrency across deployments.

## Credentials and dashboard

- Generate or import repository SSH keys, display public keys, and assign a key to a project. Do not display or export private keys.
- Encrypt private keys at rest with a master key held outside the database. Imported keys need not support passphrases in v1.
- Store and manage SSH host trust in SQLite rather than requiring a host known_hosts file. Surface host verification failures as dashboard warnings and block connections pending approval after independent verification.
- Verify webhook requests with separate per-project secrets.
- Dashboard scope: project, deployment, and key management; webhook setup; deploy and hard-redeploy actions; job status and logs.
- Target selection lists the project's branches and tags rather than requiring manually typed names. Rollback selection lists eligible commits with abbreviated IDs and short commit messages.
- Branch/tag selection is searchable and reads cached refs, showing the last successful fetch time and an explicit Refresh action. Failed refresh preserves the cached list with a warning.
- Rollback browsing shows first-parent history only, with pagination, search, abbreviated commit ID, subject, author, and date. Preserve full commit IDs for actions and revalidate ancestry after fetching. This presentation restriction does not change the agreed branch-ancestor eligibility rule.

## Confirmed lifecycle and operations

- Run at most one main daemon per installation. Compatible local CLI utilities, including setup and password-recovery grant issuance, may run alongside that daemon; they must not start daemon workers or perform cache/publication recovery. Before accessing application data, CLI commands must respect maintenance state and verify that the database schema exactly matches the version supported by their executable. A mismatch fails nonzero without mutation, migration, downgrade, or repair. Fresh initialization and schema maintenance require separate cross-process exclusion from all database users; the daemon's lifetime singleton lock must not exclude compatible CLI commands. Maintenance-locked commands still refuse, and offline database restoration requires stopping the daemon and all CLI users. This maintainer correction (2026-11-15, #27) supersedes any earlier all-command singleton proposal.
- Ordinary Deploy fetches and resolves branch targets, but uses the saved commit for tag/commit targets. If the target commit is already live, record a no-op rather than recreating files.
- Creating a deployment or changing its Git target automatically queues publication.
- Deployment deletion requires confirmation and removes its published files. Project deletion is blocked while deployments exist; repository keys cannot be deleted while assigned.
- Persist jobs and status in SQLite. Mark interrupted jobs failed after restart and preserve existing publications. Persist accepted webhook work before acknowledging delivery. Retry transient fetch failures only, with bounded attempts; do not automatically retry authentication or host-trust failures.
- Unknown or changed SSH host keys block Git connections and deployment. Show dashboard warnings with the observed fingerprint; administrator approval follows independent fingerprint verification. Trust is scoped to hostname and port.
- Bootstrap the single administrator account through browser registration using a one-time token; no public signup or default password. Local CLI commands issue setup and recovery tokens as described below.
- Repository keys may be shared across projects; select one key per SSH project. HTTPS repositories are anonymous/public only in v1.
- A job whose deployment was deleted or whose target configuration changed must not publish stale content; mark it superseded.
- Local CLI commands issue short-lived, single-use registration and recovery tokens, store only token hashes, and print the relevant URL. Disable registration after bootstrap; do not put tokens in routine logs.
- Deduplicate webhooks by project, provider, and delivery ID. Branch jobs fetch the latest branch tip rather than using the event commit as a publication command.
- Enforce configurable timeouts, webhook-body limits, export file-count/byte limits, and bounded concurrency. These limits are not an administrator sandbox.
- Retain job metadata and sanitized logs for a configurable period, defaulting to 30 days. Redact secrets and credential-bearing URLs.

## Confirmed Git compatibility and filesystem constraints

- Use stable go-git v5 for conventional SHA-1 repositories over SSH or smart HTTPS. SHA-256 Git repositories, local/file URLs, dumb HTTP, and partial clones are out of scope.
- Export tracked commit trees into isolated workspaces instead of creating native Git worktrees. No Git hooks or arbitrary checkout filters run.
- Git LFS downloads are required through an additional in-process client, within the authentication, endpoint, and transfer scope below.
- Publication paths must be real directories, not symlinks. Require the Linux atomic-exchange capabilities described below.
- Retain fetched Git objects without automatic garbage collection. Show cache size and offer explicit cache recreation with a warning about loss of unreachable commits.
- Target Linux only for v1.
- Use the existing Bun-managed SvelteKit frontend in `web/` as a client-rendered single-page application (SPA), with its production output embedded in the Go executable. See the frontend delivery requirements below; no separate frontend server or runtime Node.js/Bun installation.
- Expose dashboard endpoints and GitHub/Gitea webhook ingestion without a separately versioned public API or API tokens in v1.

## Frontend delivery and Go integration

- `web/` is the application frontend source of truth for registration, login, recovery, password changes, and the authenticated dashboard. Adapt the existing scaffold rather than introducing a parallel Go-template or hand-written JavaScript UI. This supersedes the earlier server-rendered-dashboard requirement.
- Build a fully client-rendered SPA with SvelteKit's static adapter and SSR disabled at the root layout. All application business logic, persistence, authorization, and protocol endpoints run in Go; do not depend on SvelteKit server routes, server load functions, form actions, or a production JavaScript server.
- The production build workflow installs frontend dependencies from `web/bun.lock`, builds `web/` before compiling Go, and embeds the generated HTML, JavaScript, CSS, WASM, and other required static assets using `go:embed`. Missing frontend output must fail the build rather than silently ship a placeholder or legacy UI. Document the combined build command and use it for release and browser-test artifacts. Bun/Node.js are build/development tools only; the distributed binary must run without the source tree or external asset directories.
- Go serves embedded assets and the SPA shell on the same origin as its private dashboard/authentication HTTP endpoints. These internal endpoints are required for the SPA and are not a separately versioned public API or API-token product. Development may use Vite, but must preserve the production cookie, Origin, and CSRF security model.
- Direct navigation and browser refresh on frontend routes, including `/register` and nested dashboard routes, must load the SPA. Restrict shell fallback to frontend GET/HEAD navigations; backend paths (including authentication protocol and webhook endpoints), missing assets, and unsupported methods retain their actual error/status semantics rather than returning HTML with success. Serve assets with correct MIME types; use revalidation for the shell and immutable caching only for content-hashed assets.
- The shell and static assets contain no credentials or protected data. Client-side route guards are presentation only: Go must enforce sessions and authorization on every protected read and mutation, including direct HTTP requests. Preserve HTTPS, cookie, Origin/CSRF, rate-limit, and security-header protections through the migration.
- Load the pinned OPAQUE browser module/WASM lazily only for authentication flows, including recent-authentication/password-change flows. Ordinary dashboard navigation must not import, preload, or initialize crypto through a shared bundle. Keep all assets first-party and embedded; initialization failure remains actionable and fail-closed, without password-post fallback. Preserve fragment-only setup/recovery grants: remove them from the visible URL before protected submission and never include them in resource URLs or routine logs.
- Validate the actual combined production binary from an unrelated working directory with no frontend runtime or source assets available. Browser/HTTP tests must exercise SPA registration, direct-route reloads, missing-asset/backend errors, authentication-only crypto loading, and server-enforced protected access as those features land. Frontend checks and a standalone Vite preview alone do not demonstrate embedding.
- Implementation status: the incorrect Go HTML/standalone-JavaScript bootstrap implementation was removed from history. Ticket #3 owns implementing bootstrap in `web/` and establishing the combined embedded build; #4 extends it to login and the protected dashboard. Bootstrap must include the required security protections and application/browser-test coverage. #25 and #26 remain required upgrade/distribution follow-ups, including assets bundled from `web/`.

## Confirmed publication and LFS refinements

- Require atomic directory exchange on a supported local Linux filesystem, with staging and live on the same mount. Probe capabilities and fail closed without a non-atomic fallback. Use atomic rename for first publication.
- Use durable publication intent records, filesystem syncing, and restart reconciliation of actual directory identity before updating state or cleaning up. Ambiguous state blocks further publication and raises an alert; never blindly repeat an exchange.
- A short configurable reader grace period precedes old-directory cleanup. This is not rollback retention and does not guarantee browser/request-level release consistency.
- LFS selection follows committed Git attributes rather than automatically hydrating every pointer blob, using the scope and matching ownership below.
- Require administrator-approved LFS endpoint and object-storage origins, verified HTTPS, no redirects, and explicit approval of internal destinations. Do not silently follow repository endpoint overrides; scope credentials to intended endpoints.
- Pin exported low-level git-lfs/git-lfs utility imports behind application-owned adapters. Accept upstream's unsupported external API and larger dependency graph; own attribute resolution and transport rather than adopting the full client stack. Retain applicable licenses and test attribute conformance. All local external Git/SSH/LFS executables remain forbidden.
- Default reader grace period: 60 seconds, configurable; cleanup failures remain visible and cleanup resumes after restart.
- Evaluate only .gitattributes files tracked in the selected commit, including nested files and Git precedence. Ignore host-global attributes and repository-local info/attributes.
- For LFS-selected files, fail the whole release on malformed pointers, missing payloads, unsupported required extensions, or size/hash mismatches. Preserve ordinary inline non-pointer content; never publish unresolved LFS pointers or partially hydrated output.

## Branch rollback and paused automation

- Branch-target deployments support publication of a selected commit without changing their configured branch.
- Temporary rollback leaves automation enabled; the next matching webhook or explicit deployment may publish the latest branch tip. Do not immediately catch up solely because rollback completed.
- Permanent rollback persists a per-deployment automation pause across restarts. Matching webhooks fetch repository updates only, without export, LFS hydration, or publication for that deployment. Other deployments in the project remain independent.
- Manual Deploy while paused publishes the latest branch tip without clearing the pause. Resume explicitly clears the pause and immediately queues deployment of the latest branch tip.
- Rollback supersedes older queued/in-flight publications. Permanent rollback establishes the pause when requested, even if rollback later fails; failure preserves existing live files and leaves the pause in place with a visible error.
- Rollback recreates a commit, not a retained release directory; success depends on availability of both Git objects and required LFS payloads.
- Restrict rollback to ancestors of the configured branch. Administrators can explicitly change the deployment target when a commit outside that ancestry is required.
- Offer standalone Pause and Resume actions, independent of rollback.
- Editing a paused deployment to another branch preserves the pause but queues the agreed one-off publication for target changes. Switching to a tag/commit target removes the branch-only pause state.

- Fetch successfully before validating rollback ancestry against the configured branch tip. If a newer local fetch shows the commit is no longer an ancestor before publication, invalidate the rollback. Missing branches and failed fetches fail rollback without changing live files; permanent rollback's pause remains set.

## LFS transport and cache

- Support basic HTTPS LFS downloads for anonymous/public HTTPS repositories and private SSH repositories whose server offers remote git-lfs-authenticate. Invoke that remote service through in-process Go SSH with the managed repository key and SQLite host verification.
- Pure-SSH git-lfs-transfer and private HTTPS Git/LFS credentials are out of scope for v1. Missing server capabilities fail clearly; no external credential helpers or automatic insecure fallback.
- Reuse pinned exported low-level pointer/parser/pattern utilities behind adapters, while owning effective attribute resolution, endpoint policy, HTTP downloads, resource limits, and exact payload size/SHA-256 validation.
- Keep a per-project verified LFS payload cache keyed by SHA-256 for re-deploys and rollback; no automatic eviction initially. Include it in cache-size reporting and explicit cache-recreation warnings. Cache recreation may lose payloads no longer available from the server.

## Research and confirmation

- Supporting findings: [go-git compatibility](research/go-git-compatibility.md), [LFS and directory publication](research/lfs-and-directory-publication.md), [Git LFS utility reuse](research/git-lfs-library-reuse.md), and [OPAQUE authentication](research/opaque-authentication.md).
- Requirements above incorporate subsequent decisions where research notes present alternatives.
- Shared understanding is confirmed. Implementation and the isolated OPAQUE feasibility gate require separate authorization.

## OPAQUE authentication and account lifecycle

Dependency selection approved by the maintainer on 2026-11-13: Bytemare Go v0.18.0 and the tested Serenity browser package v1.1.0, with the [gate findings](research/opaque-gate-findings.md) and [explicitly accepted risks](research/opaque-accepted-risks.md). The experiment code was removed at the maintainer's request; historical evidence remains linked from the findings. This approval does not waive license/notice obligations before distribution, the required upgrade when the frontend memory-leak fix is published, or production authentication/lifecycle security requirements below.

- RFC 9807 is mandatory for registration and password login. Do not substitute a draft protocol or transmit passwords as a fallback.
- Before dependency approval, run an isolated feasibility gate: verify exact Go/browser artifacts and security advisories, run RFC vectors and browser-to-Go registration/login tests, and measure browser resource costs. Bytemare Go with Serenity browser WASM is a candidate, not an approved or proven pairing. Unresolved security issues block shipping; unaudited implementation risk requires explicit acceptance or independent review.
- Require JavaScript/WASM for authentication. Embed pinned first-party crypto assets in the executable and load them on authentication pages, not ordinary dashboard pages. Initialization failure produces a useful error, not a password-post fallback. HTTPS is mandatory.
- Only after successful verification of the final OPAQUE login message may the server issue a separate random session token. Use server-side sessions and Secure, HttpOnly, SameSite cookies; apply CSRF/Origin protection, rate limits, and expiring one-use handshake state. OPAQUE secret outputs are not bearer session tokens.
- Password changes require recent successful OPAQUE authentication and fresh browser-side OPAQUE registration. CLI-issued, expiring, single-use recovery links authorize replacement of the existing administrator's record using the same registration protocol; they do not reveal the old password or create another account. Atomically replace credentials, revoke sessions and pending logins, and require login again.
- Keep long-term OPAQUE server secrets in a dedicated externally managed key file, separate from SQLite registration records and the repository-key master key. Preserve matching key/record/profile generations in backups. Missing long-term authentication keys fail closed; never silently regenerate them. Password changes and recovery must not change the independent repository-key encryption master key.
