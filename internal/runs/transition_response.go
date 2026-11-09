package runs

import (
	"encoding/json"
	"github.com/danielgtaylor/huma/v2"
	"reflect"
)

func (t Transition) MarshalJSON() ([]byte, error) {
	type base Transition
	b, e := json.Marshal(base(t))
	if e != nil {
		return nil, e
	}
	if t.Permission == nil {
		return b, nil
	}
	var fields map[string]json.RawMessage
	if e = json.Unmarshal(b, &fields); e != nil {
		return nil, e
	}
	p, e := json.Marshal(t.Permission)
	if e != nil {
		return nil, e
	}
	var permit map[string]json.RawMessage
	if e = json.Unmarshal(p, &permit); e != nil {
		return nil, e
	}
	for k, v := range permit {
		fields[k] = v
	}
	return json.Marshal(fields)
}
func (t *Transition) UnmarshalJSON(b []byte) error {
	type base Transition
	var fields map[string]json.RawMessage
	if e := json.Unmarshal(b, &fields); e != nil {
		return e
	}
	permit := map[string]json.RawMessage{}
	for _, k := range []string{"permission_id", "ordinal", "command_spec_digest", "platform_instance", "remaining_ms", "display_expires_at"} {
		if v, ok := fields[k]; ok {
			permit[k] = v
			delete(fields, k)
		}
	}
	delete(fields, "$schema")
	raw, e := json.Marshal(fields)
	if e != nil {
		return e
	}
	if e = Decode(raw, (*base)(t)); e != nil {
		return e
	}
	t.Permission = nil
	if len(permit) > 0 {
		if len(permit) != 6 {
			return Fail("INVALID_REQUEST")
		}
		raw, e = json.Marshal(permit)
		if e != nil {
			return e
		}
		t.Permission = &Permission{}
		return Decode(raw, t.Permission)
	}
	return nil
}
func (Transition) TransformSchema(r huma.Registry, s *huma.Schema) *huma.Schema {
	p := huma.SchemaFromType(r, reflect.TypeFor[Permission]())
	for k, v := range p.Properties {
		s.Properties[k] = v
	}
	return s
}
