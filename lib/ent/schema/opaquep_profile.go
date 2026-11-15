package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
)

// OpaqueProfile binds the external server-key generation to the protocol profile.
type OpaqueProfile struct{ ent.Schema }

func (OpaqueProfile) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "opaque_profile", Check: "singleton=1"}}
}
func (OpaqueProfile) Fields() []ent.Field {
	noIncrement := false
	return []ent.Field{
		field.Int("id").StorageKey("singleton").Annotations(entsql.Annotation{Incremental: &noIncrement}),
		field.Int("profile_generation"),
		field.String("key_generation"),
		field.String("profile"),
	}
}
