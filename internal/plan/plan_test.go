package plan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	tfjson "github.com/hashicorp/terraform-json"
)

const fixtureID = "11111111-1111-4111-8111-111111111111"
const outputID = "22222222-2222-4222-8222-222222222222"

type planFixture struct {
	Name     string          `json:"name"`
	Plan     json.RawMessage `json:"plan"`
	Expected []struct {
		Rule     string `json:"rule_id"`
		Severity string `json:"severity"`
		Message  string `json:"message_code"`
	} `json:"expected"`
	Error   bool `json:"error"`
	Secrets []struct {
		Value string `json:"value"`
		JSON  bool   `json:"json_variable"`
	} `json:"secrets"`
	States        map[string]string `json:"states"`
	Order         string            `json:"replacement_order"`
	Variant       string            `json:"unsupported_variant"`
	HiddenAddress bool              `json:"hidden_address"`
	Number        string            `json:"number"`
}

func fixtureSchema(t *testing.T) verifiedSchema {
	t.Helper()
	b, e := os.ReadFile("../testdata/compatibility.json")
	if e != nil {
		t.Fatal(e)
	}
	var data struct {
		Schemas map[string]json.RawMessage `json:"selected_resource_schemas"`
	}
	if e = json.Unmarshal(b, &data); e != nil {
		t.Fatal(e)
	}
	raw, e := json.Marshal(map[string]any{"format_version": "1.0", "provider_schemas": map[string]any{"registry.opentofu.org/hashicorp/aws": map[string]any{"provider": map[string]any{"version": 0, "block": map[string]any{}}, "resource_schemas": data.Schemas}}})
	if e != nil {
		t.Fatal(e)
	}
	// This explicit package-private seam exercises synthetic plans against
	// extracted real shapes. It is never exposed as a compatible production API.
	var typed tfjson.ProviderSchemas
	if e = json.Unmarshal(raw, &typed); e != nil {
		t.Fatal(e)
	}
	return verifiedSchema{typed: typed, semantic: map[string]bool{"registry.opentofu.org/hashicorp/aws": true}, coverage: []Coverage{{"registry.opentofu.org/hashicorp/aws", "6.41.0", "synthetic-test-only", "supported", []string{}, []string{}}}}
}
func fixtureOptions(t *testing.T, raw []byte) Options {
	t.Helper()
	v, e := strictJSON(raw)
	if e != nil {
		t.Fatal(e)
	}
	m := obj(v)
	o := Options{Engine: "opentofu", Version: "1.13.8", ProfileID: ProfileID("opentofu"), ResourceIDs: map[string]string{}, OutputIDs: map[string]string{}, Secrets: &evidence.Registry{}}
	for _, r := range list(m["resource_changes"]) {
		o.ResourceIDs[str(obj(r)["address"])] = fixtureID
	}
	for _, r := range list(m["resource_drift"]) {
		o.ResourceIDs[str(obj(r)["address"])] = fixtureID
	}
	for _, name := range keys(obj(m["output_changes"])) {
		o.OutputIDs[name] = outputID
	}
	return o
}
func loadFixtures(t *testing.T) []planFixture {
	t.Helper()
	b, e := os.ReadFile("../testdata/plans.json")
	if e != nil {
		t.Fatal(e)
	}
	var data struct {
		Cases []planFixture `json:"cases"`
	}
	if json.Unmarshal(b, &data) != nil {
		t.Fatal("invalid fixtures")
	}
	return data.Cases
}
func TestPlanFixtures(t *testing.T) {
	s := fixtureSchema(t)
	for _, f := range loadFixtures(t) {
		t.Run(f.Name, func(t *testing.T) {
			o := fixtureOptions(t, f.Plan)
			for _, secret := range f.Secrets {
				terms, e := evidence.SecretTerms(secret.Value, secret.JSON)
				if e != nil {
					t.Fatal(e)
				}
				if e = o.Secrets.Register(terms...); e != nil {
					t.Fatal(e)
				}
			}
			original := append([]byte(nil), f.Plan...)
			out, e := analyze(f.Plan, o, s)
			if f.Error {
				if e == nil || out != nil {
					t.Fatal("invalid analysis yielded normalized data")
				}
				return
			}
			if e != nil {
				t.Fatal(e)
			}
			if !bytes.Equal(original, f.Plan) {
				t.Fatal("mutated protected evidence")
			}
			if len(out.Findings) != len(f.Expected) {
				t.Fatalf("findings: got %+v, expected %+v", out.Findings, f.Expected)
			}
			for i, want := range f.Expected {
				got := out.Findings[i]
				if got.Rule != want.Rule || got.Severity != want.Severity || want.Message != "" && got.Message != want.Message {
					t.Fatalf("finding: got %+v, expected %+v", got, want)
				}
			}
			r := out.Resources[0]
			for _, finding := range out.Findings {
				if finding.Coverage == "uncertain" && len(finding.Arguments) != 0 {
					t.Fatal("uncertainty exposed native direction/value arguments")
				}
			}
			if f.Name == "SG CIDR widened" {
				wide := out.Findings[0].Arguments["internet_wide"].([]string)
				if len(wide) != 1 || wide[0] != "ipv4" {
					t.Fatal("known internet-wide ingress not labeled")
				}
			}
			if f.Order != "" && r.ReplacementOrder != f.Order {
				t.Fatal("replacement order lost")
			}
			if f.HiddenAddress && r.Address != "hidden" {
				t.Fatal("address exposed")
			}
			for path, want := range f.States {
				parts := strings.Split(path, ".")
				v := field(r, parts[0], false)
				for _, k := range parts[1:] {
					v = get(v, k)
				}
				if v.State != want {
					t.Fatalf("%s: got %+v, want %s", path, v, want)
				}
				if want == "hidden" {
					b := field(r, parts[0], true)
					for _, k := range parts[1:] {
						b = get(b, k)
					}
					if b.State != "hidden" {
						t.Fatal("only one side hidden")
					}
				}
			}
			if f.Number != "" {
				if got := out.Outputs[0].Paths[0].After.Value; got != f.Number {
					t.Fatalf("number precision: %v", got)
				}
			}
			if f.Variant != "" {
				if len(out.Coverage[0].Variants) != 1 || out.Coverage[0].Variants[0] != f.Variant {
					t.Fatal("unsupported variant claimed coverage")
				}
			}
			raw, _ := json.Marshal(out)
			if validateRegistry(raw, o.Secrets) != nil {
				t.Fatal("leak in complete result")
			}
			twice, e := analyze(f.Plan, o, s)
			if e != nil {
				t.Fatal(e)
			}
			raw2, _ := json.Marshal(twice)
			if !bytes.Equal(raw, raw2) {
				t.Fatal("nondeterministic analysis")
			}
			if len(out.Drift) > 0 && (len(out.Drift[0].Findings) != 0 || out.Summary.Actions["delete"] != 0) {
				t.Fatal("drift counted as apply action")
			}
		})
	}
}
func TestStrictJSONFailsClosed(t *testing.T) {
	for _, input := range []string{`{"a":1,"a":2}`, `{"s":"\ud800"}`, `{"s":"\udc00"}`, `{"a":1,"\u0061":2}`, `[1,]`, `{} {}`, `{"n":1e9999999}`, `{"a":"` + string([]byte{0xff}) + `"}`, strings.Repeat("[", 98) + "0" + strings.Repeat("]", 98), strings.Repeat(" ", MaxBytes+1)} {
		if _, e := strictJSON([]byte(input)); e == nil {
			t.Fatal("accepted malformed or over-limit evidence")
		}
	}
	for _, input := range []string{`null`, `{"n":9007199254740993}`, `[true,false,null]`, `{"s":"\ud83d\ude00"}`} {
		if _, e := strictJSON([]byte(input)); e != nil {
			t.Fatal(e)
		}
	}
}
func TestCanonicalDecimals(t *testing.T) {
	for input, want := range map[string]string{"9.007199254740993e15": "9007199254740993", "-0e12": "0", "1.2300": "1.23", "1e-4": "0.0001", "-12.5000": "-12.5"} {
		got, e := evidence.CanonicalNumber(input)
		if e != nil || got != want {
			t.Fatalf("%s: %s %v", input, got, e)
		}
	}
	for _, input := range []string{"1e999999", "01", "NaN", "true", "1.0.0", "1 ", "1\n"} {
		if _, e := evidence.CanonicalNumber(input); e == nil {
			t.Fatal("invalid decimal")
		}
	}
	for _, v := range []Value{{"known", "boolean", false}, {"known", "string", ""}, {"known", "object", map[string]Value{}}, {"known", "array", []Value{}}} {
		raw, _ := json.Marshal(v)
		if !bytes.Contains(raw, []byte(`"value":`)) {
			t.Fatal("zero value lost")
		}
	}
	for _, state := range []string{"hidden", "unknown", "null", "absent"} {
		raw, _ := json.Marshal(Value{state, "string", "must-not-appear"})
		if strings.Contains(string(raw), "must-not-appear") {
			t.Fatal("non-known union carried bytes")
		}
	}
}
func TestIndependentManagedSecretValidation(t *testing.T) {
	secrets := []runs.EnvSecret{{ExpectedSecret: runs.ExpectedSecret{Destination: "TF_VAR_input"}, Value: `{"leaf":"fixture-secret","n":9.1e1,"b":false}`}}
	for _, raw := range []string{`{"findings":[{"arguments":{"message":"prefix fixture-secret suffix"}}]}`, `{"fixture-secret":"safe"}`, `{"address_display":"resource[\"fixture-secret\"]"}`, `{"value":91}`, `{"value":9.10e1}`, `{"value":false}`, `{"value":"91"}`, `{"replacement_paths":[["fixture-secret"]]}`} {
		if ValidateManagedSecrets([]byte(raw), secrets) != ErrLeak {
			t.Fatal("platform accepted deliberately leaking artifact")
		}
	}
	if e := ValidateManagedSecrets([]byte(`{"state":"hidden"}`), secrets); e != nil {
		t.Fatal(e)
	}
	if ValidateManagedSecrets([]byte(`{}`), []runs.EnvSecret{{Value: ""}}) == nil {
		t.Fatal("empty managed secret accepted")
	}
	many := []runs.EnvSecret{}
	for i := 0; i < 17; i++ {
		many = append(many, runs.EnvSecret{Value: strings.Repeat(string(rune('a'+i)), 16384)})
	}
	if ValidateManagedSecrets([]byte(`{}`), many) == nil {
		t.Fatal("registry byte overflow accepted")
	}
	leaves := []string{}
	for i := 0; i < 4096; i++ {
		leaves = append(leaves, fmt.Sprintf("x%d", i))
	}
	v, _ := json.Marshal(leaves)
	if len(v) <= 16384 {
		t.Fatal("test must exercise term capacity independently")
	}
	reg := &evidence.Registry{}
	if reg.Register(leaves...) != nil || reg.Register("overflow") == nil {
		t.Fatal("registry term bound")
	}
}
func TestNativeProfilesRejectSyntheticAndMalformedSchemas(t *testing.T) {
	base := Options{Engine: "opentofu", Version: "1.13.8", ProfileID: ProfileID("opentofu"), Providers: []runs.Provider{}}
	generic := []byte(`{"format_version":"1.0","provider_schemas":{}}`)
	if _, e := checkSchema(generic, base); e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"engine", "version", "profile", "schema format", "duplicate", "provider missing", "aws synthetic", "provider version missing", "provider host"} {
		t.Run(name, func(t *testing.T) {
			o := base
			raw := generic
			switch name {
			case "engine":
				o.Engine = "other"
			case "version":
				o.Version = "1.13.7"
			case "profile":
				o.ProfileID = "unverified"
			case "schema format":
				raw = []byte(`{"format_version":"2.0","provider_schemas":{}}`)
			case "duplicate":
				raw = []byte(`{"format_version":"1.0","provider_schemas":{},"provider_schemas":{}}`)
			case "provider missing":
				o.Providers = []runs.Provider{{Address: "registry.opentofu.org/hashicorp/aws", Version: "6.41.0", Platform: "linux_amd64", Hashes: []string{"h1:fixture"}}}
			case "aws synthetic", "provider version missing", "provider host":
				address := "registry.opentofu.org/hashicorp/aws"
				version := "6.41.0"
				if name == "provider version missing" {
					version = ""
				}
				if name == "provider host" {
					address = "unapproved.invalid/hashicorp/aws"
				}
				o.Providers = []runs.Provider{{Address: address, Version: version, Platform: "linux_amd64", Hashes: []string{"h1:fixture"}}}
				raw = []byte(`{"format_version":"1.0","provider_schemas":{"` + address + `":{"provider":{"version":0,"block":{}},"resource_schemas":{}}}}`)
			}
			if _, e := checkSchema(raw, o); e == nil {
				t.Fatal("unverified profile accepted")
			}
		})
	}
	for _, raw := range []string{`{"format_version":"1.0","provider_schemas":null}`, `{"format_version":"1.0","provider_schemas":[]}`} {
		if _, e := checkSchema([]byte(raw), base); e == nil {
			t.Fatal("schema shape accepted")
		}
	}
}
func TestGenericRenderingAfterNativeValidation(t *testing.T) {
	raw := []byte(`{"format_version":"1.2","terraform_version":"1.13.8","planned_values":{},"configuration":{},"resource_changes":[{"address":"other_item.fixture","mode":"managed","name":"fixture","type":"other_item","provider_name":"registry.opentofu.org/example/other","change":{"actions":["delete"],"before":{"value":9007199254740993},"after":null,"after_unknown":{},"before_sensitive":{},"after_sensitive":{}}}]}`)
	schema := []byte(`{"format_version":"1.0","provider_schemas":{"registry.opentofu.org/example/other":{"provider":{"version":0,"block":{}},"resource_schemas":{"other_item":{"version":0,"block":{"attributes":{"value":{"type":"number","optional":true}}}}}}}}`)
	o := fixtureOptions(t, raw)
	o.Providers = []runs.Provider{{Address: "registry.opentofu.org/example/other", Version: "2.0.0", Platform: "linux_amd64", Hashes: []string{"h1:fixture"}}}
	out, e := Analyze(raw, schema, o)
	if e != nil {
		t.Fatal(e)
	}
	if len(out.Findings) != 1 || out.Findings[0].Rule != "CF-ACTION-DELETE" || out.Coverage[0].Status != "generic_only" {
		t.Fatal("generic coverage misrepresented")
	}
	// The independent boundary has the same strict profile binding and does not
	// make a successful plan ready or install itself into production.
	s := runs.ExecutionSpec{Engine: runs.Runtime{Name: o.Engine, Version: o.Version, Arch: "amd64"}, RuleVersion: RuleVersion, ProfileID: o.ProfileID}
	m := runs.Manifest{ReviewedInputs: runs.ReviewedInputs{Engine: runs.ManifestEngine{Name: o.Engine, Version: o.Version, Arch: "amd64"}, Rules: RuleVersion, Profile: o.ProfileID}, Providers: o.Providers, Lock: runs.ProviderLock{Present: true, Hash: strings.Repeat("a", 64), Size: 1}}
	if e = (Verifier{}).VerifySchema(context.Background(), s, m, schema); e != nil {
		t.Fatal(e)
	}
	m.Profile = "mismatch"
	if (Verifier{}).VerifySchema(context.Background(), s, m, schema) == nil {
		t.Fatal("manifest/spec mismatch")
	}
	raw = bytes.Replace(raw, []byte(`"delete"`), []byte(`"forget"`), 1)
	if out, e = Analyze(raw, schema, o); e == nil || out != nil {
		t.Fatal("unknown native action received generic rendering")
	}
}
func TestDeterministicResourceOrderingAndIDs(t *testing.T) {
	s := fixtureSchema(t)
	f := loadFixtures(t)[0]
	v, _ := strictJSON(f.Plan)
	m := obj(v)
	r := obj(list(m["resource_changes"])[0])
	r2 := map[string]any{}
	for k, v := range r {
		r2[k] = v
	}
	r2["address"] = "aws_db_instance.alpha"
	m["resource_changes"] = []any{r, r2}
	raw, _ := json.Marshal(m)
	o := fixtureOptions(t, raw)
	o.ResourceIDs["aws_db_instance.alpha"] = outputID
	a, e := analyze(raw, o, s)
	if e != nil {
		t.Fatal(e)
	}
	m["resource_changes"] = []any{r2, r}
	raw, _ = json.Marshal(m)
	b, e := analyze(raw, o, s)
	if e != nil {
		t.Fatal(e)
	}
	if digest(a) != digest(b) {
		t.Fatal("input resource order changed stable output")
	}
	if a.Resources[0].Address != "aws_db_instance.alpha" {
		t.Fatal("not sorted")
	}
	reg := &evidence.Registry{}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = reg.Register("parallel-private")
			if !reg.Contains("prefix-parallel-private") {
				t.Error("registry race")
			}
		}()
	}
	wg.Wait()
}
func TestAtomBoundsAndSingleCover(t *testing.T) {
	a := []atom{}
	for i := 0; i < 4097; i++ {
		a = append(a, atom{"6", "sg", fmt.Sprint(i), 443, 443})
	}
	if len(mergeAtoms(a)) != 4097 {
		t.Fatal("silent truncation")
	}
	// Each old /25 is insufficient alone to cover the new /24.
	n := atom{"6", "cidr", "10.0.0.0/24", 443, 443}
	if covers(atom{"6", "cidr", "10.0.0.0/25", 443, 443}, n) {
		t.Fatal("false single-cover claim")
	}
	// All-protocol and ICMP wildcard handling do not infer TCP meaning for ESP.
	if !covers(atom{"all", "cidr", "0.0.0.0/0", 0, 0}, n) || covers(atom{"50", "sg", "peer", 0, 0}, atom{"6", "sg", "peer", 443, 443}) {
		t.Fatal("protocol coverage")
	}
}
func TestCompatibilityProvenance(t *testing.T) {
	b, e := os.ReadFile("../testdata/compatibility.json")
	if e != nil {
		t.Fatal(e)
	}
	var data struct {
		Company  string `json:"company_compatibility"`
		Profiles []struct {
			Engine  string `json:"engine"`
			Version string `json:"engine_version"`
			Schema  string `json:"schema_provider_canonical_sha256"`
		} `json:"profiles"`
	}
	if json.Unmarshal(b, &data) != nil {
		t.Fatal("provenance")
	}
	if data.Company != "not_assessed" || len(data.Profiles) != 2 {
		t.Fatal("unsupported company claim")
	}
	engines := []string{}
	for _, p := range data.Profiles {
		want := profiles[p.Engine]
		if want.version != p.Version || want.schema != p.Schema {
			t.Fatal("profile/provenance mismatch")
		}
		engines = append(engines, p.Engine)
	}
	sort.Strings(engines)
	if strings.Join(engines, ",") != "opentofu,terraform" {
		t.Fatal("missing engine")
	}
}

func TestSensitivityShapeAndReplacementPaths(t *testing.T) {
	reg := &evidence.Registry{}
	if reg.Register("private-key") != nil {
		t.Fatal("registration")
	}
	before := map[string]any{"rows": []any{map[string]any{"private": "old", "safe": "visible"}}, "sibling": "visible"}
	after := map[string]any{"rows": []any{map[string]any{"private": "new", "safe": "visible"}, map[string]any{"private": "more"}}, "sibling": "visible"}
	b, a := normalizePair(before, true, after, true, map[string]any{"rows": []any{map[string]any{"private": true}}}, false, false, reg)
	if get(b, "rows").State != "hidden" || get(a, "rows").State != "hidden" || get(a, "sibling").State != "known" {
		t.Fatal("ambiguous collection sensitivity lost parent/sibling boundary")
	}
	for _, p := range [][]any{{"rows", "private"}, {"private-key"}} {
		safe := safePath(p, b, a, reg)
		raw, _ := json.Marshal(safe)
		if bytes.Contains(raw, []byte("private-key")) || bytes.Contains(raw, []byte(`"private"`)) {
			t.Fatal("unsafe replacement path")
		}
	}
}
func TestRequiredUnknownSchemaAndAtomBounds(t *testing.T) {
	f := loadFixtures(t)[0]
	v, _ := strictJSON(f.Plan)
	m := obj(v)
	rc := obj(list(m["resource_changes"])[0])
	obj(rc["change"])["after_unknown"] = map[string]any{"unsupported_attribute": true}
	raw, _ := json.Marshal(m)
	o := fixtureOptions(t, raw)
	if out, e := analyze(raw, o, fixtureSchema(t)); e == nil || out != nil {
		t.Fatal("unknown marker bypassed required schema")
	}
	vals := []Value{}
	for i := 0; i < 4097; i++ {
		vals = append(vals, Value{"known", "string", fmt.Sprintf("sg-%d", i)})
	}
	entry := Value{"known", "object", map[string]Value{
		"protocol": {"known", "string", "tcp"}, "from_port": {"known", "number", "443"}, "to_port": {"known", "number", "443"},
		"cidr_blocks": {"known", "array", []Value{}}, "ipv6_cidr_blocks": {"known", "array", []Value{}}, "prefix_list_ids": {"known", "array", []Value{}}, "security_groups": {"known", "array", vals}, "self": {"known", "boolean", false},
	}}
	r := Resource{Type: "aws_security_group", Actions: []string{"update"}, Paths: []PathChange{{[]any{"ingress"}, Value{"known", "array", []Value{entry}}, Value{"known", "array", []Value{entry}}}}}
	if _, _, e := ingressAtoms(r, true); e == nil {
		t.Fatal("excessive atom count accepted")
	}
	// Expansion is bounded even when thousands of redundant peers would merge.
	duplicates := make([]Value, 4097)
	for i := range duplicates {
		duplicates[i] = Value{"known", "string", "sg-same"}
	}
	entry.Value.(map[string]Value)["security_groups"] = Value{"known", "array", duplicates}
	if _, _, e := ingressAtoms(r, true); e == nil {
		t.Fatal("duplicate atom expansion silently truncated")
	}
}
func TestGenericNestedSchemaSensitivityAndValidation(t *testing.T) {
	address := "registry.opentofu.org/example/nested"
	schema := []byte(`{"format_version":"1.0","provider_schemas":{"registry.opentofu.org/example/nested":{"provider":{"version":0,"block":{}},"resource_schemas":{"nested_item":{"version":0,"block":{"attributes":{"items":{"nested_type":{"nesting_mode":"list","attributes":{"private":{"type":"string","sensitive":true},"safe":{"type":"string"}}},"optional":true}}}}}}}}`)
	raw := []byte(`{"format_version":"1.2","terraform_version":"1.13.8","planned_values":{},"configuration":{},"resource_changes":[{"address":"nested_item.fixture","name":"fixture","type":"nested_item","mode":"managed","provider_name":"registry.opentofu.org/example/nested","change":{"actions":["update"],"before":{"items":[{"private":"old","safe":"safe"}]},"after":{"items":[{"private":"new","safe":"safe"}]},"after_unknown":{},"before_sensitive":{},"after_sensitive":{}}}]}`)
	o := fixtureOptions(t, raw)
	o.Providers = []runs.Provider{{Address: address, Version: "1.0.0", Platform: "linux_amd64", Hashes: []string{"h1:fixture"}}}
	out, e := Analyze(raw, schema, o)
	if e != nil {
		t.Fatal(e)
	}
	items := field(out.Resources[0], "items", false)
	if items.State != "known" || get(items.Value.([]Value)[0], "private").State != "hidden" || get(items.Value.([]Value)[0], "safe").State != "known" {
		t.Fatal("ordered nested list lost sensitivity or safe siblings")
	}
	bad := bytes.Replace(raw, []byte(`"private":"new"`), []byte(`"private":2`), 1)
	if out, e := Analyze(bad, schema, o); e == nil || out != nil {
		t.Fatal("generic nested shape accepted invalid scalar")
	}
	malformed := bytes.Replace(schema, []byte(`"nesting_mode":"list"`), []byte(`"nesting_mode":"invalid"`), 1)
	if out, e := Analyze(raw, malformed, o); e == nil || out != nil {
		t.Fatal("malformed nested profile accepted")
	}
}
func TestRawPermissionsRemainProtected(t *testing.T) {
	for _, kind := range []string{"raw_plan_json", "provider_schema", "normalized_changes", "findings"} {
		if !evidence.Raw(kind) {
			t.Fatal("unverified normalized data gained ordinary read permission")
		}
	}
}

func TestEscapedSecretIdentifierAndArgumentProtection(t *testing.T) {
	secret := "synthetic-\"quoted\\private"
	reg := &evidence.Registry{}
	if reg.Register(secret) != nil {
		t.Fatal("registration")
	}
	address := "aws_db_instance.fixture[" + strconv.Quote(secret) + "]"
	if !containsKnown(reg, address) {
		t.Fatal("escaped known identifier evaded matching")
	}
	f := loadFixtures(t)[0]
	v, _ := strictJSON(f.Plan)
	m := obj(v)
	rc := obj(list(m["resource_changes"])[0])
	rc["address"] = address
	raw, _ := json.Marshal(m)
	o := fixtureOptions(t, raw)
	o.Secrets = reg
	out, e := analyze(raw, o, fixtureSchema(t))
	if e != nil {
		t.Fatal(e)
	}
	if out.Resources[0].Address != "hidden" {
		t.Fatal("escaped identifier was exposed")
	}
	leak, _ := json.Marshal(map[string]any{"findings": []any{map[string]any{"arguments": map[string]any{"address": address}}}})
	if ValidateManagedSecrets(leak, []runs.EnvSecret{{Value: secret}}) != ErrLeak {
		t.Fatal("independent validator accepted escaped identifier")
	}
}

func TestKnownSecretCollectionReorderingHidesBothLogicalSides(t *testing.T) {
	reg := &evidence.Registry{}
	if reg.Register("private-before") != nil {
		t.Fatal("registration")
	}
	before := map[string]any{"rows": []any{map[string]any{"id": "A", "value": "private-before"}, map[string]any{"id": "B", "value": "safe"}}, "sibling": "visible"}
	after := map[string]any{"rows": []any{map[string]any{"id": "B", "value": "safe"}, map[string]any{"id": "A", "value": "changed-after"}}, "sibling": "visible"}
	b, a := normalizePair(before, true, after, true, false, false, false, reg)
	if get(b, "rows").State != "hidden" || get(a, "rows").State != "hidden" || get(a, "sibling").State != "known" {
		t.Fatal("index pairing exposed another logical side or hid safe sibling")
	}
}

func TestSensitiveNullSidesAndShapeChanges(t *testing.T) {
	reg := &evidence.Registry{}
	b, a := normalizePair(map[string]any{"private": "value", "safe": "safe"}, true, "other-shape", true, map[string]any{"private": true}, false, false, reg)
	if b.State != "hidden" || a.State != "hidden" {
		t.Fatal("sensitive path crossed a shape change without parent masking")
	}
	s := fixtureSchema(t)
	f := loadFixtures(t)[0]
	v, _ := strictJSON(f.Plan)
	m := obj(v)
	rc := obj(list(m["resource_changes"])[0])
	c := obj(rc["change"])
	c["actions"] = []any{"delete"}
	c["before"] = map[string]any{"password": "synthetic-password", "identifier": "visible"}
	c["after"] = nil
	raw, _ := json.Marshal(m)
	o := fixtureOptions(t, raw)
	out, e := analyze(raw, o, s)
	if e != nil {
		t.Fatal(e)
	}
	r := out.Resources[0]
	if field(r, "password", true).State != "hidden" || field(r, "password", false).State != "hidden" || field(r, "identifier", true).State != "known" {
		t.Fatal("deletion lost bilateral sensitivity or safe sibling")
	}
	c["actions"] = []any{"create"}
	c["after"] = c["before"]
	c["before"] = nil
	raw, _ = json.Marshal(m)
	out, e = analyze(raw, o, s)
	if e != nil {
		t.Fatal(e)
	}
	r = out.Resources[0]
	if field(r, "password", true).State != "hidden" || field(r, "password", false).State != "hidden" || field(r, "identifier", false).State != "known" {
		t.Fatal("creation lost bilateral sensitivity or safe sibling")
	}
}

func TestDriftCoverageDoesNotImplyDefaultVariantSupport(t *testing.T) {
	f := loadFixtures(t)[0]
	v, _ := strictJSON(f.Plan)
	m := obj(v)
	rc := obj(list(m["resource_changes"])[0])
	rc["type"] = "aws_default_route_table"
	rc["address"] = "aws_default_route_table.fixture"
	c := obj(rc["change"])
	c["before"] = map[string]any{"route": []any{}}
	c["after"] = map[string]any{"route": []any{}}
	m["resource_drift"] = m["resource_changes"]
	m["resource_changes"] = []any{}
	raw, _ := json.Marshal(m)
	o := fixtureOptions(t, raw)
	out, e := analyze(raw, o, fixtureSchema(t))
	if e != nil {
		t.Fatal(e)
	}
	if len(out.Findings) != 0 || len(out.Resources) != 0 || len(out.Coverage[0].Variants) != 1 || out.Coverage[0].Status != "partial" {
		t.Fatal("drift gained action counts or unimplemented semantic coverage")
	}
}

// Display masking must never change the validated rule dispatch or severity.
func TestMaskedResourceClassification(t *testing.T) {
	s := fixtureSchema(t)
	for _, f := range loadFixtures(t) {
		v, err := strictJSON(f.Plan)
		if err != nil {
			t.Fatal(err)
		}
		entries := list(obj(v)["resource_changes"])
		if len(entries) != 1 {
			continue
		}
		typ := str(obj(entries[0])["type"])
		if !supportedType(typ) {
			continue
		}
		t.Run(f.Name, func(t *testing.T) {
			o := fixtureOptions(t, f.Plan)
			for _, value := range f.Secrets {
				terms, err := evidence.SecretTerms(value.Value, value.JSON)
				if err != nil {
					t.Fatal(err)
				}
				if err = o.Secrets.Register(terms...); err != nil {
					t.Fatal(err)
				}
			}
			secret := typ
			if typ == "aws_db_instance" {
				secret = "db_instance"
			}
			if err := o.Secrets.Register(secret); err != nil {
				t.Fatal(err)
			}
			out, err := analyze(f.Plan, o, s)
			if f.Error {
				if err == nil || out != nil {
					t.Fatal("masked invalid evidence did not fail closed")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			r := out.Resources[0]
			if r.Type != "hidden" || r.Address != "hidden" {
				t.Fatal("matched identifiers exposed")
			}
			if len(out.Findings) != len(f.Expected) {
				t.Fatalf("masked dispatch lost findings: got %+v, want %+v", out.Findings, f.Expected)
			}
			for i, want := range f.Expected {
				got := out.Findings[i]
				if got.Rule != want.Rule || got.Severity != want.Severity || want.Message != "" && got.Message != want.Message {
					t.Fatalf("masked dispatch changed warning: got %+v, want %+v", got, want)
				}
			}
			raw, err := json.Marshal(out)
			if err != nil || bytes.Contains(raw, []byte(secret)) || validateRegistry(raw, o.Secrets) != nil {
				t.Fatal("serialized result leaked masked identifier")
			}
			if len(out.Coverage[0].Variants) != 0 {
				t.Fatal("masked supported type became an unsupported variant")
			}
		})
	}
}

func TestRetainedSanitizedRulesCannotBeOmitted(t *testing.T) {
	s := fixtureSchema(t)
	for _, f := range loadFixtures(t) {
		if f.Error || len(f.Expected) == 0 || len(f.Secrets) > 0 {
			continue
		}
		t.Run(f.Name, func(t *testing.T) {
			options := fixtureOptions(t, f.Plan)
			expected, e := analyze(f.Plan, options, s)
			if e != nil {
				t.Fatal(e)
			}
			raw, _ := json.Marshal(expected)
			var decoded Result
			if json.Unmarshal(raw, &decoded) != nil {
				t.Fatal("decode safe artifact")
			}
			if e = VerifySanitized(expected, &decoded); e != nil {
				t.Fatal("valid retained artifact rejected", e)
			}
			decoded.Resources[0].Findings = []Finding{}
			decoded.Findings = []Finding{}
			decoded.Summary.Findings = map[string]int{}
			if VerifySanitized(expected, &decoded) == nil {
				t.Fatal("omitted warnings accepted")
			}
		})
	}
}
