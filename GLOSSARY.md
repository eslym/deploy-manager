# Static Site Deployment Management

Management of Git-backed static content publication. Building content and serving HTTP traffic are outside this domain.

## Language

**Project**:
A Git repository whose static content can be published through one or more deployments.

**Deployment**:
A persistent, named publication destination within a project, with a selected Git target.
_Avoid_: Release, build

**Release**:
A successfully published version of a deployment, identified by its resolved Git commit.
_Avoid_: Deployment, build

**Branch target**:
A deployment target that follows a named Git branch and receives automatic updates from verified webhooks.
_Avoid_: Immutable pin

**Commit target**:
A deployment target fixed to a specific Git commit.

**Tag target**:
A deployment target selected by a Git tag and frozen to its resolved commit unless explicitly refreshed.

**Repository key**:
An SSH credential assigned to a project for reading its Git repository, not for accessing deployment servers.
_Avoid_: Deployment-server key

**Hard re-deploy**:
An explicit publication that refreshes the configured Git target and recreates its published content even when the resolved commit is unchanged. Branches and tags are resolved again; a commit target remains fixed.

**Publication path**:
The stable local destination through which a deployment's current release is made available to an external web server.

**Temporary rollback**:
A publication of a selected commit for a branch-target deployment without pausing automatic deployment. A later matching webhook or explicit deployment may replace it with the branch tip.

**Permanent rollback**:
A publication of a selected commit for a branch-target deployment that also persistently pauses automatic deployment while preserving the configured branch.
_Avoid_: Commit target

**Automatic deployment paused**:
A persistent branch-deployment state in which matching webhooks fetch repository updates but do not prepare or publish content. Manual publication remains available.

**Resume**:
An explicit action that enables automatic deployment for a paused branch deployment and immediately requests publication of the latest branch tip.
