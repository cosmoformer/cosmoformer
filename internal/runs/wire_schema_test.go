package runs

import (
	"encoding/json"
	"github.com/danielgtaylor/huma/v2"
	"testing"
)

func TestPermissionSchemaMatchesFlattenedWireResponse(t *testing.T) {
	registry := huma.NewMapRegistry("#/components/schemas/", huma.DefaultSchemaNamer)
	schema := (Transition{}).TransformSchema(registry, &huma.Schema{Properties: map[string]*huma.Schema{}})
	for _, key := range []string{"permission_id", "ordinal", "command_spec_digest", "platform_instance", "remaining_ms", "display_expires_at"} {
		if schema.Properties[key] == nil {
			t.Fatal("missing wire permission field", key)
		}
	}
	if schema.Properties["permission"] != nil {
		t.Fatal("schema advertises a nested permission")
	}
	assignment := (Assignment{}).TransformSchema(registry, &huma.Schema{Properties: map[string]*huma.Schema{}})
	if len(assignment.Properties["execution_spec"].OneOf) != 2 {
		t.Fatal("preflight and execution schemas were collapsed")
	}
}
func TestAssignmentRejectsAmbiguousOrInvalidExecutionSpecs(t *testing.T) {
	for _, raw := range []string{`{`, `{"phase":1}`, `{"phase":"execution","unknown":true}`, `{"phase":"execution","execution_spec":{"unknown":true}}`, `{"phase":"execution","execution_spec":{}}`} {
		var a Assignment
		if json.Unmarshal([]byte(raw), &a) == nil {
			t.Fatal("invalid assignment accepted", raw)
		}
	}
}
