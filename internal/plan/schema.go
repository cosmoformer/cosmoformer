package plan

import (
	"github.com/danielgtaylor/huma/v2"
)

// The browser receives the same lossless, recursive tagged union that is
// serialized by Value. In particular no generic JSON number alternative exists.
func (Value) TransformSchema(r huma.Registry, s *huma.Schema) *huma.Schema {
	ref := &huma.Schema{Ref: "#/components/schemas/Value"}
	literal := func(v string) *huma.Schema { return &huma.Schema{Type: "string", Enum: []any{v}} }
	alternatives := []*huma.Schema{}
	for _, state := range []string{"hidden", "unknown", "null", "absent"} {
		alternatives = append(alternatives, &huma.Schema{Type: "object", Properties: map[string]*huma.Schema{"state": literal(state)}, Required: []string{"state"}, AdditionalProperties: false})
	}
	for _, typ := range []string{"string", "number", "boolean", "object", "array"} {
		v := &huma.Schema{Type: "string"}
		switch typ {
		case "number":
			v.Pattern = `^-?(0|[1-9][0-9]*)(\.[0-9]+)?$`
		case "boolean":
			v.Type = "boolean"
		case "object":
			v.Type = "object"
			v.AdditionalProperties = ref
		case "array":
			v.Type = "array"
			v.Items = ref
		}
		alternatives = append(alternatives, &huma.Schema{Type: "object", Properties: map[string]*huma.Schema{"state": literal("known"), "type": literal(typ), "value": v}, Required: []string{"state", "type", "value"}, AdditionalProperties: false})
	}
	return &huma.Schema{OneOf: alternatives}
}
func pathSchema() *huma.Schema {
	return &huma.Schema{Type: "array", Items: &huma.Schema{OneOf: []*huma.Schema{{Type: "string"}, {Type: "integer", Minimum: ptr(0), Maximum: ptr(9007199254740991)}}}}
}
func ptr(v float64) *float64 { return &v }
func (PathChange) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	s.Properties["path"] = pathSchema()
	return s
}
func (Finding) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	s.Properties["path"] = pathSchema()
	return s
}
func (Resource) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	s.Properties["replacement_paths"] = &huma.Schema{Type: "array", Items: pathSchema()}
	return s
}
