package evidence

import "github.com/danielgtaylor/huma/v2"

func (Segment) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	text := *s
	gap := *s
	text.Properties = map[string]*huma.Schema{}
	gap.Properties = map[string]*huma.Schema{}
	for k, v := range s.Properties {
		if k != "gap" {
			text.Properties[k] = v
		}
		if k != "text" {
			gap.Properties[k] = v
		}
	}
	text.Required = []string{"sequence", "byte_start", "byte_end", "text"}
	gap.Required = []string{"sequence", "byte_start", "byte_end", "gap"}
	return &huma.Schema{OneOf: []*huma.Schema{&text, &gap}}
}
