package schema

// This file describes the rest of goose v1's physical tables. It is a query
// model only: runtime migrations are deliberately disabled and goose remains
// the sole schema executor. For legacy composite-key tables, the first key is
// exposed as the Ent ID and the full database uniqueness is retained by Index.

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
)

func table(name string) []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: name}}
}

type RepositoryKeyStore struct{ ent.Schema }

func (RepositoryKeyStore) Annotations() []schema.Annotation { return table("repository_key_store") }
func (RepositoryKeyStore) Fields() []ent.Field {
	return []ent.Field{field.Int("id").StorageKey("singleton"), field.Bytes("verifier")}
}

type RepositoryKey struct{ ent.Schema }

func (RepositoryKey) Annotations() []schema.Annotation { return table("repository_keys") }
func (RepositoryKey) Fields() []ent.Field {
	return []ent.Field{field.String("id"), field.String("name"), field.String("algorithm"), field.Int("bits"), field.String("fingerprint"), field.String("public_key"), field.Bytes("ciphertext"), field.Int64("created_at")}
}

type Project struct{ ent.Schema }

func (Project) Annotations() []schema.Annotation { return table("projects") }
func (Project) Fields() []ent.Field {
	return []ent.Field{field.String("id"), field.String("name"), field.String("remote_url"), field.Int64("created_at"), field.Int64("last_fetched").Optional().Nillable(), field.String("warning").Default("")}
}
func (Project) Edges() []ent.Edge {
	return []ent.Edge{edge.To("deployments", Deployment.Type)}
}

type ProjectRef struct{ ent.Schema }

func (ProjectRef) Annotations() []schema.Annotation { return table("project_refs") }
func (ProjectRef) Fields() []ent.Field {
	return []ent.Field{field.String("id").StorageKey("project_id"), field.String("kind"), field.String("name"), field.String("commit_id")}
}
func (ProjectRef) Indexes() []ent.Index {
	return []ent.Index{index.Fields("id", "kind", "name").Unique()}
}

type ProjectRefreshJournal struct{ ent.Schema }

func (ProjectRefreshJournal) Annotations() []schema.Annotation {
	return table("project_refresh_journal")
}
func (ProjectRefreshJournal) Fields() []ent.Field {
	return []ent.Field{field.String("id").StorageKey("project_id")}
}

type ProjectSSHKey struct{ ent.Schema }

func (ProjectSSHKey) Annotations() []schema.Annotation { return table("project_ssh_keys") }
func (ProjectSSHKey) Fields() []ent.Field {
	return []ent.Field{field.String("id").StorageKey("project_id"), field.String("key_id")}
}

type ProjectDeletion struct{ ent.Schema }

func (ProjectDeletion) Annotations() []schema.Annotation { return table("project_deletions") }
func (ProjectDeletion) Fields() []ent.Field {
	return []ent.Field{field.String("id").StorageKey("project_id"), field.Int64("cache_dev"), field.Int64("cache_ino"), field.Int64("lfs_dev").Default(0), field.Int64("lfs_ino").Default(0), field.String("status"), field.String("last_error").Default(""), field.Int64("created_at"), field.Int64("updated_at"), field.Int("attempts").Default(0), field.Int64("retry_after").Default(0)}
}

type SSHHostTrust struct{ ent.Schema }

func (SSHHostTrust) Annotations() []schema.Annotation { return table("ssh_host_trust") }
func (SSHHostTrust) Fields() []ent.Field {
	return []ent.Field{field.String("id").StorageKey("host"), field.Int("port"), field.Bytes("trusted_key").Optional(), field.Bytes("observed_key").Optional(), field.String("observed_fingerprint").Default(""), field.String("warning").Default("")}
}
func (SSHHostTrust) Indexes() []ent.Index { return []ent.Index{index.Fields("id", "port").Unique()} }

type ProjectLFSSetting struct{ ent.Schema }

func (ProjectLFSSetting) Annotations() []schema.Annotation { return table("project_lfs_settings") }
func (ProjectLFSSetting) Fields() []ent.Field {
	return []ent.Field{field.String("id").StorageKey("project_id"), field.String("endpoint"), field.String("object_origins"), field.Int("allow_internal").Default(0), field.String("internal_addresses").Default("[]"), field.Int64("updated_at")}
}

type Deployment struct{ ent.Schema }

func (Deployment) Annotations() []schema.Annotation { return table("deployments") }
func (Deployment) Fields() []ent.Field {
	return []ent.Field{field.String("id"), field.String("project_id"), field.String("name"), field.String("commit_id"), field.String("publication_name"), field.Int64("generation").Default(1), field.String("live_commit").Default(""), field.Int64("live_dev").Default(0), field.Int64("live_ino").Default(0), field.String("status").Default("queued"), field.String("error").Default(""), field.Int64("created_at"), field.Int64("updated_at"), field.String("target_kind").Default("commit"), field.String("target_name").Default(""), field.Int("automation_paused").Default(0), field.Int64("automation_epoch").Default(0)}
}
func (Deployment) Indexes() []ent.Index {
	return []ent.Index{index.Fields("project_id", "name").Unique(), index.Fields("publication_name").Unique()}
}
func (Deployment) Edges() []ent.Edge {
	return []ent.Edge{
		edge.From("project", Project.Type).Ref("deployments").Unique().Field("project_id").Required().Annotations(entsql.OnDelete(entsql.Restrict)),
		edge.To("jobs", DeploymentJob.Type),
	}
}

type DeploymentJob struct{ ent.Schema }

func (DeploymentJob) Annotations() []schema.Annotation { return table("deployment_jobs") }
func (DeploymentJob) Fields() []ent.Field {
	return []ent.Field{field.String("id"), field.String("deployment_id"), field.String("commit_id"), field.Int64("generation"), field.String("status"), field.String("log").Default(""), field.Int64("created_at"), field.Int64("updated_at"), field.Int("force_redeploy").Default(0), field.String("rollback_commit").Default(""), field.Int("durable").Default(0), field.Int("retry_enqueued").Default(0), field.Int("automatic").Default(0), field.Int64("automation_epoch").Default(0)}
}
func (DeploymentJob) Edges() []ent.Edge {
	return []ent.Edge{edge.From("deployment", Deployment.Type).Ref("jobs").Unique().Field("deployment_id").Required().Annotations(entsql.OnDelete(entsql.Cascade))}
}

type PublicationIntent struct{ ent.Schema }

func (PublicationIntent) Annotations() []schema.Annotation { return table("publication_intents") }
func (PublicationIntent) Fields() []ent.Field {
	return []ent.Field{field.String("id").StorageKey("deployment_id"), field.String("job_id"), field.String("stage_name"), field.Int("had_old"), field.Int64("old_dev"), field.Int64("old_ino"), field.Int64("new_dev"), field.Int64("new_ino"), field.String("phase"), field.Int64("created_at")}
}

type PublicationCleanup struct{ ent.Schema }

func (PublicationCleanup) Annotations() []schema.Annotation { return table("publication_cleanup") }
func (PublicationCleanup) Fields() []ent.Field {
	return []ent.Field{field.String("id").StorageKey("deployment_id"), field.String("job_id"), field.String("path_name"), field.Int64("dev"), field.Int64("ino"), field.Int64("not_before"), field.String("last_error").Default(""), field.Int("attempts").Default(0)}
}
func (PublicationCleanup) Indexes() []ent.Index {
	return []ent.Index{index.Fields("id", "job_id").Unique()}
}

type DeploymentRemovalPath struct{ ent.Schema }

func (DeploymentRemovalPath) Annotations() []schema.Annotation {
	return table("deployment_removal_paths")
}
func (DeploymentRemovalPath) Fields() []ent.Field {
	return []ent.Field{field.String("id").StorageKey("deployment_id"), field.String("path_name"), field.Int64("dev"), field.Int64("ino"), field.String("last_error").Default(""), field.Int("attempts").Default(0)}
}
func (DeploymentRemovalPath) Indexes() []ent.Index {
	return []ent.Index{index.Fields("id", "path_name").Unique()}
}

type DeploymentDeletion struct{ ent.Schema }

func (DeploymentDeletion) Annotations() []schema.Annotation { return table("deployment_deletions") }
func (DeploymentDeletion) Fields() []ent.Field {
	return []ent.Field{field.String("id").StorageKey("deployment_id"), field.String("status"), field.String("last_error").Default(""), field.Int64("created_at"), field.Int64("updated_at"), field.Int64("retry_after").Default(0)}
}

type ArchivedDeploymentJob struct{ ent.Schema }

func (ArchivedDeploymentJob) Annotations() []schema.Annotation {
	return table("archived_deployment_jobs")
}
func (ArchivedDeploymentJob) Fields() []ent.Field {
	return []ent.Field{field.String("id"), field.String("deployment_id"), field.String("commit_id"), field.Int64("generation"), field.String("status"), field.String("log"), field.Int64("created_at"), field.Int64("updated_at")}
}

type ProjectWebhook struct{ ent.Schema }

func (ProjectWebhook) Annotations() []schema.Annotation { return table("project_webhooks") }
func (ProjectWebhook) Fields() []ent.Field {
	return []ent.Field{field.String("id").StorageKey("project_id"), field.Bytes("secret"), field.Int64("created_at")}
}

type ProjectGiteaWebhook struct{ ent.Schema }

func (ProjectGiteaWebhook) Annotations() []schema.Annotation { return table("project_gitea_webhooks") }
func (ProjectGiteaWebhook) Fields() []ent.Field {
	return []ent.Field{field.String("id").StorageKey("project_id"), field.Bytes("secret"), field.Int64("created_at")}
}

type WebhookDelivery struct{ ent.Schema }

func (WebhookDelivery) Annotations() []schema.Annotation { return table("webhook_deliveries") }
func (WebhookDelivery) Fields() []ent.Field {
	return []ent.Field{field.String("id").StorageKey("project_id"), field.String("provider"), field.String("delivery_id"), field.String("event"), field.String("outcome"), field.Int64("created_at")}
}
func (WebhookDelivery) Indexes() []ent.Index {
	return []ent.Index{index.Fields("id", "provider", "delivery_id").Unique()}
}
