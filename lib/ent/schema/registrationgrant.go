package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
)

// RegistrationGrant maps one-use setup and recovery grants. token_hash is the
// physical primary key; no bearer token is persisted.
type RegistrationGrant struct{ ent.Schema }

func (RegistrationGrant) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "registration_grants"}}
}
func (RegistrationGrant) Fields() []ent.Field {
	return []ent.Field{
		field.Bytes("id").StorageKey("token_hash"),
		field.Int64("expires_at"),
		field.Int64("created_at"),
		field.String("purpose").Default("setup"),
		field.Int64("credential_generation").Default(0),
	}
}
