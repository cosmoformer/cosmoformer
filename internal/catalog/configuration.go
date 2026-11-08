package catalog

import "errors"

var ErrLimits = errors.New("declaration exceeds current administrator limits")

type Limits struct {
	ProjectID    string         `json:"project_id"`
	Environments []string       `json:"environments"`
	Branch       string         `json:"branch"`
	Pools        []string       `json:"pools"`
	Engines      []Engine       `json:"engines"`
	Secrets      []SecretSource `json:"secrets"`
	Minimum      int            `json:"minimum"`
	AllowPRApply bool           `json:"allow_pr_apply"`
}

func (l Limits) Resolve(s Stack) (Stack, error) {
	contains := func(a []string, v string) bool {
		for _, x := range a {
			if x == v {
				return true
			}
		}
		return false
	}
	if s.Validate() != nil || s.ProjectID != l.ProjectID || s.TrackedBranch != l.Branch || !contains(l.Environments, s.EnvironmentID) || !contains(l.Pools, s.Worker.ID) || l.Minimum < 1 || l.Minimum > 100 {
		return s, ErrLimits
	}
	engine := false
	for _, e := range l.Engines {
		if e == s.Engine {
			engine = true
		}
	}
	if !engine {
		return s, ErrLimits
	}
	for _, r := range s.Secrets {
		found := false
		for _, a := range l.Secrets {
			if a == r.Source {
				found = true
			}
		}
		if !found {
			return s, ErrLimits
		}
	}
	s.Approvals.Minimum = max(s.Approvals.Minimum, l.Minimum)
	s.AllowPRApply = s.AllowPRApply && l.AllowPRApply
	return s, nil
}
