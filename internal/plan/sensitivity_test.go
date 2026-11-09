package plan_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/plan"
	"github.com/cosmoformer/cosmoformer/internal/runs"
)

// Exercise the public sanitizer with schema-sensitive values omitted by native
// markers and the known-value registry, including preventing-control cases.
func TestMapBlockSchemaSensitivity(t *testing.T) {
	const provider = "registry.opentofu.org/example/nested"
	const addr = "nested_item.fixture"
	const old = "audit-private-before-6381"
	const next = "audit-private-after-9472"
	cases := []struct {
		name, mode, alias                         string
		attr, beforeMarker, afterMarker, register bool
		wrongArray, overlap                       bool
	}{
		{name: "map_block_empty_markers", mode: "map"},
		{name: "format_metadata_case_alias", mode: "map", alias: "format"},
		{name: "provider_map_case_alias", mode: "map", alias: "providers"},
		{name: "provider_case_alias", mode: "map", alias: "provider"},
		{name: "resource_map_case_alias", mode: "map", alias: "resources"},
		{name: "schema_item_case_alias", mode: "map", alias: "item"},
		{name: "attributes_metadata_case_alias", mode: "map", alias: "attributes"},
		{name: "blocks_metadata_case_alias", mode: "map", alias: "blocks"},
		{name: "block_nesting_case_alias", mode: "map", alias: "block_mode"},
		{name: "attribute_type_case_alias", mode: "map", alias: "type"},
		{name: "sensitive_metadata_case_alias", mode: "map", alias: "sensitive"},
		{name: "sensitive_metadata_unicode_case_alias", mode: "map", alias: "unicode"},
		{name: "nested_attribute_case_alias", mode: "map", attr: true, alias: "nested_mode"},
		{name: "attribute_block_name_overlap", mode: "single", overlap: true},
		{name: "map_block_rejects_array", mode: "map", wrongArray: true},
		{name: "map_attribute_rejects_array", mode: "map", attr: true, wrongArray: true},
		{name: "map_attribute_empty_markers", mode: "map", attr: true},
		{name: "list_block_empty_markers", mode: "list"},
		{name: "single_block_empty_markers", mode: "single"},
		{name: "group_block_empty_markers", mode: "group"},
		{name: "set_block_empty_markers", mode: "set"},
		{name: "map_block_before_native_marker", mode: "map", beforeMarker: true},
		{name: "map_block_after_native_marker", mode: "map", afterMarker: true},
		{name: "map_block_before_registered", mode: "map", register: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Disable schema sensitivity in native-marker/registry controls so each
			// preventing layer must work independently.
			sensitive := !tc.beforeMarker && !tc.afterMarker && !tc.register && !tc.overlap
			attrs := map[string]any{"private": map[string]any{"type": "string", "sensitive": sensitive}, "safe": map[string]any{"type": "string"}, "Private": map[string]any{"type": "string"}}
			block := map[string]any{}
			if tc.attr {
				block["attributes"] = map[string]any{"items": map[string]any{"optional": true, "nested_type": map[string]any{"nesting_mode": tc.mode, "attributes": attrs}}}
			} else {
				block["block_types"] = map[string]any{"items": map[string]any{"nesting_mode": tc.mode, "block": map[string]any{"attributes": attrs}}}
			}
			if tc.overlap {
				block["attributes"] = map[string]any{"items": map[string]any{
					"type":      []any{"object", map[string]any{"private": "string", "safe": "string", "Private": "string"}},
					"sensitive": true,
				}}
			}
			resourceSchema := map[string]any{"version": 0, "block": block}
			providerSchema := map[string]any{"provider": map[string]any{"version": 0, "block": map[string]any{}}, "resource_schemas": map[string]any{"nested_item": resourceSchema}}
			schemaInput := map[string]any{"format_version": "1.0", "provider_schemas": map[string]any{provider: providerSchema}}
			switch tc.alias {
			case "format":
				schemaInput["FORMAT_VERSION"] = "1.0"
			case "providers":
				schemaInput["PROVIDER_SCHEMAS"] = schemaInput["provider_schemas"]
			case "provider":
				providerSchema["PROVIDER"] = providerSchema["provider"]
			case "resources":
				providerSchema["RESOURCE_SCHEMAS"] = providerSchema["resource_schemas"]
			case "item":
				resourceSchema["BLOCK"] = block
			case "attributes":
				block["ATTRIBUTES"] = map[string]any{"extra": map[string]any{"type": "string"}}
			case "blocks":
				block["BLOCK_TYPES"] = block["block_types"]
			case "block_mode":
				block["block_types"].(map[string]any)["items"].(map[string]any)["NESTING_MODE"] = "map"
			case "type":
				attrs["private"].(map[string]any)["TYPE"] = "string"
			case "sensitive":
				attrs["private"].(map[string]any)["SENSITIVE"] = false
			case "unicode":
				attrs["private"].(map[string]any)["ſensitive"] = false
			case "nested_mode":
				block["attributes"].(map[string]any)["items"].(map[string]any)["nested_type"].(map[string]any)["NESTING_MODE"] = "map"
			}
			schema, err := json.Marshal(schemaInput)
			if err != nil {
				t.Fatal("schema encoding")
			}
			wrap := func(value any) any {
				if tc.wrongArray {
					return []any{value}
				}
				switch tc.mode {
				case "map":
					return map[string]any{"slot": value, "Slot": value, "other": value}
				case "list", "set":
					return []any{value}
				default:
					return value
				}
			}
			before := map[string]any{"items": wrap(map[string]any{"private": old, "safe": "visible", "Private": "case-visible"})}
			after := map[string]any{"items": wrap(map[string]any{"private": next, "safe": "visible", "Private": "case-visible"})}
			bs, as := map[string]any{}, map[string]any{}
			if tc.beforeMarker {
				bs["items"] = wrap(map[string]any{"private": true})
			}
			if tc.afterMarker {
				as["items"] = wrap(map[string]any{"private": true})
			}
			raw, err := json.Marshal(map[string]any{"format_version": "1.2", "terraform_version": "1.13.8", "planned_values": map[string]any{}, "configuration": map[string]any{}, "resource_changes": []any{map[string]any{"address": addr, "name": "fixture", "type": "nested_item", "mode": "managed", "provider_name": provider, "change": map[string]any{"actions": []string{"update"}, "before": before, "after": after, "after_unknown": map[string]any{}, "before_sensitive": bs, "after_sensitive": as}}}})
			if err != nil {
				t.Fatal("plan encoding")
			}
			reg := &evidence.Registry{}
			if tc.register {
				if reg.Register(old) != nil {
					t.Fatal("registration")
				}
			}
			out, err := plan.Analyze(raw, schema, plan.Options{Engine: "opentofu", Version: "1.13.8", ProfileID: plan.ProfileID("opentofu"), Providers: []runs.Provider{{Address: provider, Version: "1.0.0", Platform: "linux_amd64", Hashes: []string{"h1:fixture"}}}, ResourceIDs: map[string]string{addr: "11111111-1111-4111-8111-111111111111"}, Secrets: reg})
			if tc.wrongArray || tc.overlap || tc.alias != "" {
				if err == nil || out != nil {
					t.Fatal("ambiguous schema or mismatched native collection accepted")
				}
				return
			}
			if err != nil || out == nil {
				t.Fatalf("exported Analyze rejected fixture: %v", err)
			}
			if len(out.Resources) != 1 || len(out.Resources[0].Paths) != 1 || len(out.Coverage) != 1 || out.Coverage[0].Status != "generic_only" {
				t.Fatal("unexpected projection or coverage")
			}
			descend := func(v plan.Value) plan.Value {
				if v.State != "known" {
					return v
				}
				switch tc.mode {
				case "map":
					v = v.Value.(map[string]plan.Value)["slot"]
				case "list", "set":
					v = v.Value.([]plan.Value)[0]
				}
				if v.State != "known" {
					return v
				}
				return v.Value.(map[string]plan.Value)["private"]
			}
			p := out.Resources[0].Paths[0]
			bv, av := descend(p.Before), descend(p.After)
			if tc.mode == "map" {
				for _, side := range []plan.Value{p.Before, p.After} {
					if side.State != "known" {
						t.Fatal("map container hidden unnecessarily")
					}
					for _, label := range []string{"slot", "Slot", "other"} {
						child := side.Value.(map[string]plan.Value)[label]
						if child.State != "known" {
							t.Fatal("map child or safe sibling hidden unnecessarily")
						}
						fields := child.Value.(map[string]plan.Value)
						if fields["private"].State != "hidden" || fields["safe"].State != "known" || fields["safe"].Value != "visible" || fields["Private"].State != "known" || fields["Private"].Value != "case-visible" {
							t.Fatal("map child lost sensitivity or safe sibling")
						}
					}
				}
			}
			encoded, err := json.Marshal(out)
			if err != nil {
				t.Fatal("result encoding")
			}
			retained := bytes.Contains(encoded, []byte(old)) || bytes.Contains(encoded, []byte(next))
			if bv.State != "hidden" || av.State != "hidden" || retained {
				t.Fatalf("states=%s/%s retained=%t", bv.State, av.State, retained)
			}
			t.Logf("Analyze success; generic_only; private states=%s/%s; protected dummy bytes retained=%t", bv.State, av.State, retained)
		})
	}
}
