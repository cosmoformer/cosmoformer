package coordinator

import (
	"context"
	"slices"

	"github.com/cosmoformer/cosmoformer/internal/plan"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/jackc/pgx/v5"
)

// ApprovalInputs rechecks reviewed platform references. It grants no execution
// authority and deliberately does not borrow the original planner's rights or
// require that the worker which produced historical evidence is still online.
func ApprovalInputs(ctx context.Context, tx pgx.Tx, stack, attempt string, s runs.ExecutionSpec) error {
	if s.Validate() != nil {
		return runs.Fail("INPUTS_CHANGED")
	}
	if !plan.SupportedRuntime(s.Engine.Name, s.Engine.Version) || s.ProfileID != plan.ProfileID(s.Engine.Name) || s.RuleVersion != plan.RuleVersion {
		return runs.Fail("PROFILE_UNSUPPORTED")
	}
	var valid bool
	e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.stacks st JOIN cosmoformer.projects p ON p.id=st.project_id JOIN cosmoformer.environments en ON en.id=st.environment_id JOIN cosmoformer.repositories r ON r.id=st.repository_id JOIN cosmoformer.integrations i ON i.id=r.integration_id JOIN cosmoformer.source_subjects ss ON ss.id=$3 WHERE st.id=$1 AND st.effective_revision_id=$2 AND NOT st.archived AND st.assessment='compatible' AND NOT p.archived AND NOT en.archived AND p.approval_policy_revision=$4 AND p.minimum_approvals<=$5 AND r.id=$6 AND r.url=$7 AND r.enabled AND i.enabled AND ss.repository_id=r.id AND ss.facts_revision=$8 AND ss.facts->>'commit'=$9 AND (ss.kind='branch' AND ss.external_key=r.tracked_branch AND r.inventory_commit=$9 OR ss.kind='pr' AND ss.facts->>'state'='OPEN' AND ss.facts->>'destination'=r.tracked_branch))`, stack, s.StackRevisionID, s.Source.SubjectID, s.Review.Revision, s.Review.Minimum, s.Source.RepositoryID, s.Source.URL, s.Source.FactsRevision, s.Source.Commit).Scan(&valid)
	if e != nil {
		return e
	}
	if !valid {
		return runs.Fail("INPUTS_CHANGED")
	}
	e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.runtime_catalog WHERE id=$1 AND engine=$2 AND version=$3 AND architecture=$4 AND image_digest=$5 AND approved AND NOT withdrawn) AND EXISTS(SELECT FROM cosmoformer.worker_pools WHERE id=$6 AND enabled) AND EXISTS(SELECT FROM cosmoformer.aws_connections c JOIN cosmoformer.stacks st ON st.id=$7 WHERE c.id=$8 AND c.revision=$9 AND c.role_arn=$10 AND c.expected_account=$11 AND c.expected_bootstrap_principal=$12 AND c.session_seconds=$13 AND c.enabled AND c.project_id=st.project_id AND (c.environment_id IS NULL OR c.environment_id=st.environment_id))`, s.Engine.ID, s.Engine.Name, s.Engine.Version, s.Engine.Arch, s.Engine.ImageDigest, s.Pool.ID, stack, s.AWS.ID, s.AWS.Revision, s.AWS.RoleARN, s.AWS.Account, s.AWS.Bootstrap, s.AWS.SessionSeconds).Scan(&valid)
	if e != nil {
		return e
	}
	if !valid {
		return runs.Fail("INPUTS_CHANGED")
	}
	for _, ref := range s.Secrets {
		e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.secrets sec JOIN cosmoformer.stacks st ON st.id=$1 WHERE sec.id=$2 AND sec.semantic_revision=$3 AND sec.enabled AND (($4='org' AND sec.project_id IS NULL) OR ($4='project' AND sec.project_id=st.project_id)))`, stack, ref.ID, ref.Revision, ref.Scope).Scan(&valid)
		if e != nil {
			return e
		}
		if !valid {
			return runs.Fail("INPUTS_CHANGED")
		}
	}
	// Compare the full selected grant/credential sets, including newly introduced
	// references. IDs and semantic revisions cannot be replaced by latest values.
	rows, e := tx.Query(ctx, `SELECT g.id::text,g.kind,coalesce(g.resource_id::text,''),g.revision,coalesce(g.namespace,''),coalesce(g.resource_key,'') FROM cosmoformer.resource_grants g JOIN cosmoformer.stacks st ON st.id=$1 WHERE g.project_id=st.project_id AND (g.environment_id IS NULL OR g.environment_id=st.environment_id) AND g.enabled ORDER BY g.id`, stack)
	if e != nil {
		return e
	}
	refs := []runs.ResourceRef{}
	for rows.Next() {
		var r runs.ResourceRef
		if e = rows.Scan(&r.ID, &r.Kind, &r.ResourceID, &r.Revision, &r.Namespace, &r.Key); e != nil {
			rows.Close()
			return e
		}
		refs = append(refs, r)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	expected := slices.Clone(s.Grants)
	slices.SortFunc(expected, func(a, b runs.ResourceRef) int {
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	if runs.Digest(refs) != runs.Digest(expected) {
		return runs.Fail("INPUTS_CHANGED")
	}
	repos := []string{}
	for _, r := range refs {
		if r.Kind == "repository" {
			repos = append(repos, r.ResourceID)
		}
	}
	rows, e = tx.Query(ctx, `SELECT id::text,repository_id::text,semantic_revision FROM cosmoformer.source_credentials WHERE repository_id=ANY($1::uuid[]) AND enabled ORDER BY id`, repos)
	if e != nil {
		return e
	}
	credentials := []runs.SourceCredential{}
	for rows.Next() {
		var r runs.SourceCredential
		if e = rows.Scan(&r.ID, &r.RepositoryID, &r.Revision); e != nil {
			rows.Close()
			return e
		}
		credentials = append(credentials, r)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	if runs.Digest(credentials) != runs.Digest(s.SourceCredentials) {
		return runs.Fail("INPUTS_CHANGED")
	}
	e = tx.QueryRow(ctx, `SELECT cosmoformer.execution_binding_matches($1,$2,$3,$4,$5,$6) AND NOT EXISTS(SELECT FROM cosmoformer.recovery_cases r JOIN cosmoformer.attempts a ON a.target_id=r.target_id WHERE a.id=$1 AND r.status='open') AND EXISTS(SELECT FROM cosmoformer.plans p WHERE p.attempt_id=$1 AND p.state_epoch=cosmoformer.execution_target_epoch($1))`, attempt, s.Backend.Target.Partition, s.Backend.Target.Bucket, s.Backend.Target.Key, s.Backend.Profile, nullable(s.Backend.TableARN)).Scan(&valid)
	if e != nil {
		return e
	}
	if !valid {
		return runs.Fail("STATE_OR_RECOVERY_CHANGED")
	}
	return nil
}
