package plan

import (
	"encoding/json"
	"reflect"
	"sort"

	"github.com/cosmoformer/cosmoformer/internal/evidence"
)

// VerifySanitized permits additional worker-known AWS/Git masks, but never
// altered actions, invented values, missing resources, or omitted risk rules.
// Native identity remains private when reclassifying the safe structural data.
func VerifySanitized(expected, actual *Result) error {
	if expected == nil || actual == nil || expected.ProfileID != actual.ProfileID || expected.RuleVersion != actual.RuleVersion {
		return ErrInvalid
	}
	for pairIndex, pair := range [][2][]Resource{{expected.Resources, actual.Resources}, {expected.Drift, actual.Drift}, {expected.Outputs, actual.Outputs}} {
		if len(pair[0]) != len(pair[1]) {
			return ErrInvalid
		}
		byID := map[string]Resource{}
		for _, r := range pair[0] {
			byID[r.ID] = r
		}
		for _, a := range pair[1] {
			e, ok := byID[a.ID]
			if !ok {
				return ErrInvalid
			}
			delete(byID, a.ID)
			if !reflect.DeepEqual(e.Actions, a.Actions) || e.Provider != a.Provider || e.ReplacementOrder != a.ReplacementOrder || a.Address != "hidden" && a.Address != e.Address || a.Type != "hidden" && a.Type != e.Type {
				return ErrInvalid
			}
			if len(a.ReplacementPaths) != len(e.ReplacementPaths) {
				return ErrInvalid
			}
			for i, p := range a.ReplacementPaths {
				if len(p) > len(e.ReplacementPaths[i]) || digest(p) != digest(e.ReplacementPaths[i][:len(p)]) {
					return ErrInvalid
				}
			}
			if len(a.Paths) == 1 && len(a.Paths[0].Path) == 0 && a.Paths[0].Before.State == "hidden" && a.Paths[0].After.State == "hidden" { // whole parent masking
			} else {
				if len(e.Paths) != len(a.Paths) {
					return ErrInvalid
				}
				for i, p := range a.Paths {
					if !reflect.DeepEqual(p.Path, e.Paths[i].Path) || !moreHidden(e.Paths[i].Before, p.Before) || !moreHidden(e.Paths[i].After, p.After) {
						return ErrInvalid
					}
				}
			}
			// Only independently validated managed resources receive semantic rules.
			// Outputs and data sources have no resource findings, including in drift.
			if pairIndex == 2 || e.classificationMode == "data" {
				if len(a.Findings) != 0 {
					return ErrInvalid
				}
				continue
			}
			check := a
			check.classificationType = e.classificationType
			check.Findings = []Finding{}
			semantic := false
			for _, c := range expected.Coverage {
				if c.Provider == e.Provider && c.Status != "generic_only" {
					semantic = true
				}
			}
			if err := classify(&check, semantic); err != nil {
				return err
			}
			// Drift is diagnostic only, including a hidden update/deletion.
			if pairIndex != 1 && digest(check.Findings) != digest(a.Findings) {
				return ErrInvalid
			}
			if pairIndex == 1 && len(a.Findings) != 0 {
				return ErrInvalid
			}
		}
	}
	if digest(expected.Notices) != digest(actual.Notices) {
		return ErrInvalid
	}
	if actual.Coverage == nil || len(expected.Coverage) != len(actual.Coverage) {
		return ErrInvalid
	}
	for i, e := range expected.Coverage {
		a := actual.Coverage[i]
		// Derive limitations from independently validated private resource types,
		// then the verified safe values. Worker-only masking may add uncertainty
		// or hide a variant label, but cannot remove its unsupported limitation.
		derived := Coverage{e.Provider, e.Version, e.Schema, "supported", []string{}, []string{}}
		if e.Status == "generic_only" {
			derived.Status = "generic_only"
		}
		for _, pair := range [][2][]Resource{{expected.Resources, actual.Resources}, {expected.Drift, actual.Drift}} {
			byID := map[string]Resource{}
			for _, r := range pair[0] {
				byID[r.ID] = r
			}
			for _, r := range pair[1] {
				if r.Provider != e.Provider {
					continue
				}
				if e.Status != "generic_only" && !supportedType(byID[r.ID].classificationType) {
					derived.Variants = append(derived.Variants, r.Type)
				}
				for _, p := range r.Paths {
					if uncertainTree(p.Before) || uncertainTree(p.After) {
						derived.UnknownHidden = append(derived.UnknownHidden, r.ID)
						break
					}
				}
			}
		}
		sort.Strings(derived.Variants)
		derived.Variants = unique(derived.Variants)
		sort.Strings(derived.UnknownHidden)
		derived.UnknownHidden = unique(derived.UnknownHidden)
		if derived.Status == "supported" && len(derived.UnknownHidden) > 0 {
			derived.Status = "valid_with_unknown_hidden"
		}
		if len(derived.Variants) > 0 {
			derived.Status = "partial"
		}
		if digest(a) != digest(derived) {
			return ErrInvalid
		}
	}
	findings := []Finding{}
	for _, r := range actual.Resources {
		findings = append(findings, r.Findings...)
	}
	sortFindings(findings, actual.Resources)
	if digest(findings) != digest(actual.Findings) {
		return ErrInvalid
	}
	actions, risks := map[string]int{}, map[string]int{}
	for _, r := range actual.Resources {
		for _, a := range r.Actions {
			actions[a]++
		}
	}
	for _, f := range actual.Findings {
		risks[f.Severity]++
	}
	if digest(actions) != digest(actual.Summary.Actions) || digest(risks) != digest(actual.Summary.Findings) {
		return ErrInvalid
	}
	return nil
}
func moreHidden(e, a Value) bool {
	if a.State == "hidden" {
		return true
	}
	if e.State != a.State || e.Type != a.Type {
		return false
	}
	if e.State != "known" {
		return true
	}
	// Decode both representations identically; worker artifact values use ordinary
	// JSON dictionaries while fresh normalization uses typed Value containers.
	eb, _ := json.Marshal(e)
	ab, _ := json.Marshal(a)
	var ev, av any
	if json.Unmarshal(eb, &ev) != nil || json.Unmarshal(ab, &av) != nil {
		return false
	}
	return hiddenJSON(ev, av)
}
func hiddenJSON(e, a any) bool {
	am, aMap := a.(map[string]any)
	em, eMap := e.(map[string]any)
	if aMap && am["state"] == "hidden" && len(am) == 1 {
		return true
	}
	if aMap || eMap {
		if !aMap || !eMap || len(am) != len(em) {
			return false
		}
		for k, v := range em {
			other, ok := am[k]
			if !ok || !hiddenJSON(v, other) {
				return false
			}
		}
		return true
	}
	aa, aArray := a.([]any)
	ea, eArray := e.([]any)
	if aArray || eArray {
		if !aArray || !eArray || len(aa) != len(ea) {
			return false
		}
		for i, v := range ea {
			if !hiddenJSON(v, aa[i]) {
				return false
			}
		}
		return true
	}
	return reflect.DeepEqual(e, a)
}

// DecodeValue restores typed safe containers for classification after custody
// JSON decoding. No native values or sensitive identifiers are added.
func (v *Value) UnmarshalJSON(raw []byte) error {
	var x struct {
		State string          `json:"state"`
		Type  string          `json:"type"`
		Value json.RawMessage `json:"value"`
	}
	if json.Unmarshal(raw, &x) != nil {
		return ErrInvalid
	}
	v.State = x.State
	v.Type = x.Type
	if x.State != "known" {
		if x.State != "hidden" && x.State != "unknown" && x.State != "null" && x.State != "absent" {
			return ErrInvalid
		}
		if x.Type != "" || len(x.Value) > 0 {
			return ErrInvalid
		}
		return nil
	}
	switch x.Type {
	case "object":
		var m map[string]Value
		if json.Unmarshal(x.Value, &m) != nil || m == nil {
			return ErrInvalid
		}
		v.Value = m
	case "array":
		var a []Value
		if json.Unmarshal(x.Value, &a) != nil || a == nil {
			return ErrInvalid
		}
		v.Value = a
	case "string", "number":
		var s string
		if json.Unmarshal(x.Value, &s) != nil {
			return ErrInvalid
		}
		if x.Type == "number" {
			n, e := evidence.CanonicalNumber(s)
			if e != nil || n != s {
				return ErrInvalid
			}
		}
		v.Value = s
	case "boolean":
		var b bool
		if json.Unmarshal(x.Value, &b) != nil {
			return ErrInvalid
		}
		v.Value = b
	default:
		return ErrInvalid
	}
	return nil
}
