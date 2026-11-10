package plan

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/cosmoformer/cosmoformer/internal/runs"
)

func TestRetainedCoverageKeepsMultipleProviderLimitationsBoundToTheirIdentities(t *testing.T) {
	const aws = "registry.opentofu.org/hashicorp/aws"
	const other = "registry.opentofu.org/example/other"
	const driftID = "33333333-3333-4333-8333-333333333333"
	const workerValue = "worker-private-review-value-7351"

	var raw []byte
	for _, f := range loadFixtures(t) {
		if f.Variant == "aws_default_route_table" {
			raw = f.Plan
			break
		}
	}
	if raw == nil {
		t.Fatal("unsupported AWS default-route-table fixture missing")
	}
	v, err := strictJSON(raw)
	if err != nil {
		t.Fatal(err)
	}
	m := obj(v)
	awsDrift := list(m["resource_changes"])[0]
	generic := func(name string, unknown bool) map[string]any {
		return map[string]any{
			"address": "other_item." + name, "mode": "managed", "type": "other_item", "name": name, "provider_name": other,
			"change": map[string]any{
				"actions": []string{"update"},
				"before":  map[string]any{"label": "visible-before"}, "after": map[string]any{"label": workerValue},
				"after_unknown": map[string]any{"label": unknown}, "before_sensitive": map[string]any{}, "after_sensitive": map[string]any{},
			},
		}
	}
	m["resource_changes"] = []any{generic("planned", false)}
	m["resource_drift"] = []any{awsDrift, generic("drift", true)}
	raw, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	o := fixtureOptions(t, raw)
	o.ResourceIDs["aws_default_route_table.fixture"] = fixtureID
	o.ResourceIDs["other_item.planned"] = outputID
	o.ResourceIDs["other_item.drift"] = driftID

	// Keep the existing extracted AWS schema seam, and independently validate
	// the second provider's schema, version and resource identity before merging.
	s := fixtureSchema(t)
	otherOptions := o
	otherOptions.Providers = []runs.Provider{{Address: other, Version: "2.0.0", Platform: "linux_amd64", Hashes: []string{"zh:" + strings.Repeat("b", 64)}}}
	otherSchema, err := checkSchema([]byte(`{"format_version":"1.0","provider_schemas":{"registry.opentofu.org/example/other":{"provider":{"version":0,"block":{}},"resource_schemas":{"other_item":{"version":0,"block":{"attributes":{"label":{"type":"string","optional":true}}}}}}}}`), otherOptions)
	if err != nil {
		t.Fatal(err)
	}
	s.typed.Schemas[other] = otherSchema.typed.Schemas[other]
	s.coverage = append(s.coverage, otherSchema.coverage...)
	o.Providers = append(otherOptions.Providers, runs.Provider{Address: aws, Version: "6.41.0", Platform: "linux_amd64", Hashes: []string{"zh:" + strings.Repeat("a", 64)}})
	expected, err := analyze(raw, o, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(expected.Resources) != 1 || len(expected.Drift) != 2 || len(expected.Coverage) != 2 {
		t.Fatal("multi-provider fixture lost planned resources, drift or coverage")
	}
	awsIndex, otherIndex := -1, -1
	for i, c := range expected.Coverage {
		switch c.Provider {
		case aws:
			awsIndex = i
		case other:
			otherIndex = i
		}
	}
	if awsIndex < 0 || otherIndex < 0 {
		t.Fatal("provider identities missing")
	}
	ac, oc := expected.Coverage[awsIndex], expected.Coverage[otherIndex]
	if ac.Status != "partial" || !reflect.DeepEqual(ac.Variants, []string{"aws_default_route_table"}) || len(ac.UnknownHidden) != 0 || oc.Status != "generic_only" || len(oc.Variants) != 0 || !reflect.DeepEqual(oc.UnknownHidden, []string{driftID}) || ac.Version == oc.Version || ac.Schema == oc.Schema {
		t.Fatal("fixture must independently attribute distinct limitations to both providers", expected.Coverage)
	}
	clone := func(source *Result) *Result {
		t.Helper()
		b, err := json.Marshal(source)
		if err != nil {
			t.Fatal(err)
		}
		var actual Result
		if err := json.Unmarshal(b, &actual); err != nil {
			t.Fatal(err)
		}
		return &actual
	}
	if err := VerifySanitized(expected, clone(expected)); err != nil {
		t.Fatal("legitimate multi-provider retained analysis rejected", err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Result)
	}{
		{"provider identity swap", func(a *Result) {
			a.Coverage[awsIndex].Provider, a.Coverage[otherIndex].Provider = a.Coverage[otherIndex].Provider, a.Coverage[awsIndex].Provider
		}},
		{"provider version swap", func(a *Result) {
			a.Coverage[awsIndex].Version, a.Coverage[otherIndex].Version = a.Coverage[otherIndex].Version, a.Coverage[awsIndex].Version
		}},
		{"provider schema swap", func(a *Result) {
			a.Coverage[awsIndex].Schema, a.Coverage[otherIndex].Schema = a.Coverage[otherIndex].Schema, a.Coverage[awsIndex].Schema
		}},
		{"drift uncertainty moved to another provider", func(a *Result) {
			a.Coverage[awsIndex].UnknownHidden = []string{driftID}
			a.Coverage[otherIndex].UnknownHidden = []string{}
		}},
		{"unsupported drift variant moved to another provider", func(a *Result) {
			a.Coverage[awsIndex].Variants = []string{}
			a.Coverage[awsIndex].Status = "supported"
			a.Coverage[otherIndex].Variants = []string{"aws_default_route_table"}
			a.Coverage[otherIndex].Status = "partial"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			actual := clone(expected)
			tc.mutate(actual)
			if err := VerifySanitized(expected, actual); err == nil {
				t.Fatal("cross-provider coverage forgery accepted")
			}
		})
	}

	if err := o.Secrets.Register(workerValue); err != nil {
		t.Fatal(err)
	}
	masked, err := analyze(raw, o, s)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(masked.Coverage[awsIndex], ac) || masked.Coverage[otherIndex].Status != "generic_only" || !reflect.DeepEqual(masked.Coverage[otherIndex].UnknownHidden, []string{outputID, driftID}) {
		t.Fatal("worker-only masking must add uncertainty solely to its owning provider", masked.Coverage)
	}
	if err := VerifySanitized(expected, clone(masked)); err != nil {
		t.Fatal("legitimate worker-only masking rejected", err)
	}
	actual := clone(masked)
	actual.Coverage[awsIndex].UnknownHidden = []string{outputID}
	actual.Coverage[otherIndex].UnknownHidden = []string{driftID}
	if err := VerifySanitized(expected, actual); err == nil {
		t.Fatal("worker-only masking uncertainty attributed to another provider")
	}
}
