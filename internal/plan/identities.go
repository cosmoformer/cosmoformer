package plan

import (
	"github.com/cosmoformer/cosmoformer/internal/catalog"
)

// Identities contains only random public IDs, in raw evidence traversal order.
// Native names remain exclusively in protected evidence and local maps.
type Identities struct {
	Resources []string `json:"resource_ids"`
	Outputs   []string `json:"output_ids"`
}
type Analysis struct {
	Identities Identities `json:"identities"`
	Changes    *Result    `json:"changes"`
}

// Allocate binds opaque identities once to an analysis attempt. Revalidation
// uses the retained list and never hashes a native identifier into a public ID.
func Allocate(raw []byte, o Options, ids *Identities, allocate func() string) (Options, Identities, error) {
	v, e := strictJSON(raw)
	if e != nil {
		return o, Identities{}, e
	}
	m := obj(v)
	if m == nil {
		return o, Identities{}, ErrInvalid
	}
	out := Identities{Resources: []string{}, Outputs: []string{}}
	o.ResourceIDs = map[string]string{}
	o.OutputIDs = map[string]string{}
	seenIDs := map[string]bool{}
	next := func(index int, supplied []string) (string, error) {
		id := ""
		if ids != nil {
			if index >= len(supplied) {
				return "", ErrInvalid
			}
			id = supplied[index]
		} else {
			if allocate == nil {
				return "", ErrInvalid
			}
			id = allocate()
		}
		if !catalog.UUID(id) || seenIDs[id] {
			return "", ErrInvalid
		}
		seenIDs[id] = true
		return id, nil
	}
	for _, kind := range []string{"resource_changes", "resource_drift"} {
		if len(list(m[kind])) > maxResources {
			return o, out, ErrInvalid
		}
		for _, entry := range list(m[kind]) {
			name := str(obj(entry)["address"])
			if name == "" {
				return o, out, ErrInvalid
			}
			if o.ResourceIDs[name] != "" {
				continue
			}
			var supplied []string
			if ids != nil {
				supplied = ids.Resources
			}
			id, e := next(len(out.Resources), supplied)
			if e != nil {
				return o, out, e
			}
			o.ResourceIDs[name] = id
			out.Resources = append(out.Resources, id)
		}
	}
	if len(obj(m["output_changes"])) > maxResources {
		return o, out, ErrInvalid
	}
	for _, name := range keys(obj(m["output_changes"])) {
		var supplied []string
		if ids != nil {
			supplied = ids.Outputs
		}
		id, e := next(len(out.Outputs), supplied)
		if e != nil {
			return o, out, e
		}
		o.OutputIDs[name] = id
		out.Outputs = append(out.Outputs, id)
	}
	if ids != nil && (len(ids.Resources) != len(out.Resources) || len(ids.Outputs) != len(out.Outputs)) {
		return o, out, ErrInvalid
	}
	return o, out, nil
}
func (r *Result) HasChanges() bool {
	for _, set := range [][]Resource{r.Resources, r.Outputs} {
		for _, resource := range set {
			for _, a := range resource.Actions {
				if a != "no-op" && a != "read" {
					return true
				}
			}
		}
	}
	return false
}
