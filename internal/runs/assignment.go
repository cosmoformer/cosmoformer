package runs

import (
	"encoding/json"
	"github.com/danielgtaylor/huma/v2"
	"reflect"
)

// Assignment keeps the two closed spec schemas separate on the wire.
func (a Assignment) MarshalJSON() ([]byte, error) {
	type alias Assignment
	if a.Execution == nil {
		return json.Marshal(alias(a))
	}
	b, e := json.Marshal(alias(a))
	if e != nil {
		return nil, e
	}
	var fields map[string]json.RawMessage
	if e = json.Unmarshal(b, &fields); e != nil {
		return nil, e
	}
	fields["execution_spec"], e = json.Marshal(a.Execution)
	if e != nil {
		return nil, e
	}
	return json.Marshal(fields)
}
func (a *Assignment) UnmarshalJSON(b []byte) error {
	type alias Assignment
	var fields map[string]json.RawMessage
	if e := json.Unmarshal(b, &fields); e != nil {
		return e
	}
	var phase string
	if e := json.Unmarshal(fields["phase"], &phase); e != nil {
		return e
	}
	raw := fields["execution_spec"]
	if phase == "execution" {
		fields["execution_spec"] = json.RawMessage(`{}`)
	}
	rest, e := json.Marshal(fields)
	if e != nil {
		return e
	}
	if e = Decode(rest, (*alias)(a)); e != nil {
		return e
	}
	if phase == "execution" {
		a.Execution = &ExecutionSpec{}
		if e = Decode(raw, a.Execution); e != nil {
			return e
		}
		if e = a.Execution.Validate(); e != nil {
			return e
		}
		a.Spec = a.Execution.Preflight()
	}
	return nil
}
func (Assignment) TransformSchema(r huma.Registry, s *huma.Schema) *huma.Schema {
	s.Properties["execution_spec"] = &huma.Schema{OneOf: []*huma.Schema{r.Schema(reflect.TypeFor[PreflightSpec](), true, "PreflightSpec"), r.Schema(reflect.TypeFor[ExecutionSpec](), true, "ExecutionSpec")}}
	return s
}
