package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
)

// Administrator maps the singleton RFC 9807 registration record. It does not
// model application users or roles; the physical id=1 constraint is retained.
type Administrator struct{ ent.Schema }

func (Administrator) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "administrator", Check: "id=1"}}
}
func (Administrator) Fields() []ent.Field {
	noIncrement := false
	return []ent.Field{
		field.Int("id").Positive().Annotations(entsql.Annotation{Incremental: &noIncrement}),
		field.Bytes("registration_record"),
		field.Int("profile_generation"),
		field.Int64("credential_generation").Default(1),
		field.Int64("created_at"),
	}
}
