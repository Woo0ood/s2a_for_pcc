package schema

import (
	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema"
	"entgo.io/ent/schema/field"
	"entgo.io/ent/schema/index"
	"github.com/Wei-Shaw/sub2api/ent/schema/mixins"
)

type PayloadAuditLog struct{ ent.Schema }

func (PayloadAuditLog) Annotations() []schema.Annotation {
	return []schema.Annotation{entsql.Annotation{Table: "payload_audit_logs"}}
}

func (PayloadAuditLog) Mixin() []ent.Mixin { return []ent.Mixin{mixins.TimeMixin{}} }

func (PayloadAuditLog) Fields() []ent.Field {
	return []ent.Field{
		field.String("request_id").MaxLen(64).Default(""),
		field.Int64("user_id").Optional().Nillable(),
		field.String("user_email").MaxLen(255).Default(""),
		field.Int64("api_key_id").Optional().Nillable(),
		field.String("api_key_name").MaxLen(100).Default(""),
		field.Int64("group_id").Optional().Nillable(),
		field.String("group_name").MaxLen(255).Default(""),
		field.String("client_ip").MaxLen(45).Default(""),
		field.String("endpoint").MaxLen(128).Default(""),
		field.String("provider").MaxLen(64).Default(""),
		field.String("model").MaxLen(255).Default(""),
		field.String("upstream_model").MaxLen(255).Default(""),
		field.Bool("stream").Default(false),
		field.Int("status_code").Default(0),
		field.Int("duration_ms").Default(0),
		field.String("input_excerpt").MaxLen(2048).Default(""),
		field.String("output_excerpt").MaxLen(2048).Default(""),
		field.Text("input_body").Default(""),
		field.Text("output_body").Default(""),
		field.String("input_format").MaxLen(16).Default("json"),
		field.String("output_format").MaxLen(16).Default("text"),
		field.Int("input_bytes").Default(0),
		field.Int("output_bytes").Default(0),
		field.Bool("input_truncated").Default(false),
		field.Bool("output_truncated").Default(false),
		field.Bool("output_omitted").Default(false),
		field.Text("error_message").Default(""),
	}
}

func (PayloadAuditLog) Indexes() []ent.Index {
	return []ent.Index{
		index.Fields("created_at", "id"),
		index.Fields("user_id", "created_at"),
		index.Fields("group_id", "created_at"),
		index.Fields("api_key_id", "created_at"),
		index.Fields("request_id"),
	}
}
