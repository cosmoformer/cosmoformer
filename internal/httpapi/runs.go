package httpapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/cosmoformer/cosmoformer/internal/access"
	"github.com/cosmoformer/cosmoformer/internal/bitbucket"
	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/coordinator"
	"github.com/cosmoformer/cosmoformer/internal/plan"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
)

type RepoFacts struct {
	URL, ExternalID, Branch, ProjectID, Inventory, IntegrationID string
	Revision                                                     int64
}

func repoFacts(ctx context.Context, tx pgx.Tx, id string) (RepoFacts, error) {
	var r RepoFacts
	e := tx.QueryRow(ctx, `SELECT url,external_id,tracked_branch,project_id::text,coalesce(inventory_commit,''),revision,integration_id::text FROM cosmoformer.repositories WHERE id=$1 AND enabled AND EXISTS(SELECT FROM cosmoformer.integrations i WHERE i.id=integration_id AND i.enabled)`, id).Scan(&r.URL, &r.ExternalID, &r.Branch, &r.ProjectID, &r.Inventory, &r.Revision, &r.IntegrationID)
	return r, e
}

type SyncInput struct {
	OrgIDPath
	Body struct {
		Revision int64 `json:"expected_revision" minimum:"1"`
	}
}
type SyncOutput struct {
	Body struct {
		RepositoryID string   `json:"repository_id" format:"uuid"`
		Commit       string   `json:"commit"`
		StackIDs     []string `json:"stack_ids"`
	}
}
type PlanInput struct {
	OrgIDPath
	Body struct {
		RecoveryCase     string `json:"recovery_case_id,omitempty" format:"uuid"`
		RecoveryRevision int64  `json:"expected_evidence_revision,omitempty" minimum:"1" maximum:"9007199254740991"`
		Intent           string `json:"intent,omitempty" enum:"source_check,native_plan,recovery_plan"`
		Source           struct {
			Kind string `json:"kind" enum:"branch,pr"`
			PR   int    `json:"pr_number,omitempty" minimum:"1"`
		} `json:"source"`
		Priority string `json:"priority" enum:"low,medium,high"`
	}
}
type ReceiptOutput struct {
	Body struct {
		RequestID   string `json:"request_id"`
		SubjectID   string `json:"subject_id" format:"uuid"`
		Revision    int64  `json:"revision"`
		Status      string `json:"status"`
		OperationID string `json:"operation_id" format:"uuid"`
	}
}
type StacksOutput struct {
	Body struct {
		Stacks []StackPublic `json:"stacks" nullable:"false"`
		Next   string        `json:"next_cursor,omitempty"`
	}
}
type StackPublic struct {
	ProjectName     string `json:"project_name"`
	EnvironmentName string `json:"environment_name"`
	Repository      string `json:"repository_id" format:"uuid"`
	SourceURL       string `json:"source_url"`
	Branch          string `json:"tracked_branch"`
	Root            string `json:"root"`
	Assessment      string `json:"assessment" enum:"not_assessed,compatible,incompatible"`
	CheckReason     string `json:"check_reason"`
	PlanReason      string `json:"plan_reason"`
	CanReview       bool   `json:"can_review"`

	ID            string `json:"id" format:"uuid"`
	Name          string `json:"name"`
	ProjectID     string `json:"project_id" format:"uuid"`
	EnvironmentID string `json:"environment_id" format:"uuid"`
	Commit        string `json:"commit"`
	Archived      bool   `json:"archived"`
	CanPlan       bool   `json:"can_plan"`
	CanNativePlan bool   `json:"can_native_plan"`
}

func (a *Server) registerSourceActions(api huma.API) {
	register(api, "POST", "/organizations/{org}/repositories/{id}/sync", "sync-repository", func(ctx context.Context, in *SyncInput) (*SyncOutput, error) {
		var r RepoFacts
		var observation coordinator.Observation
		var replay *SyncOutput
		var found bool
		if e := a.tx(ctx, in.Org, func(tx pgx.Tx, actor store.Actor) error {
			if e := orgAdmin(ctx, tx, actor, in.Org); e != nil {
				return e
			}
			var e error
			replay, found, e = browserReceipt[SyncOutput](ctx, tx, actor)
			if found || e != nil {
				return e
			}
			r, e = repoFacts(ctx, tx, in.ID)
			if e != nil {
				return e
			}
			if r.Revision != in.Body.Revision {
				return store.ErrConflict
			}
			observation, e = a.Coordinator.BeginObservation(ctx, tx, in.Org, in.ID, "branch", r.Branch)
			return e
		}); e != nil {
			return nil, e
		}
		if found {
			return replay, nil
		}
		token, e := a.apiToken(ctx, in.Org, r.IntegrationID)
		if e != nil {
			return nil, fail(ctx, e)
		}
		facts, e := a.Bitbucket.Read(ctx, r.URL, r.ExternalID, r.Branch, 0, token)
		contextChanged := errors.Is(e, bitbucket.ErrContextChanged)
		if e != nil && !contextChanged {
			return nil, fail(ctx, e)
		}
		facts.InventoryRevision = r.Revision
		if r.Inventory != facts.Commit {
			facts.InventoryRevision++
		}
		factsRevision, e := a.acceptSourceFacts(ctx, in.Org, in.ID, r, observation, facts)
		if e != nil {
			return nil, fail(ctx, e)
		}
		if contextChanged {
			return nil, problem(ctx, 409, "The repository source context changed and is unsupported. Review its connection before requesting work.")
		}
		raw, e := a.Bitbucket.Manifest(ctx, r.URL, facts.Commit, token)
		if e != nil {
			return nil, fail(ctx, e)
		}
		manifest, e := catalog.Parse(raw)
		if e != nil {
			return nil, problem(ctx, 422, "The tracked manifest is invalid or unsupported.")
		}
		return mutate(a, ctx, in.Org, false, a.adminGuard(ctx, in.Org), func(tx pgx.Tx, actor store.Actor) (*SyncOutput, error) {
			current, e := repoFacts(ctx, tx, in.ID)
			if e != nil {
				return nil, e
			}
			if current.Revision != r.Revision {
				return nil, store.ErrConflict
			}
			if e = a.Coordinator.RequireObservation(ctx, tx, observation, factsRevision); e != nil {
				return nil, e
			}
			out, e := a.importInventory(ctx, tx, in.Org, in.ID, r, observation, facts, manifest)
			if e != nil {
				return nil, e
			}
			return out, store.Audit(ctx, tx, in.Org, actor.ID, "repository.sync", in.ID)
		})
	})
	register(api, "POST", "/organizations/{org}/stacks/{id}/plans", "request-preflight", func(ctx context.Context, in *PlanInput) (*ReceiptOutput, error) {
		if in.Body.Source.Kind == "branch" && in.Body.Source.PR != 0 || in.Body.Source.Kind == "pr" && in.Body.Source.PR < 1 {
			return nil, problem(ctx, 422, "Select a tracked branch or a pull request.")
		}
		var repo, project, env, rev string
		var r RepoFacts
		var effective runs.Effective
		var observation coordinator.Observation
		var replay *ReceiptOutput
		var found bool
		authorize := func(tx pgx.Tx, actor store.Actor) error {
			var e error
			e = tx.QueryRow(ctx, `SELECT s.repository_id::text,s.project_id::text,s.environment_id::text,s.effective_revision_id::text,sr.effective_config FROM cosmoformer.stacks s JOIN cosmoformer.stack_revisions sr ON sr.id=s.effective_revision_id WHERE s.id=$1 AND NOT s.archived`, in.ID).Scan(&repo, &project, &env, &rev, newJSON(&effective))
			if e != nil {
				return store.ErrNotFound
			}
			g, e := store.GrantsFor(ctx, tx, actor.ID)
			if e != nil {
				return e
			}
			if !access.Allows(g, project, env, access.Plan) {
				return store.ErrForbidden
			}
			return nil
		}
		if e := a.tx(ctx, in.Org, func(tx pgx.Tx, actor store.Actor) error {
			if e := authorize(tx, actor); e != nil {
				return e
			}
			var e error
			replay, found, e = browserReceipt[ReceiptOutput](ctx, tx, actor)
			if found || e != nil {
				return e
			}
			r, e = repoFacts(ctx, tx, repo)
			if e != nil {
				return e
			}
			key := r.Branch
			if in.Body.Source.Kind == "pr" {
				key = fmt.Sprint(in.Body.Source.PR)
			}
			observation, e = a.Coordinator.BeginObservation(ctx, tx, in.Org, repo, in.Body.Source.Kind, key)
			return e
		}); e != nil {
			return nil, e
		}
		if found {
			return replay, nil
		}
		token, e := a.apiToken(ctx, in.Org, r.IntegrationID)
		if e != nil {
			return nil, fail(ctx, e)
		}
		facts, e := a.Bitbucket.Read(ctx, r.URL, r.ExternalID, r.Branch, in.Body.Source.PR, token)
		contextChanged := errors.Is(e, bitbucket.ErrContextChanged)
		if e != nil && !contextChanged {
			return nil, fail(ctx, e)
		}
		facts.InventoryRevision = r.Revision
		factsRevision, e := a.acceptSourceFacts(ctx, in.Org, repo, r, observation, facts)
		if e != nil {
			return nil, fail(ctx, e)
		}
		if contextChanged {
			return nil, problem(ctx, 409, "The repository source context changed and is unsupported. Review its connection before requesting work.")
		}
		if in.Body.Source.Kind == "branch" && facts.Commit != r.Inventory {
			return nil, problem(ctx, 409, "Sync the verified tracked inventory before requesting its preflight.")
		}
		if in.Body.Source.Kind == "pr" && facts.State != "OPEN" {
			return nil, problem(ctx, 409, "The pull request is no longer open.")
		}
		if in.Body.Source.Kind == "pr" && facts.Destination != r.Branch {
			return nil, problem(ctx, 409, "The pull request no longer targets the tracked branch.")
		}
		// PR source is validated against the accepted tracked declaration. New IDs
		// and changes to administrator-selected resource identity cannot self-enable.
		raw, e := a.Bitbucket.Manifest(ctx, r.URL, facts.Commit, token)
		if e != nil {
			return nil, fail(ctx, e)
		}
		manifest, e := catalog.Parse(raw)
		if e != nil {
			return nil, problem(ctx, 422, "The source manifest is invalid.")
		}
		var proposed *catalog.Stack
		for _, s := range manifest.Stacks {
			if s.ID == effective.Declaration.ID {
				copy := s
				proposed = &copy
			}
		}
		if proposed == nil {
			return nil, problem(ctx, 409, "This declaration is absent from the selected source.")
		}
		old := effective.Declaration
		if proposed.ProjectID != old.ProjectID || proposed.EnvironmentID != old.EnvironmentID || proposed.Worker.ID != old.Worker.ID || proposed.Root != old.Root || proposed.Workspace != old.Workspace || proposed.BackendOwnerAccountID != old.BackendOwnerAccountID || proposed.Engine != old.Engine || proposed.TrackedBranch != old.TrackedBranch {
			return nil, problem(ctx, 409, "Merge and sync resource identity changes before requesting preflight.")
		}
		proposed.Approvals.Minimum = max(proposed.Approvals.Minimum, old.Approvals.Minimum)
		proposed.AllowPRApply = proposed.AllowPRApply && old.AllowPRApply
		return mutate(a, ctx, in.Org, false, authorize, func(tx pgx.Tx, actor store.Actor) (*ReceiptOutput, error) {
			current, e := repoFacts(ctx, tx, repo)
			if e != nil {
				return nil, e
			}
			if current.Revision != r.Revision {
				return nil, store.ErrConflict
			}
			if e = a.Coordinator.RequireObservation(ctx, tx, observation, factsRevision); e != nil {
				return nil, e
			}
			if in.Body.Intent != "source_check" && in.Body.Intent != "" {
				var compatible bool
				if e := tx.QueryRow(ctx, `SELECT assessment='compatible' FROM cosmoformer.stacks WHERE id=$1`, in.ID).Scan(&compatible); e != nil {
					return nil, e
				}
				if !compatible {
					return nil, problem(ctx, 409, "Execution compatibility has not been assessed for this stack.")
				}
			}
			eff, e := coordinator.BuildSpec(ctx, tx, *proposed, repo)
			if e != nil {
				return nil, e
			}
			if in.Body.Intent != "source_check" && in.Body.Intent != "" {
				if a.Evidence == nil {
					return nil, problem(ctx, 503, "Protected planning evidence storage is unavailable.")
				}
				if !plan.SupportedRuntime(eff.Runtime.Name, eff.Runtime.Version) {
					return nil, problem(ctx, 409, "This runtime has no supported planning profile.")
				}
				var fixedSession bool
				if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.aws_connections WHERE id=$1 AND revision=$2 AND expected_bootstrap_principal IS NOT NULL AND session_seconds=3600 AND enabled)`, eff.AWS.ID, eff.AWS.Revision).Scan(&fixedSession); e != nil {
					return nil, e
				}
				if !fixedSession {
					return nil, problem(ctx, 409, "The fixed execution session metadata is incomplete.")
				}
			}
			spec := coordinator.Spec(eff, rev, repo, r.URL, facts.Commit, observation.SubjectID, factsRevision)
			var recoveryID any
			var recoveryRevision any
			if in.Body.Intent == "recovery_plan" {
				rc, e := recoveryFor(ctx, tx, actor, in.Body.RecoveryCase, access.Plan)
				if e != nil {
					return nil, e
				}
				if rc.Stack != in.ID || !rc.CanPlan || rc.EvidenceRevision != in.Body.RecoveryRevision {
					return nil, store.ErrConflict
				}
				spec.Recovery = &runs.Provenance{Kind: "recovery", Case: rc.ID, Revision: rc.EvidenceRevision}
				recoveryID = rc.ID
				recoveryRevision = rc.EvidenceRevision
			} else {
				if in.Body.RecoveryCase != "" || in.Body.RecoveryRevision != 0 {
					return nil, store.ErrConflict
				}
				var held bool
				if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.recovery_cases WHERE stack_id=$1 AND status='open')`, in.ID).Scan(&held); e != nil {
					return nil, e
				}
				if held && in.Body.Intent == "native_plan" {
					return nil, approvalProblem(ctx, "RECOVERY_REQUIRED")
				}
			}
			// Coalesce only older unclaimed source work; never overwrite active attempts.
			if _, e = tx.Exec(ctx, `UPDATE cosmoformer.operations SET status='superseded',revision=revision+1 WHERE stack_id=$1 AND kind='plan' AND status='queued' AND native_planning=$3 AND NOT EXISTS(SELECT FROM cosmoformer.attempts WHERE operation_id=cosmoformer.operations.id AND closed_at IS NULL) AND recovery_case_id IS NOT DISTINCT FROM $4::uuid AND run_id IN(SELECT id FROM cosmoformer.runs WHERE source_subject_id=$2)`, in.ID, observation.SubjectID, in.Body.Intent != "source_check" && in.Body.Intent != "", recoveryID); e != nil {
				return nil, e
			}
			if _, e = tx.Exec(ctx, `UPDATE cosmoformer.work_items SET status='retired' WHERE status='queued' AND operation_id IN(SELECT id FROM cosmoformer.operations WHERE stack_id=$1 AND status='superseded')`, in.ID); e != nil {
				return nil, e
			}
			run, op := runs.ID(), runs.ID()
			purpose := "tracked"
			if in.Body.Source.Kind == "pr" {
				purpose = "pr_preview"
			}
			if in.Body.Intent == "recovery_plan" {
				purpose = "recovery"
			}
			if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.runs(id,org_id,stack_id,source_subject_id,source_facts_revision,source_commit,purpose,requester_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, run, in.Org, in.ID, observation.SubjectID, factsRevision, facts.Commit, purpose, actor.ID); e != nil {
				return nil, e
			}
			if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.operations(id,org_id,run_id,stack_id,kind,status,priority,accepted_at,ordering_at,requester_id,native_planning,recovery_case_id,recovery_evidence_revision) VALUES($1,$2,$3,$4,'plan','queued',$5,clock_timestamp(),clock_timestamp(),$6,$7,$8,$9)`, op, in.Org, run, in.ID, in.Body.Priority, actor.ID, in.Body.Intent != "source_check" && in.Body.Intent != "", recoveryID, recoveryRevision); e != nil {
				return nil, e
			}
			if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.work_items(org_id,operation_id,phase,status,assignment_spec) VALUES($1,$2,'preflight','queued',$3)`, in.Org, op, jsonBytes(spec)); e != nil {
				return nil, e
			}
			out := &ReceiptOutput{}
			out.Body.RequestID = request(ctx).ID
			out.Body.SubjectID = run
			out.Body.Revision = 1
			out.Body.Status = "queued"
			out.Body.OperationID = op
			action := "preflight.request"
			if in.Body.Intent != "source_check" && in.Body.Intent != "" {
				action = "plan.request"
			}
			return out, store.Audit(ctx, tx, in.Org, actor.ID, action, run)
		})
	})
}

type jsonScanner struct{ v any }

func (s *jsonScanner) Scan(src any) error {
	switch b := src.(type) {
	case []byte:
		return json.Unmarshal(b, s.v)
	case string:
		return json.Unmarshal([]byte(b), s.v)
	default:
		return errors.New("invalid stored JSON")
	}
}
func newJSON(v any) *jsonScanner { return &jsonScanner{v} }
func jsonBytes(v any) []byte     { b, _ := json.Marshal(v); return b }

func (a *Server) importInventory(ctx context.Context, tx pgx.Tx, org, repo string, r RepoFacts, observation coordinator.Observation, facts bitbucket.Facts, manifest catalog.Manifest) (*SyncOutput, error) {
	var e error
	out := &SyncOutput{}
	out.Body.RepositoryID = repo
	out.Body.Commit = facts.Commit
	out.Body.StackIDs = []string{}
	ids := []string{}
	for _, s := range manifest.Stacks {
		if s.ProjectID != r.ProjectID {
			return nil, store.ErrForbidden
		}
		effective, e := coordinator.BuildSpec(ctx, tx, s, repo)
		if e != nil {
			return nil, e
		}
		id := ""
		e = tx.QueryRow(ctx, `INSERT INTO cosmoformer.stacks(org_id,project_id,environment_id,repository_id,declaration_id,display_name) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(org_id,repository_id,declaration_id) DO UPDATE SET display_name=excluded.display_name,archived=false,revision=cosmoformer.stacks.revision+1 WHERE cosmoformer.stacks.project_id=excluded.project_id AND cosmoformer.stacks.environment_id=excluded.environment_id RETURNING id::text`, org, s.ProjectID, s.EnvironmentID, repo, s.ID, s.Name).Scan(&id)
		if e != nil {
			return nil, e
		}
		b, _ := catalog.Canonical(effective)
		h := sha256.Sum256(b)
		var rev string
		e = tx.QueryRow(ctx, `SELECT id::text FROM cosmoformer.stack_revisions WHERE stack_id=$1 AND commit=$2 AND semantic_digest=$3 ORDER BY created_at DESC LIMIT 1`, id, facts.Commit, h[:]).Scan(&rev)
		if errors.Is(e, pgx.ErrNoRows) {
			rev = runs.ID()
			parsed, _ := json.Marshal(s)
			_, e = tx.Exec(ctx, `INSERT INTO cosmoformer.stack_revisions(id,org_id,stack_id,commit,parsed_config,effective_config,semantic_digest,inventory_revision) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, rev, org, id, facts.Commit, parsed, b, h[:], facts.InventoryRevision)
		}
		if e != nil {
			return nil, e
		}
		if _, e = tx.Exec(ctx, `UPDATE cosmoformer.stacks SET effective_revision_id=$2 WHERE id=$1`, id, rev); e != nil {
			return nil, e
		}
		out.Body.StackIDs = append(out.Body.StackIDs, id)
		ids = append(ids, s.ID)
	}
	if _, e = tx.Exec(ctx, `UPDATE cosmoformer.stacks SET archived=true,revision=revision+1 WHERE repository_id=$1 AND NOT (declaration_id=ANY($2::uuid[])) AND NOT archived`, repo, ids); e != nil {
		return nil, e
	}
	if _, e = tx.Exec(ctx, `UPDATE cosmoformer.repositories SET inventory_commit=$2,revision=$3 WHERE id=$1`, repo, facts.Commit, facts.InventoryRevision); e != nil {
		return nil, e
	}
	return out, nil
}

// Authenticated source facts are durable independently of manifest validity.
// This transaction grants no new work; actor/grant checks run again when the
// caller later imports inventory or creates a request.
func (a *Server) acceptSourceFacts(ctx context.Context, org, repo string, r RepoFacts, o coordinator.Observation, f bitbucket.Facts) (int64, error) {
	var revision int64
	e := a.serviceTx(ctx, org, func(tx pgx.Tx) error {
		current, e := repoFacts(ctx, tx, repo)
		if e != nil {
			return e
		}
		if current.Revision != r.Revision {
			return store.ErrConflict
		}
		revision, e = a.Coordinator.AcceptObservation(ctx, tx, org, o, f)
		return e
	})
	return revision, e
}
