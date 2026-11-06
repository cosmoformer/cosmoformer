// Package access expands additive team grants. Administration never grants
// execution, approval or access to unredacted evidence.
package access

type Action string

const (
	View            Action = "view"
	Plan            Action = "plan"
	Apply           Action = "apply"
	Control         Action = "control"
	Recovery        Action = "recovery"
	Manage          Action = "manage"
	Approve         Action = "approve"
	RawEvidenceRead Action = "raw_evidence_read"
)

var roles = map[string][]Action{
	"viewer":            {View},
	"planner":           {View, Plan},
	"deployer":          {View, Plan, Apply, Control, Recovery},
	"administrator":     {Manage},
	"approver":          {Approve},
	"raw_evidence_read": {RawEvidenceRead},
}

type Grant struct {
	ProjectID     string `json:"project_id"`
	EnvironmentID string `json:"environment_id,omitempty"`
	Role          string `json:"role"`
}

func ValidRole(role string) bool { _, ok := roles[role]; return ok }

// Allows requires an exact project; an environment grant cannot authorize a
// project-wide request. Callers load and verify the organization chain first.
func Allows(grants []Grant, project, environment string, action Action) bool {
	if project == "" {
		return false
	}
	for _, grant := range grants {
		if grant.ProjectID != project || grant.EnvironmentID != "" && grant.EnvironmentID != environment {
			continue
		}
		for _, allowed := range roles[grant.Role] {
			if allowed == action {
				return true
			}
		}
	}
	return false
}
