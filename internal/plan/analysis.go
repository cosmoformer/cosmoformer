// Package plan validates protected native evidence and produces only sanitized
// Changes. It has no dispatcher, persistence, readiness or execution authority.
package plan

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	tfjson "github.com/hashicorp/terraform-json"
	"github.com/zclconf/go-cty/cty"
)

const RuleVersion = "warnings-v1"
const MaxBytes = 67108864
const maxNodes = 2000000
const maxResources = 10000

// Errors intentionally contain no native bytes, paths or decoder diagnostics.
var ErrInvalid = errors.New("PLAN_ANALYSIS_INVALID")
var ErrUnsupported = errors.New("PLAN_ANALYSIS_UNSUPPORTED")
var ErrLeak = errors.New("NORMALIZED_SECRET_DETECTED")

type Value struct {
	State string `json:"state"`
	Type  string `json:"type,omitempty"`
	Value any    `json:"value,omitempty"`
}

// MarshalJSON must include false, empty string and empty collections for known
// values, while all other alternatives carry exactly one state field.
func (v Value) MarshalJSON() ([]byte, error) {
	if v.State != "known" {
		return json.Marshal(struct {
			State string `json:"state"`
		}{v.State})
	}
	return json.Marshal(struct {
		State string `json:"state"`
		Type  string `json:"type"`
		Value any    `json:"value"`
	}{v.State, v.Type, v.Value})
}

type PathChange struct {
	Path   []any `json:"path"`
	Before Value `json:"before"`
	After  Value `json:"after"`
}
type Finding struct {
	ID         string         `json:"id"`
	Rule       string         `json:"rule_id"`
	Severity   string         `json:"severity"`
	ResourceID string         `json:"resource_id"`
	Path       []any          `json:"path"`
	Message    string         `json:"message_code"`
	Arguments  map[string]any `json:"arguments"`
	Coverage   string         `json:"coverage"`
}
type Resource struct {
	// Validated native dispatch facts; never part of the serialized display.
	classificationType string
	classificationMode string
	ID                 string       `json:"id"`
	Address            string       `json:"address_display"`
	Type               string       `json:"type"`
	Provider           string       `json:"provider_identity"`
	Actions            []string     `json:"actions"`
	ReplacementOrder   string       `json:"replacement_order,omitempty"`
	ReplacementPaths   [][]any      `json:"replacement_paths,omitempty"`
	Paths              []PathChange `json:"paths"`
	Findings           []Finding    `json:"findings"`
}
type Coverage struct {
	Provider      string   `json:"provider_identity"`
	Version       string   `json:"provider_version"`
	Schema        string   `json:"schema_sha256"`
	Status        string   `json:"status"`
	Variants      []string `json:"unsupported_variants"`
	UnknownHidden []string `json:"unknown_hidden_resources"`
}
type Summary struct {
	Actions  map[string]int `json:"actions"`
	Findings map[string]int `json:"findings"`
}
type Result struct {
	RuleVersion string     `json:"rule_version"`
	ProfileID   string     `json:"profile_id"`
	Resources   []Resource `json:"resources"`
	Drift       []Resource `json:"drift"`
	Outputs     []Resource `json:"outputs"`
	Findings    []Finding  `json:"findings"`
	Coverage    []Coverage `json:"coverage"`
	Summary     Summary    `json:"summary"`
	Notices     []string   `json:"notices"`
}

// ResourceIDs and OutputIDs must be supplied by the caller's opaque identity
// allocator and retained with an attempt. Never derive these from native values.
// No variable values or raw JSON are returned. Retain original raw bytes using
// protected raw_plan_json custody separately; never persist them as Changes.
type Options struct {
	Engine      string
	Version     string
	ProfileID   string
	Providers   []runs.Provider
	ResourceIDs map[string]string
	OutputIDs   map[string]string
	Secrets     *evidence.Registry
}

type nativeProfile struct{ version, registry, schema string }

// Digests cover canonical complete provider_schemas[address] JSON (Go's
// encoding/json with UseNumber), not a hand-maintained schema subset. Evidence
// is reproduced by TestNativeEngineCompatibility in --network none containers.
var profiles = map[string]nativeProfile{
	"opentofu":  {"1.13.8", "registry.opentofu.org", "855291201a4219bb4b338cf189160978aacac2daadb4f7cd18e52e2bfc3baea4"},
	"terraform": {"1.17.1", "registry.terraform.io", "af6ea57eec4fef7a117c08c265cd60a5abcc87ede68202b8121f0c8e0a1bf0b6"},
}

func SupportedRuntime(engine, version string) bool {
	p, ok := profiles[engine]
	return ok && version == p.version
}

func ProfileID(engine string) string {
	if p, ok := profiles[engine]; ok {
		return engine + "-" + p.version + "-warnings-v1"
	}
	return ""
}
func digest(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func obj(v any) map[string]any { m, _ := v.(map[string]any); return m }
func str(v any) string         { s, _ := v.(string); return s }
func list(v any) []any         { a, _ := v.([]any); return a }
func keys(m map[string]any) []string {
	k := make([]string, 0, len(m))
	for s := range m {
		k = append(k, s)
	}
	sort.Strings(k)
	return k
}

// strictJSON rejects duplicate decoded keys, invalid UTF-8, trailing documents,
// excessive nesting/nodes and unbounded decimal expansion before typed decode.
// encoding/json replaces unpaired escaped UTF-16 surrogates. Reject them
// instead of changing protected bytes into a different decoded identity.
func validUnicodeEscapes(raw []byte) bool {
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue
		}
		if i+4 >= len(raw) {
			return false
		}
		n, e := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if e != nil {
			return false
		}
		i += 4
		if n >= 0xdc00 && n <= 0xdfff {
			return false
		}
		if n >= 0xd800 && n <= 0xdbff {
			if i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
				return false
			}
			m, e := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
			if e != nil || m < 0xdc00 || m > 0xdfff {
				return false
			}
			i += 6
		}
	}
	return true
}

func strictJSON(raw []byte) (any, error) {
	if len(raw) == 0 || len(raw) > MaxBytes || !utf8.Valid(raw) || !validUnicodeEscapes(raw) {
		return nil, ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	n := 0
	expanded := 0
	var read func(int) (any, error)
	read = func(depth int) (any, error) {
		n++
		if expanded > MaxBytes || n > maxNodes || depth > 96 {
			return nil, ErrInvalid
		}
		t, e := d.Token()
		if e != nil {
			return nil, ErrInvalid
		}
		switch x := t.(type) {
		case json.Delim:
			switch x {
			case '{':
				m := map[string]any{}
				for d.More() {
					k, e := d.Token()
					if e != nil {
						return nil, ErrInvalid
					}
					s, ok := k.(string)
					if !ok {
						return nil, ErrInvalid
					}
					if _, ok = m[s]; ok {
						return nil, ErrInvalid
					}
					v, e := read(depth + 1)
					if e != nil {
						return nil, e
					}
					expanded += len(s)
					if expanded > MaxBytes {
						return nil, ErrInvalid
					}
					m[s] = v
				}
				end, e := d.Token()
				if e != nil || end != json.Delim('}') {
					return nil, ErrInvalid
				}
				return m, nil
			case '[':
				a := []any{}
				for d.More() {
					v, e := read(depth + 1)
					if e != nil {
						return nil, e
					}
					a = append(a, v)
				}
				end, e := d.Token()
				if e != nil || end != json.Delim(']') {
					return nil, ErrInvalid
				}
				return a, nil
			}
			return nil, ErrInvalid
		case json.Number:
			number, e := evidence.CanonicalNumber(string(x))
			if e != nil {
				return nil, ErrInvalid
			}
			expanded += len(number)
		case string:
			expanded += len(x)
		}
		if expanded > MaxBytes {
			return nil, ErrInvalid
		}
		return t, nil
	}
	v, e := read(0)
	if e != nil {
		return nil, e
	}
	if _, e = d.Token(); e != io.EOF {
		return nil, ErrInvalid
	}
	return v, nil
}

type verifiedSchema struct {
	typed    tfjson.ProviderSchemas
	coverage []Coverage
	semantic map[string]bool
}

func checkSchema(raw []byte, o Options) (verifiedSchema, error) {
	out := verifiedSchema{semantic: map[string]bool{}}
	p, ok := profiles[o.Engine]
	if !ok || o.Version != p.version || o.ProfileID != ProfileID(o.Engine) {
		return out, ErrUnsupported
	}
	v, e := strictJSON(raw)
	if e != nil {
		return out, e
	}
	m := obj(v)
	if !canonicalSchemaFields(m, "format_version", "provider_schemas") {
		return out, ErrInvalid
	}
	if str(m["format_version"]) != "1.0" {
		return out, ErrUnsupported
	}
	schemas := obj(m["provider_schemas"])
	// Both pinned engines omit provider_schemas for an empty provider set.
	// An explicit null, wrong shape, or omission with external providers fails.
	if value, exists := m["provider_schemas"]; exists {
		if obj(value) == nil {
			return out, ErrInvalid
		}
	} else if len(o.Providers) > 0 {
		return out, ErrInvalid
	}
	if len(schemas) != len(o.Providers) || len(o.Providers) > 256 {
		return out, ErrInvalid
	}
	seen := map[string]bool{}
	for _, prov := range o.Providers {
		parts := strings.Split(prov.Address, "/")
		if len(parts) != 3 || (parts[0] != "registry.terraform.io" && parts[0] != "registry.opentofu.org") || parts[1] == "" || parts[2] == "" || prov.Version == "" || prov.Platform != "linux_amd64" || len(prov.Hashes) == 0 || seen[prov.Address] {
			return out, ErrInvalid
		}
		seen[prov.Address] = true
		s := obj(schemas[prov.Address])
		if !canonicalSchemaFields(s, "provider", "resource_schemas", "data_source_schemas", "ephemeral_resource_schemas", "action_schemas", "functions", "resource_identity_schemas", "list_resource_schemas", "state_store_schemas") || obj(s["provider"]) == nil || !validSchemaItem(obj(s["provider"])) {
			return out, ErrInvalid
		}
		for _, kind := range []string{"resource_schemas", "data_source_schemas"} {
			if entries, ok := s[kind]; ok {
				if obj(entries) == nil {
					return out, ErrInvalid
				}
				for _, item := range obj(entries) {
					if !validSchemaItem(obj(item)) {
						return out, ErrInvalid
					}
				}
			}
		}
		c := Coverage{prov.Address, prov.Version, digest(s), "generic_only", []string{}, []string{}}
		if parts[1] == "hashicorp" && parts[2] == "aws" {
			if prov.Address != p.registry+"/hashicorp/aws" || prov.Version != "6.41.0" || c.Schema != p.schema {
				return out, ErrUnsupported
			}
			c.Status = "supported"
			out.semantic[prov.Address] = true
		}
		out.coverage = append(out.coverage, c)
	}
	if json.Unmarshal(raw, &out.typed) != nil {
		return out, ErrInvalid
	}
	sort.Slice(out.coverage, func(i, j int) bool { return out.coverage[i].Provider < out.coverage[j].Provider })
	return out, nil
}

// encoding/json matches struct fields without regard to case. Reject metadata
// aliases that raw validation would ignore before handing the bytes to it.
// Resource, attribute and map-label dictionaries remain case-sensitive.
func canonicalSchemaFields(m map[string]any, fields ...string) bool {
	if m == nil {
		return false
	}
	for key := range m {
		for _, field := range fields {
			if key != field && strings.EqualFold(key, field) {
				return false
			}
		}
	}
	return true
}
func validSchemaItem(m map[string]any) bool {
	if !canonicalSchemaFields(m, "version", "block") {
		return false
	}
	n, ok := m["version"].(json.Number)
	if !ok {
		return false
	}
	i, e := strconv.ParseUint(string(n), 10, 64)
	_ = i
	if e != nil {
		return false
	}
	return validBlock(obj(m["block"]))
}
func validBlock(b map[string]any) bool {
	if !canonicalSchemaFields(b, "attributes", "block_types", "description", "description_kind", "deprecated") {
		return false
	}
	names := map[string]bool{}
	for _, kind := range []string{"attributes", "block_types"} {
		v, exists := b[kind]
		if !exists {
			continue
		}
		m := obj(v)
		if m == nil {
			return false
		}
		for name, v := range m {
			if names[name] {
				return false
			}
			names[name] = true
			item := obj(v)
			if item == nil {
				return false
			}
			if kind == "block_types" {
				if !canonicalSchemaFields(item, "nesting_mode", "block", "min_items", "max_items") {
					return false
				}
				mode := str(item["nesting_mode"])
				if mode != "single" && mode != "list" && mode != "set" && mode != "map" && mode != "group" {
					return false
				}
				if !validBlock(obj(item["block"])) {
					return false
				}
			} else {
				if !canonicalSchemaFields(item, "type", "nested_type", "description", "description_kind", "deprecated", "required", "optional", "computed", "sensitive", "write_only") {
					return false
				}
				_, hasType := item["type"]
				nested := obj(item["nested_type"])
				if hasType == (nested != nil) {
					return false
				}
				if nested != nil {
					if !canonicalSchemaFields(nested, "attributes", "nesting_mode", "min_items", "max_items") {
						return false
					}
					mode := str(nested["nesting_mode"])
					if mode != "single" && mode != "list" && mode != "set" && mode != "map" {
						return false
					}
					if !validBlock(map[string]any{"attributes": nested["attributes"]}) {
						return false
					}
				}
				for _, flag := range []string{"sensitive", "required", "optional", "computed"} {
					if v, ok := item[flag]; ok {
						if _, ok = v.(bool); !ok {
							return false
						}
					}
				}
			}
		}
	}
	return true
}

// Verifier implements coordinator.NativeEvidenceVerifier without importing the
// coordinator or enabling production admission. Callers must explicitly install
// it; successful verification alone conveys no readiness or launch authority.
type Verifier struct{}

func (Verifier) VerifySchema(ctx context.Context, s runs.ExecutionSpec, m runs.Manifest, raw []byte) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if s.Engine.Name != m.Engine.Name || s.Engine.Version != m.Engine.Version || s.Engine.Arch != "amd64" || m.Engine.Arch != "amd64" || s.ProfileID != m.Profile || s.RuleVersion != RuleVersion || m.Rules != RuleVersion || m.ValidateLock() != nil {
		return ErrInvalid
	}
	_, e := checkSchema(raw, Options{Engine: s.Engine.Name, Version: s.Engine.Version, ProfileID: s.ProfileID, Providers: m.Providers})
	return e
}

// Analyze is all-or-nothing: failure returns nil, never partial findings, raw
// bytes, or an empty successful fallback. Raw plan and schema are read-only.
func Analyze(raw, schema []byte, o Options) (*Result, error) {
	s, e := checkSchema(schema, o)
	if e != nil {
		return nil, e
	}
	return analyze(raw, o, s)
}
func analyze(raw []byte, o Options, s verifiedSchema) (*Result, error) {
	v, e := strictJSON(raw)
	if e != nil {
		return nil, e
	}
	m := obj(v)
	if e = validatePlan(m, o, s); e != nil {
		return nil, e
	}
	coverage := make([]Coverage, len(s.coverage))
	for i, c := range s.coverage {
		coverage[i] = c
		coverage[i].Variants = append([]string{}, c.Variants...)
		coverage[i].UnknownHidden = append([]string{}, c.UnknownHidden...)
	}
	reg := o.Secrets
	if reg == nil {
		reg = &evidence.Registry{}
	}
	out := &Result{RuleVersion: RuleVersion, ProfileID: o.ProfileID, Resources: []Resource{}, Drift: []Resource{}, Outputs: []Resource{}, Findings: []Finding{}, Coverage: coverage, Summary: Summary{map[string]int{}, map[string]int{}}, Notices: []string{"KNOWN_VALUE_REDACTION_MAY_BE_BROAD", "TRANSFORMED_AND_UNKNOWN_HISTORICAL_VALUES_NOT_TRACKED"}}
	used := map[string]bool{}
	owners := map[string]string{}
	for _, kind := range []string{"resource_changes", "resource_drift"} {
		for _, entry := range list(m[kind]) {
			rc := obj(entry)
			address := str(rc["address"])
			id := o.ResourceIDs[address]
			if !catalog.UUID(id) || owners[id] != "" && owners[id] != address {
				return nil, ErrInvalid
			}
			owners[id] = address
			// The same resource may also have separate drift; no independent apply
			// actions/findings are counted for that diagnostic history.
			if kind == "resource_changes" {
				if used[id] {
					return nil, ErrInvalid
				}
				used[id] = true
			}
			r, e := normalizeResource(rc, id, reg, s)
			if e != nil {
				return nil, e
			}
			for i := range out.Coverage {
				c := &out.Coverage[i]
				if c.Provider == r.Provider {
					for _, p := range r.Paths {
						if uncertainTree(p.Before) || uncertainTree(p.After) {
							c.UnknownHidden = append(c.UnknownHidden, r.ID)
							break
						}
					}
				}
				if c.Provider == r.Provider && s.semantic[r.Provider] && !supportedType(r.classificationType) {
					c.Variants = append(c.Variants, r.Type)
				}
			}
			if kind == "resource_drift" {
				if r.classificationMode == "managed" {
					if e = classify(&r, s.semantic[str(rc["provider_name"])]); e != nil {
						return nil, e
					}
				}
				r.Findings = []Finding{}
				out.Drift = append(out.Drift, r)
				continue
			}
			if r.classificationMode == "managed" {
				if e = classify(&r, s.semantic[str(rc["provider_name"])]); e != nil {
					return nil, e
				}
			}
			out.Resources = append(out.Resources, r)
			out.Findings = append(out.Findings, r.Findings...)
			for _, a := range r.Actions {
				out.Summary.Actions[a]++
			}
		}
	}
	for _, name := range keys(obj(m["output_changes"])) {
		id := o.OutputIDs[name]
		if !catalog.UUID(id) || used[id] || owners[id] != "" {
			return nil, ErrInvalid
		}
		used[id] = true
		before, after, e := normalizeChange(obj(obj(m["output_changes"])[name]), reg)
		if e != nil {
			return nil, e
		}
		address := name
		if containsKnown(reg, name) {
			address = "hidden"
		}
		out.Outputs = append(out.Outputs, Resource{ID: id, Address: address, Type: "output", Actions: actions(obj(obj(m["output_changes"])[name])), Paths: []PathChange{{[]any{}, before, after}}, Findings: []Finding{}})
	}
	sortResources(out.Resources)
	sortResources(out.Drift)
	sortResources(out.Outputs)
	sortFindings(out.Findings, out.Resources)
	for _, f := range out.Findings {
		out.Summary.Findings[f.Severity]++
	}
	for i := range out.Coverage {
		sort.Strings(out.Coverage[i].Variants)
		out.Coverage[i].Variants = unique(out.Coverage[i].Variants)
		sort.Strings(out.Coverage[i].UnknownHidden)
		out.Coverage[i].UnknownHidden = unique(out.Coverage[i].UnknownHidden)
		if out.Coverage[i].Status == "supported" && len(out.Coverage[i].UnknownHidden) > 0 {
			out.Coverage[i].Status = "valid_with_unknown_hidden"
		}
		if len(out.Coverage[i].Variants) > 0 {
			out.Coverage[i].Status = "partial"
		}
	}
	// Raw structure, required semantic shapes and supported actions have now
	// passed validation. Typed decoding is a final compatibility check, with
	// JSON numbers retained; it cannot erase missing-field distinctions.
	var typed tfjson.Plan
	typed.UseJSONNumber(true)
	if json.Unmarshal(raw, &typed) != nil {
		return nil, ErrInvalid
	}
	// Independently scan all returned strings, keys, identifiers and message args,
	// including metadata. A collision in fixed metadata fails closed as well.
	b, e := json.Marshal(out)
	if e != nil || len(b) > MaxBytes {
		return nil, ErrInvalid
	}
	if validateRegistry(b, reg) != nil {
		return nil, ErrLeak
	}
	return out, nil
}
func actions(m map[string]any) []string {
	a := []string{}
	for _, v := range list(m["actions"]) {
		a = append(a, str(v))
	}
	return a
}
func validActions(v any) bool {
	a, ok := v.([]any)
	if !ok {
		return false
	}
	b, _ := json.Marshal(a)
	switch string(b) {
	case `["no-op"]`, `["read"]`, `["create"]`, `["update"]`, `["delete"]`, `["delete","create"]`, `["create","delete"]`:
		return true
	}
	return false
}
func validatePlan(m map[string]any, o Options, s verifiedSchema) error {
	if m == nil || str(m["format_version"]) != "1.2" || str(m["terraform_version"]) != o.Version || obj(m["planned_values"]) == nil || obj(m["configuration"]) == nil {
		return ErrUnsupported
	}
	for _, flag := range []string{"errored", "complete", "applyable"} {
		if v, ok := m[flag]; ok {
			b, ok := v.(bool)
			if !ok {
				return ErrInvalid
			}
			if flag == "errored" && b || flag == "complete" && !b {
				return ErrUnsupported
			}
		}
	}
	for _, field := range []string{"deferred_changes", "action_invocations"} {
		if v, ok := m[field]; ok {
			a, ok := v.([]any)
			if !ok {
				return ErrInvalid
			}
			if len(a) > 0 {
				return ErrUnsupported
			}
		}
	}
	for _, kind := range []string{"resource_changes", "resource_drift"} {
		v, exists := m[kind]
		if !exists {
			continue
		}
		a, ok := v.([]any)
		if !ok || len(a) > maxResources {
			return ErrInvalid
		}
		seen := map[string]bool{}
		for _, v := range a {
			r := obj(v)
			if r == nil {
				return ErrInvalid
			}
			address := str(r["address"])
			mode := str(r["mode"])
			typ := str(r["type"])
			prov := str(r["provider_name"])
			if address == "" || seen[address] || str(r["name"]) == "" || typ == "" || (mode != "managed" && mode != "data") {
				return ErrInvalid
			}
			seen[address] = true
			ps := s.typed.Schemas[prov]
			if ps == nil {
				return ErrUnsupported
			}
			rs := ps.ResourceSchemas[typ]
			if mode == "data" {
				rs = ps.DataSourceSchemas[typ]
			}
			if rs == nil || rs.Block == nil {
				return ErrUnsupported
			}
			for _, field := range []string{"previous_address", "module_address", "action_reason", "deposed"} {
				if v, ok := r[field]; ok {
					if _, ok := v.(string); !ok {
						return ErrInvalid
					}
				}
			}
			if index, exists := r["index"]; exists {
				switch x := index.(type) {
				case string:
				case json.Number:
					if _, e := strconv.ParseUint(string(x), 10, 64); e != nil {
						return ErrInvalid
					}
				default:
					return ErrInvalid
				}
			}
			c := obj(r["change"])
			if e := validateChange(c); e != nil {
				return e
			}
			act := actions(c)
			if (len(act) == 2 || act[0] == "update" || act[0] == "delete" || act[0] == "no-op") && c["before"] == nil {
				return ErrInvalid
			}
			if !(len(act) == 1 && act[0] == "delete") && c["after"] == nil && c["after_unknown"] != true {
				return ErrInvalid
			}
			if importing, ok := c["importing"]; ok && importing != nil {
				return ErrUnsupported
			}
			if !validUnknownBlock(c["after_unknown"], rs.Block) {
				return ErrInvalid
			}
			for _, side := range []string{"before", "after"} {
				if c[side] != nil {
					if obj(c[side]) == nil || !validValueBlock(obj(c[side]), rs.Block) {
						return ErrInvalid
					}
				}
			}
			if paths, ok := c["replace_paths"]; ok {
				a, ok := paths.([]any)
				if !ok || len(a) > 10000 || len(a) > 0 && len(act) != 2 {
					return ErrInvalid
				}
				for _, path := range a {
					p, ok := path.([]any)
					if !ok || len(p) > 96 {
						return ErrInvalid
					}
					for _, part := range p {
						switch x := part.(type) {
						case string:
						case json.Number:
							i, e := strconv.Atoi(string(x))
							if e != nil || i < 0 {
								return ErrInvalid
							}
						default:
							return ErrInvalid
						}
					}
				}
			}
		}
	}
	if v, ok := m["output_changes"]; ok {
		outputs := obj(v)
		if outputs == nil || len(outputs) > maxResources {
			return ErrInvalid
		}
		for k, v := range outputs {
			if k == "" {
				return ErrInvalid
			}
			if e := validateChange(obj(v)); e != nil {
				return e
			}
		}
	}
	return nil
}
func validateChange(c map[string]any) error {
	if c == nil {
		return ErrInvalid
	}
	if !validActions(c["actions"]) {
		return ErrUnsupported
	}
	for _, k := range []string{"before", "after", "after_unknown", "before_sensitive", "after_sensitive"} {
		if _, ok := c[k]; !ok {
			return ErrInvalid
		}
	}
	for _, k := range []string{"after_unknown", "before_sensitive", "after_sensitive"} {
		if !validMarker(c[k]) {
			return ErrInvalid
		}
	}
	a := actions(c)
	if len(a) == 1 {
		switch a[0] {
		case "create":
			if c["before"] != nil {
				return ErrInvalid
			}
		case "delete":
			if c["after"] != nil {
				return ErrInvalid
			}
		}
	}
	return nil
}
func validMarker(v any) bool {
	switch x := v.(type) {
	case bool:
		return true
	case []any:
		for _, v := range x {
			if !validMarker(v) {
				return false
			}
		}
		return true
	case map[string]any:
		for _, v := range x {
			if !validMarker(v) {
				return false
			}
		}
		return true
	}
	return false
}
func validValueBlock(m map[string]any, b *tfjson.SchemaBlock) bool {
	for k, v := range m {
		if a := b.Attributes[k]; a != nil {
			if a.AttributeNestedType != nil && !validNestedAttribute(v, a.AttributeNestedType) || a.AttributeType != cty.NilType && !validType(v, a.AttributeType) {
				return false
			}
			continue
		}
		if n := b.NestedBlocks[k]; n != nil {
			if v == nil {
				continue
			}
			switch n.NestingMode {
			case tfjson.SchemaNestingModeSingle, tfjson.SchemaNestingModeGroup:
				if obj(v) == nil || !validValueBlock(obj(v), n.Block) {
					return false
				}
			case tfjson.SchemaNestingModeList, tfjson.SchemaNestingModeSet:
				a, ok := v.([]any)
				if !ok {
					return false
				}
				for _, v := range a {
					if obj(v) == nil || !validValueBlock(obj(v), n.Block) {
						return false
					}
				}
			case tfjson.SchemaNestingModeMap:
				if obj(v) == nil {
					return false
				}
				for _, v := range obj(v) {
					if obj(v) == nil || !validValueBlock(obj(v), n.Block) {
						return false
					}
				}
			default:
				return false
			}
			continue
		}
		return false
	}
	return true
}
func validType(v any, t cty.Type) bool {
	if v == nil || t == cty.DynamicPseudoType {
		return true
	}
	switch {
	case t == cty.String:
		_, ok := v.(string)
		return ok
	case t == cty.Number:
		_, ok := v.(json.Number)
		return ok
	case t == cty.Bool:
		_, ok := v.(bool)
		return ok
	case t.IsObjectType():
		m := obj(v)
		if m == nil {
			return false
		}
		for k, v := range m {
			if !t.HasAttribute(k) || !validType(v, t.AttributeType(k)) {
				return false
			}
		}
		return true
	case t.IsMapType():
		m := obj(v)
		if m == nil {
			return false
		}
		for _, v := range m {
			if !validType(v, t.ElementType()) {
				return false
			}
		}
		return true
	case t.IsListType() || t.IsSetType():
		a, ok := v.([]any)
		if !ok {
			return false
		}
		for _, v := range a {
			if !validType(v, t.ElementType()) {
				return false
			}
		}
		return true
	case t.IsTupleType():
		a, ok := v.([]any)
		ts := t.TupleElementTypes()
		if !ok || len(a) != len(ts) {
			return false
		}
		for i, v := range a {
			if !validType(v, ts[i]) {
				return false
			}
		}
		return true
	}
	return false
}

func hasTrue(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case map[string]any:
		for _, v := range x {
			if hasTrue(v) {
				return true
			}
		}
	case []any:
		for _, v := range x {
			if hasTrue(v) {
				return true
			}
		}
	}
	return false
}
func markerChild(v any, k string, i int) any {
	if m := obj(v); m != nil {
		return m[k]
	}
	if a, ok := v.([]any); ok && i >= 0 && i < len(a) {
		return a[i]
	}
	return nil
}
func markerAmbiguous(v, marker any) bool {
	if !hasTrue(marker) {
		return false
	}
	switch m := marker.(type) {
	case bool:
		return false
	case map[string]any:
		if obj(v) == nil {
			return true
		}
	case []any:
		a, ok := v.([]any)
		if !ok || len(a) != len(m) {
			return true
		}
	}
	return false
}
func scalar(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, true
	case bool:
		return strconv.FormatBool(x), true
	case json.Number:
		n, _ := evidence.CanonicalNumber(string(x))
		return n, true
	}
	return "", false
}
func unknownShapeMismatch(v, marker any) bool {
	if !hasTrue(marker) {
		return false
	}
	if obj(marker) != nil {
		return v != nil && obj(v) == nil
	}
	if m, ok := marker.([]any); ok {
		a, ok := v.([]any)
		return !ok || len(a) != len(m)
	}
	return false
}

// Native addresses quote instance keys using string escapes. Check their
// decoded quoted tokens too, so an escaped quote, slash or Unicode code point
// in a supplied value cannot evade the identifier/argument boundary.
func containsKnown(reg *evidence.Registry, value string) bool {
	if reg.Contains(value) {
		return true
	}
	for i := 0; i < len(value); i++ {
		if value[i] != '"' {
			continue
		}
		closed := false
		for j := i + 1; j < len(value); j++ {
			if value[j] == '\\' {
				j++
				continue
			}
			if value[j] == '"' {
				decoded, e := strconv.Unquote(value[i : j+1])
				if e == nil && reg.Contains(decoded) {
					return true
				}
				i = j
				closed = true
				break
			}
		}
		if !closed {
			return false
		}
	}
	return false
}

func hiddenTree(v Value) bool {
	if v.State == "hidden" {
		return true
	}
	switch x := v.Value.(type) {
	case map[string]Value:
		for _, v := range x {
			if hiddenTree(v) {
				return true
			}
		}
	case []Value:
		for _, v := range x {
			if hiddenTree(v) {
				return true
			}
		}
	}
	return false
}

func rawContainsKnown(v any, reg *evidence.Registry) bool {
	if s, ok := scalar(v); ok && containsKnown(reg, s) {
		return true
	}
	switch x := v.(type) {
	case map[string]any:
		for k, v := range x {
			if containsKnown(reg, k) || rawContainsKnown(v, reg) {
				return true
			}
		}
	case []any:
		for _, v := range x {
			if rawContainsKnown(v, reg) {
				return true
			}
		}
	}
	return false
}

func normalizePair(b any, bok bool, a any, aok bool, bs, as, u any, reg *evidence.Registry) (Value, Value) {
	return normalizePairAt(b, bok, a, aok, bs, as, u, reg, false)
}
func normalizePairAt(b any, bok bool, a any, aok bool, bs, as, u any, reg *evidence.Registry, resourceRoot bool) (Value, Value) {
	hidden := Value{State: "hidden"}
	if bs == true || as == true || markerAmbiguous(b, bs) || markerAmbiguous(a, as) {
		return hidden, hidden
	}
	// A collection whose order/size changes cannot reliably transfer a nested
	// sensitivity marker to the other side. Hide its parent, preserve siblings.
	if ba, ok := b.([]any); ok && (hasTrue(bs) || hasTrue(as)) && len(ba) != len(list(a)) {
		return hidden, hidden
	}
	for _, v := range []any{b, a} {
		if s, ok := scalar(v); ok && containsKnown(reg, s) {
			return hidden, hidden
		}
		for k := range obj(v) {
			if containsKnown(reg, k) {
				return hidden, hidden
			}
		}
	}
	// A nested sensitive path cannot transfer to a different shape. Hide
	// that parent rather than exposing the other logical side. Resource roots
	// are projected as individual paths below, preserving safe root siblings.
	shape := func(v any, exists bool) string {
		if !exists {
			return "absent"
		}
		switch v.(type) {
		case map[string]any:
			return "object"
		case []any:
			return "array"
		case string:
			return "string"
		case json.Number:
			return "number"
		case bool:
			return "boolean"
		}
		return "null"
	}
	if !resourceRoot && shape(b, bok) != shape(a, aok) && (hasTrue(bs) || hasTrue(as) || rawContainsKnown(b, reg) || rawContainsKnown(a, reg)) {
		return hidden, hidden
	}
	// Without a safe association, a changed collection containing a known
	// value must hide its parent. Positional masking alone could expose the
	// other value of the same logical set member after a reorder.
	_, beforeArray := b.([]any)
	_, afterArray := a.([]any)
	if (beforeArray || afterArray) && digest(b) != digest(a) && (rawContainsKnown(b, reg) || rawContainsKnown(a, reg)) {
		return hidden, hidden
	}
	makeScalar := func(v any, exists bool, unknown any) Value {
		if unknown == true || unknownShapeMismatch(v, unknown) {
			return Value{State: "unknown"}
		}
		if !exists {
			return Value{State: "absent"}
		}
		if v == nil {
			return Value{State: "null"}
		}
		switch x := v.(type) {
		case string:
			return Value{"known", "string", x}
		case bool:
			return Value{"known", "boolean", x}
		case json.Number:
			n, _ := evidence.CanonicalNumber(string(x))
			return Value{"known", "number", n}
		}
		return Value{State: "absent"}
	}
	bv, av := makeScalar(b, bok, nil), makeScalar(a, aok, u)
	bm, am := obj(b), obj(a)
	if bm != nil || am != nil || obj(u) != nil {
		union := map[string]any{}
		for _, m := range []map[string]any{bm, am, obj(u), obj(bs), obj(as)} {
			for k := range m {
				union[k] = true
				if containsKnown(reg, k) {
					return hidden, hidden
				}
			}
		}
		bo, ao := map[string]Value{}, map[string]Value{}
		for _, k := range keys(union) {
			bb, bok := bm[k]
			aa, aok := am[k]
			bbv, aav := normalizePair(bb, bok, aa, aok, markerChild(bs, k, -1), markerChild(as, k, -1), markerChild(u, k, -1), reg)
			bo[k] = bbv
			ao[k] = aav
		}
		if bm != nil {
			bv = Value{"known", "object", bo}
		}
		if am != nil && av.State != "unknown" {
			av = Value{"known", "object", ao}
		} else if obj(u) != nil && av.State == "absent" {
			av = Value{"known", "object", ao}
		}
	}
	ba, barray := b.([]any)
	aa, aarray := a.([]any)
	if barray || aarray {
		count := max(len(ba), len(aa))
		bo, ao := []Value{}, []Value{}
		for i := 0; i < count; i++ {
			var bb, ab any
			if i < len(ba) {
				bb = ba[i]
			}
			if i < len(aa) {
				ab = aa[i]
			}
			bbv, aav := normalizePair(bb, i < len(ba), ab, i < len(aa), markerChild(bs, "", i), markerChild(as, "", i), markerChild(u, "", i), reg)
			if i < len(ba) {
				bo = append(bo, bbv)
			}
			if i < len(aa) {
				ao = append(ao, aav)
			}
		}
		if barray {
			bv = Value{"known", "array", bo}
		}
		if aarray && av.State != "unknown" {
			av = Value{"known", "array", ao}
		}
	}
	return bv, av
}
func normalizeChange(c map[string]any, reg *evidence.Registry) (Value, Value, error) {
	b, a := normalizePair(c["before"], true, c["after"], true, c["before_sensitive"], c["after_sensitive"], c["after_unknown"], reg)
	return b, a, nil
}
func schemaSensitivity(block *tfjson.SchemaBlock, v any) any {
	m := obj(v)
	if m == nil {
		return false
	}
	mask := map[string]any{}
	for k, v := range m {
		if a := block.Attributes[k]; a != nil {
			if a.Sensitive {
				mask[k] = true
			} else if a.AttributeNestedType != nil {
				mask[k] = nestedSensitivity(a.AttributeNestedType, v)
			}
		}
		if n := block.NestedBlocks[k]; n != nil {
			switch n.NestingMode {
			case tfjson.SchemaNestingModeMap:
				children := map[string]any{}
				for label, child := range obj(v) {
					children[label] = schemaSensitivity(n.Block, child)
				}
				mask[k] = children
			case tfjson.SchemaNestingModeSingle, tfjson.SchemaNestingModeGroup:
				mask[k] = schemaSensitivity(n.Block, v)
			case tfjson.SchemaNestingModeList, tfjson.SchemaNestingModeSet:
				a := []any{}
				for _, v := range list(v) {
					a = append(a, schemaSensitivity(n.Block, v))
				}
				mask[k] = a
			}
		}
	}
	return mask
}
func mergeMarkers(a, b any) any {
	if a == true || b == true {
		return true
	}
	if am, bm := obj(a), obj(b); am != nil || bm != nil {
		m := map[string]any{}
		for k, v := range am {
			m[k] = v
		}
		for k, v := range bm {
			m[k] = mergeMarkers(m[k], v)
		}
		return m
	}
	aa, aok := a.([]any)
	ba, bok := b.([]any)
	if aok || bok {
		out := []any{}
		for i := 0; i < max(len(aa), len(ba)); i++ {
			out = append(out, mergeMarkers(markerChild(a, "", i), markerChild(b, "", i)))
		}
		return out
	}
	return false
}
func normalizeResource(rc map[string]any, id string, reg *evidence.Registry, s verifiedSchema) (Resource, error) {
	address := str(rc["address"])
	if containsKnown(reg, address) {
		address = "hidden"
	}
	typ, prov := str(rc["type"]), str(rc["provider_name"])
	// Resource name/module/index are intentionally not separately published.
	if containsKnown(reg, typ) {
		typ = "hidden"
	}
	if containsKnown(reg, prov) {
		return Resource{}, ErrLeak
	}
	r := Resource{classificationType: str(rc["type"]), classificationMode: str(rc["mode"]), ID: id, Address: address, Type: typ, Provider: prov, Actions: actions(obj(rc["change"])), Paths: []PathChange{}, Findings: []Finding{}}
	c := obj(rc["change"])
	ps := s.typed.Schemas[str(rc["provider_name"])]
	schema := ps.ResourceSchemas[str(rc["type"])]
	if str(rc["mode"]) == "data" {
		schema = ps.DataSourceSchemas[str(rc["type"])]
	}
	copyChange := map[string]any{}
	for k, v := range c {
		copyChange[k] = v
	}
	copyChange["before_sensitive"] = mergeMarkers(c["before_sensitive"], schemaSensitivity(schema.Block, c["before"]))
	copyChange["after_sensitive"] = mergeMarkers(c["after_sensitive"], schemaSensitivity(schema.Block, c["after"]))
	bs, as := pairedSensitivity(c["before"], c["after"], copyChange["before_sensitive"], copyChange["after_sensitive"], blockType(schema.Block))
	copyChange["before_sensitive"], copyChange["after_sensitive"] = bs, as
	b, a := normalizePairAt(copyChange["before"], true, copyChange["after"], true, copyChange["before_sensitive"], copyChange["after_sensitive"], copyChange["after_unknown"], reg, true)
	if b.Type == "object" || a.Type == "object" {
		union := map[string]any{}
		bm, _ := b.Value.(map[string]Value)
		am, _ := a.Value.(map[string]Value)
		for k := range bm {
			union[k] = true
		}
		for k := range am {
			union[k] = true
		}
		for _, k := range keys(union) {
			bv, bok := bm[k]
			av, aok := am[k]
			if !bok {
				bv = Value{State: b.State}
				if b.State == "known" {
					bv.State = "absent"
				}
			}
			if !aok {
				av = Value{State: a.State}
				if a.State == "known" {
					av.State = "absent"
				}
			}
			if bv.State == "hidden" || av.State == "hidden" || hiddenTree(bv) && av.State != "known" || hiddenTree(av) && bv.State != "known" {
				bv = Value{State: "hidden"}
				av = Value{State: "hidden"}
			}
			r.Paths = append(r.Paths, PathChange{[]any{k}, bv, av})
		}
	} else {
		r.Paths = append(r.Paths, PathChange{[]any{}, b, a})
	}
	if len(r.Actions) == 2 {
		r.ReplacementOrder = "destroy_then_create"
		if r.Actions[0] == "create" {
			r.ReplacementOrder = "create_then_destroy"
		}
		for _, path := range list(c["replace_paths"]) {
			p := safePath(list(path), b, a, reg)
			r.ReplacementPaths = append(r.ReplacementPaths, p)
		}
		sort.Slice(r.ReplacementPaths, func(i, j int) bool { return pathKey(r.ReplacementPaths[i]) < pathKey(r.ReplacementPaths[j]) })
	}
	return r, nil
}
func safePath(path []any, b, a Value, reg *evidence.Registry) []any {
	out := []any{}
	for _, part := range path {
		if b.State == "hidden" || a.State == "hidden" || b.State == "unknown" || a.State == "unknown" {
			return out
		}
		switch x := part.(type) {
		case string:
			if containsKnown(reg, x) {
				return out
			}
			out = append(out, x)
			bm, _ := b.Value.(map[string]Value)
			am, _ := a.Value.(map[string]Value)
			b = bm[x]
			a = am[x]
		case json.Number:
			i, _ := strconv.Atoi(string(x))
			out = append(out, i)
			ba, _ := b.Value.([]Value)
			aa, _ := a.Value.([]Value)
			b = Value{}
			a = Value{}
			if i < len(ba) {
				b = ba[i]
			}
			if i < len(aa) {
				a = aa[i]
			}
		}
	}
	return out
}
func pathKey(p []any) string { b, _ := json.Marshal(p); return string(b) }
func sortResources(rs []Resource) {
	sort.Slice(rs, func(i, j int) bool {
		a, b := rs[i].Address, rs[j].Address
		if a == "hidden" {
			a = rs[i].ID
		}
		if b == "hidden" {
			b = rs[j].ID
		}
		if a == b {
			return rs[i].ID < rs[j].ID
		}
		return a < b
	})
}
func sortFindings(fs []Finding, rs []Resource) {
	addresses := map[string]string{}
	for _, r := range rs {
		addresses[r.ID] = r.Address
		if r.Address == "hidden" {
			addresses[r.ID] = r.ID
		}
	}
	rank := map[string]int{"critical": 0, "high": 1, "attention": 2, "unknown_detail": 3}
	sort.Slice(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if a.Severity != b.Severity {
			return rank[a.Severity] < rank[b.Severity]
		}
		if addresses[a.ResourceID] != addresses[b.ResourceID] {
			return addresses[a.ResourceID] < addresses[b.ResourceID]
		}
		if a.Rule != b.Rule {
			return a.Rule < b.Rule
		}
		if pathKey(a.Path) != pathKey(b.Path) {
			return pathKey(a.Path) < pathKey(b.Path)
		}
		return a.ID < b.ID
	})
}
func unique(a []string) []string {
	out := []string{}
	for _, s := range a {
		if len(out) == 0 || out[len(out)-1] != s {
			out = append(out, s)
		}
	}
	return out
}

// ValidateManagedSecrets is an independent platform-side check of untrusted
// normalized_changes/findings bytes. It does not trust worker masking or rule
// output and must be run again before any future readiness transaction.
func ValidateManagedSecrets(data []byte, secrets []runs.EnvSecret) error {
	reg := &evidence.Registry{}
	for _, s := range secrets {
		t, e := evidence.SecretTerms(s.Value, strings.HasPrefix(s.Destination, "TF_VAR_"))
		if e != nil {
			return ErrInvalid
		}
		if reg.Register(t...) != nil {
			return ErrInvalid
		}
	}
	return validateRegistry(data, reg)
}
func validateRegistry(data []byte, reg *evidence.Registry) error {
	v, e := strictJSON(data)
	if e != nil {
		return e
	}
	var walk func(any) bool
	walk = func(v any) bool {
		if s, ok := scalar(v); ok && containsKnown(reg, s) {
			return false
		}
		switch x := v.(type) {
		case map[string]any:
			for k, v := range x {
				if containsKnown(reg, k) || !walk(v) {
					return false
				}
			}
		case []any:
			for _, v := range x {
				if !walk(v) {
					return false
				}
			}
		}
		return true
	}
	if !walk(v) {
		return ErrLeak
	}
	return nil
}

func validNestedAttribute(v any, n *tfjson.SchemaNestedAttributeType) bool {
	if v == nil {
		return true
	}
	b := &tfjson.SchemaBlock{Attributes: n.Attributes}
	switch n.NestingMode {
	case tfjson.SchemaNestingModeSingle:
		return obj(v) != nil && validValueBlock(obj(v), b)
	case tfjson.SchemaNestingModeMap:
		if obj(v) == nil {
			return false
		}
		for _, child := range obj(v) {
			if obj(child) == nil || !validValueBlock(obj(child), b) {
				return false
			}
		}
		return true
	case tfjson.SchemaNestingModeList, tfjson.SchemaNestingModeSet:
		a, ok := v.([]any)
		if !ok {
			return false
		}
		for _, child := range a {
			if obj(child) == nil || !validValueBlock(obj(child), b) {
				return false
			}
		}
		return true
	}
	return false
}
func nestedSensitivity(n *tfjson.SchemaNestedAttributeType, v any) any {
	b := &tfjson.SchemaBlock{Attributes: n.Attributes}
	if n.NestingMode == tfjson.SchemaNestingModeSingle {
		return schemaSensitivity(b, v)
	}
	if n.NestingMode == tfjson.SchemaNestingModeMap {
		m := map[string]any{}
		for k, child := range obj(v) {
			m[k] = schemaSensitivity(b, child)
		}
		return m
	}
	a := []any{}
	for _, child := range list(v) {
		a = append(a, schemaSensitivity(b, child))
	}
	return a
}
func validUnknownBlock(u any, b *tfjson.SchemaBlock) bool {
	if u == true || !hasTrue(u) {
		return true
	}
	m := obj(u)
	if m == nil {
		return false
	}
	for k, u := range m {
		if a := b.Attributes[k]; a != nil {
			if a.AttributeNestedType != nil {
				if !validUnknownNested(u, a.AttributeNestedType) {
					return false
				}
			} else if !validUnknownType(u, a.AttributeType) {
				return false
			}
			continue
		}
		if n := b.NestedBlocks[k]; n != nil {
			switch n.NestingMode {
			case tfjson.SchemaNestingModeSingle, tfjson.SchemaNestingModeGroup:
				if !validUnknownBlock(u, n.Block) {
					return false
				}
			case tfjson.SchemaNestingModeMap:
				if u != true && hasTrue(u) {
					if obj(u) == nil {
						return false
					}
					for _, u := range obj(u) {
						if !validUnknownBlock(u, n.Block) {
							return false
						}
					}
				}
			default:
				if u != true && hasTrue(u) {
					a, ok := u.([]any)
					if !ok {
						return false
					}
					for _, u := range a {
						if !validUnknownBlock(u, n.Block) {
							return false
						}
					}
				}
			}
			continue
		}
		return false
	}
	return true
}
func validUnknownNested(u any, n *tfjson.SchemaNestedAttributeType) bool {
	if u == true || !hasTrue(u) {
		return true
	}
	b := &tfjson.SchemaBlock{Attributes: n.Attributes}
	if n.NestingMode == tfjson.SchemaNestingModeSingle {
		return validUnknownBlock(u, b)
	}
	if n.NestingMode == tfjson.SchemaNestingModeMap {
		if obj(u) == nil {
			return false
		}
		for _, u := range obj(u) {
			if !validUnknownBlock(u, b) {
				return false
			}
		}
		return true
	}
	a, ok := u.([]any)
	if !ok {
		return false
	}
	for _, u := range a {
		if !validUnknownBlock(u, b) {
			return false
		}
	}
	return true
}
func validUnknownType(u any, t cty.Type) bool {
	if u == true || !hasTrue(u) || t == cty.DynamicPseudoType {
		return true
	}
	if t.IsObjectType() {
		if obj(u) == nil {
			return false
		}
		for k, u := range obj(u) {
			if !t.HasAttribute(k) || !validUnknownType(u, t.AttributeType(k)) {
				return false
			}
		}
		return true
	}
	if t.IsMapType() {
		if obj(u) == nil {
			return false
		}
		for _, u := range obj(u) {
			if !validUnknownType(u, t.ElementType()) {
				return false
			}
		}
		return true
	}
	if t.IsListType() || t.IsSetType() {
		a, ok := u.([]any)
		if !ok {
			return false
		}
		for _, u := range a {
			if !validUnknownType(u, t.ElementType()) {
				return false
			}
		}
		return true
	}
	if t.IsTupleType() {
		a, ok := u.([]any)
		if !ok || len(a) > len(t.TupleElementTypes()) {
			return false
		}
		for i, u := range a {
			if !validUnknownType(u, t.TupleElementTypes()[i]) {
				return false
			}
		}
		return true
	}
	return false
}

// Ordered list positions are meaningful; set positions are not. Transfer a
// sensitive marker across a changed set only by hiding that set's parent.
func blockType(b *tfjson.SchemaBlock) cty.Type {
	attrs := map[string]cty.Type{}
	for k, a := range b.Attributes {
		if a.AttributeNestedType != nil {
			attrs[k] = nestedAttributeType(a.AttributeNestedType)
		} else if a.AttributeType != cty.NilType {
			attrs[k] = a.AttributeType
		} else {
			attrs[k] = cty.DynamicPseudoType
		}
	}
	for k, n := range b.NestedBlocks {
		attrs[k] = collectionType(blockType(n.Block), n.NestingMode)
	}
	return cty.Object(attrs)
}
func nestedAttributeType(n *tfjson.SchemaNestedAttributeType) cty.Type {
	return collectionType(blockType(&tfjson.SchemaBlock{Attributes: n.Attributes}), n.NestingMode)
}
func collectionType(t cty.Type, mode tfjson.SchemaNestingMode) cty.Type {
	switch mode {
	case tfjson.SchemaNestingModeList:
		return cty.List(t)
	case tfjson.SchemaNestingModeSet:
		return cty.Set(t)
	case tfjson.SchemaNestingModeMap:
		return cty.Map(t)
	}
	return t
}
func pairedSensitivity(b, a, bs, as any, t cty.Type) (any, any) {
	if markerAmbiguous(b, bs) || markerAmbiguous(a, as) {
		return true, true
	}
	if t.IsSetType() && (hasTrue(bs) || hasTrue(as)) && digest(b) != digest(a) {
		return true, true
	}
	if t.IsObjectType() || t.IsMapType() {
		bm, am := map[string]any{}, map[string]any{}
		for k, v := range obj(bs) {
			bm[k] = v
		}
		for k, v := range obj(as) {
			am[k] = v
		}
		union := map[string]bool{}
		for k := range bm {
			union[k] = true
		}
		for k := range am {
			union[k] = true
		}
		for k := range union {
			child := cty.DynamicPseudoType
			if t.IsObjectType() {
				if !t.HasAttribute(k) {
					continue
				}
				child = t.AttributeType(k)
			} else {
				child = t.ElementType()
			}
			bm[k], am[k] = pairedSensitivity(obj(b)[k], obj(a)[k], bm[k], am[k], child)
		}
		return mergeMarkers(bs, bm), mergeMarkers(as, am)
	}
	if t.IsListType() || t.IsTupleType() {
		ba, aa := list(b), list(a)
		bm, am := []any{}, []any{}
		for i := 0; i < max(len(list(bs)), len(list(as))); i++ {
			var bv, av any
			if i < len(ba) {
				bv = ba[i]
			}
			if i < len(aa) {
				av = aa[i]
			}
			child := cty.DynamicPseudoType
			if t.IsListType() {
				child = t.ElementType()
			} else if i < len(t.TupleElementTypes()) {
				child = t.TupleElementTypes()[i]
			}
			bs, as := pairedSensitivity(bv, av, markerChild(bs, "", i), markerChild(as, "", i), child)
			bm = append(bm, bs)
			am = append(am, as)
		}
		return mergeMarkers(bs, bm), mergeMarkers(as, am)
	}
	return bs, as
}
