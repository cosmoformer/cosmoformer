package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/access"
	"github.com/cosmoformer/cosmoformer/internal/bitbucket"
	"github.com/cosmoformer/cosmoformer/internal/coordinator"
	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
)

type ApplyOutput struct {
	Body struct {
		RequestID   string `json:"request_id"`
		SubjectID   string `json:"subject_id" format:"uuid"`
		Revision    int64  `json:"revision"`
		Status      string `json:"status"`
		OperationID string `json:"operation_id" format:"uuid"`
	}
	Status int
}
type ApplyInput struct {
	OrgIDPath
	Body struct {
		Revision int64  `json:"expected_plan_revision" minimum:"1" maximum:"9007199254740991"`
		Manifest string `json:"manifest_digest" pattern:"^[0-9a-f]{64}$"`
		Priority string `json:"priority" enum:"low,medium,high"`
		Context  struct {
			Stack       string `json:"stack_id" format:"uuid"`
			Environment string `json:"environment_id" format:"uuid"`
			Commit      string `json:"source_commit" pattern:"^[0-9a-f]{40}$"`
		} `json:"confirmed_context"`
	}
}

func applySubjectFor(ctx context.Context, tx pgx.Tx, actor store.Actor, id string) (approvalSubject, error) {
	p, e := approvalSubjectFor(ctx, tx, actor, id, false)
	if e != nil {
		return p, e
	}
	g, e := store.GrantsFor(ctx, tx, actor.ID)
	if e != nil {
		return p, e
	}
	if !access.Allows(g, p.Project, p.Environment, access.Apply) {
		return p, store.ErrForbidden
	}
	return p, nil
}
func applyProblem(ctx context.Context, e error) error {
	code := runs.Code(e)
	if code == "" {
		return approvalFailure(ctx, e)
	}
	messages := map[string]string{"APPROVALS_REQUIRED": "Current eligible approvals are insufficient. No Apply was queued.", "PR_APPLY_DISABLED": "This stack does not allow Apply from an open pull request.", "APPLY_ALREADY_PENDING": "An Apply already exists for this exact plan.", "NEW_PLAN_REQUIRED": "This plan has been consumed. Request a new normal plan and approvals.", "CONFIRMATION_REQUIRED": "Confirm the exact stack, environment and source commit.", "PLAN_IDENTITY_CHANGED": "The exact reviewed plan identity changed. Refresh this run."}
	if message, ok := messages[code]; ok {
		p := problem(ctx, 409, message)
		p.Code = code
		return p
	}
	return approvalProblem(ctx, code)
}
func (a *Server) applyClock(ctx context.Context, tx pgx.Tx) (bool, error) {
	tick, e := a.Coordinator.Clock.Now()
	if e != nil {
		_, e = tx.Exec(ctx, `SELECT cosmoformer.execution_clock_failed()`)
		return false, e
	}
	var good bool
	e = tx.QueryRow(ctx, `SELECT cosmoformer.execution_clock_observe($1,$2)`, a.Coordinator.Instance, int64(tick)).Scan(&good)
	return good, e
}
func retainedSpec(p approvalSubject, records []evidence.Artifact) runs.ExecutionSpec {
	s := p.Spec
	s.RetainedPlan = &runs.RetainedPlan{ID: p.ID, Revision: p.Revision, ManifestDigest: p.Manifest, Artifacts: p.Artifacts, Records: []runs.RetainedArtifact{}}
	byID := map[string]evidence.Artifact{}
	for _, r := range records {
		byID[r.ID] = r
	}
	for _, id := range p.Artifacts {
		r := byID[id]
		s.RetainedPlan.Records = append(s.RetainedPlan.Records, runs.RetainedArtifact{ID: r.ID, Kind: r.Kind, Hash: r.Hash, Size: r.Size, Version: r.Version})
	}
	return s
}
func (a *Server) registerApply(api huma.API) {
	register(api, "POST", "/organizations/{org}/plans/{id}/apply", "apply-plan", func(ctx context.Context, in *ApplyInput) (*ApplyOutput, error) {
		var subject approvalSubject
		var observation coordinator.Observation
		var repo RepoFacts
		var records []evidence.Artifact
		var out *ApplyOutput
		var found, clockDenied bool
		check := func(tx pgx.Tx, p approvalSubject) error {
			if p.Revision != in.Body.Revision || p.Manifest != in.Body.Manifest {
				return runs.Fail("PLAN_IDENTITY_CHANGED")
			}
			if in.Body.Context.Stack != p.Stack || in.Body.Context.Environment != p.Environment || in.Body.Context.Commit != p.Commit {
				return runs.Fail("CONFIRMATION_REQUIRED")
			}
			reason, e := a.approvalEligibility(ctx, tx, p)
			if e != nil {
				return e
			}
			if reason != "" {
				return runs.Fail(reason)
			}
			rec, e := approvalArtifacts(ctx, tx, p)
			if e != nil {
				return runs.Fail("EVIDENCE_UNAVAILABLE")
			}
			if e = coordinator.ApplyEligibility(ctx, tx, p.Stack, retainedSpec(p, rec)); e != nil {
				return e
			}
			return coordinator.ApplyHistory(ctx, tx, p.ID)
		}
		e := a.tx(ctx, in.Org, func(tx pgx.Tx, actor store.Actor) error {
			var e error
			subject, e = applySubjectFor(ctx, tx, actor, in.ID)
			if e != nil {
				return e
			}
			out, found, e = browserReceipt[ApplyOutput](ctx, tx, actor)
			if e != nil || found {
				return e
			}
			good, e := a.applyClock(ctx, tx)
			if e != nil {
				return e
			}
			if !good {
				clockDenied = true
				return nil
			}
			if e = check(tx, subject); e != nil {
				return applyProblem(ctx, e)
			}
			records, e = approvalArtifacts(ctx, tx, subject)
			if e != nil {
				return e
			}
			repo, e = repoFacts(ctx, tx, subject.Repo)
			if e != nil {
				return e
			}
			observation, e = a.Coordinator.BeginObservation(ctx, tx, in.Org, subject.Repo, subject.Kind, subject.Key)
			return e
		})
		if e != nil {
			return nil, e
		}
		if found {
			out.Status = 202
			return out, nil
		}
		if clockDenied {
			return nil, approvalProblem(ctx, "CLOCK_UNRELIABLE")
		}
		readCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		token, e := a.apiToken(readCtx, in.Org, repo.IntegrationID)
		if e != nil {
			cancel()
			return nil, approvalProblem(ctx, "SOURCE_UNAVAILABLE")
		}
		pr := 0
		if subject.Kind == "pr" {
			pr, e = strconv.Atoi(subject.Key)
			if e != nil {
				cancel()
				return nil, approvalProblem(ctx, "SOURCE_CHANGED")
			}
		}
		facts, e := a.Bitbucket.Read(readCtx, repo.URL, repo.ExternalID, repo.Branch, pr, token)
		cancel()
		changed := errors.Is(e, bitbucket.ErrContextChanged)
		if e != nil && !changed {
			return nil, approvalProblem(ctx, "SOURCE_UNAVAILABLE")
		}
		facts.InventoryRevision = repo.Revision
		revision, e := a.acceptSourceFacts(ctx, in.Org, subject.Repo, repo, observation, facts)
		if e != nil {
			return nil, approvalProblem(ctx, "SOURCE_VERIFICATION_STALE")
		}
		if changed || revision != subject.Spec.Source.FactsRevision || facts.Commit != subject.Commit {
			return nil, approvalProblem(ctx, "SOURCE_CHANGED")
		}
		authorize := func(fn func(pgx.Tx) error) error {
			return a.Store.ActorTx(ctx, request(ctx).Hash, in.Org, func(tx pgx.Tx, actor store.Actor) error {
				p, e := applySubjectFor(ctx, tx, actor, in.ID)
				if e != nil {
					return e
				}
				if p.Revision != in.Body.Revision || p.Manifest != in.Body.Manifest {
					return runs.Fail("PLAN_IDENTITY_CHANGED")
				}
				return fn(tx)
			})
		}
		var manifest runs.Manifest
		for _, r := range records {
			_, f, e := a.Evidence.Read(ctx, authorize, r.ID)
			if e != nil {
				return nil, applyProblem(ctx, runs.Fail("EVIDENCE_UNAVAILABLE"))
			}
			if r.Kind == "manifest" {
				raw, readErr := io.ReadAll(io.LimitReader(f, evidence.PlanMax+1))
				if readErr != nil || int64(len(raw)) > evidence.PlanMax || runs.Decode(raw, &manifest) != nil || runs.Digest(manifest) != subject.Manifest {
					e = evidence.ErrUnavailable
				}
			}
			f.Close()
			os.Remove(f.Name())
			if e != nil {
				return nil, applyProblem(ctx, runs.Fail("EVIDENCE_UNAVAILABLE"))
			}
		}
		if manifest.ReviewedInputs.Validate(subject.Spec, subject.Stack, subject.Epoch) != nil || manifest.ValidateLock() != nil {
			return nil, applyProblem(ctx, runs.Fail("EVIDENCE_UNAVAILABLE"))
		}
		e = a.tx(ctx, in.Org, func(tx pgx.Tx, actor store.Actor) error {
			p, e := applySubjectFor(ctx, tx, actor, in.ID)
			if e != nil {
				return e
			}
			out, found, e = browserReceipt[ApplyOutput](ctx, tx, actor)
			if e != nil || found {
				return e
			}
			good, e := a.applyClock(ctx, tx)
			if e != nil {
				return e
			}
			if !good {
				clockDenied = true
				return nil
			}
			if e = check(tx, p); e != nil {
				return applyProblem(ctx, e)
			}
			current, e := approvalArtifacts(ctx, tx, p)
			if e != nil || runs.Digest(current) != runs.Digest(records) {
				return applyProblem(ctx, runs.Fail("EVIDENCE_UNAVAILABLE"))
			}
			var authenticated bool
			if e = tx.QueryRow(ctx, `SELECT cosmoformer.session_identity($1) IS NOT NULL`, request(ctx).Hash).Scan(&authenticated); e != nil {
				return e
			}
			if !authenticated {
				return store.ErrUnauthenticated
			}
			// Reference and session checks can consume the last source/plan
			// window. Reobserve trusted time at the final acceptance decision.
			good, e = a.applyClock(ctx, tx)
			if e != nil {
				return e
			}
			if !good {
				clockDenied = true
				return nil
			}
			var unexpired bool
			if e = tx.QueryRow(ctx, `SELECT expires_at>clock_timestamp() FROM cosmoformer.plans WHERE id=$1`, p.ID).Scan(&unexpired); e != nil {
				return e
			}
			if !unexpired {
				return approvalProblem(ctx, "PLAN_EXPIRED")
			}
			if e = a.Coordinator.RequireObservation(ctx, tx, observation, p.Spec.Source.FactsRevision); e != nil {
				return approvalProblem(ctx, "SOURCE_VERIFICATION_STALE")
			}
			operation, work := runs.ID(), runs.ID()
			s := retainedSpec(p, records)
			tag, e := tx.Exec(ctx, `INSERT INTO cosmoformer.operations(id,org_id,run_id,stack_id,kind,plan_id,status,priority,accepted_at,ordering_at,requester_id) SELECT $1,$2,$3,$4,'apply',$5,'queued',$6,clock_timestamp(),clock_timestamp(),$7 FROM cosmoformer.plans WHERE id=$5 AND expires_at>clock_timestamp()`, operation, in.Org, p.RunID, p.Stack, p.ID, in.Body.Priority, actor.ID)
			if e != nil {
				return e
			}
			if tag.RowsAffected() != 1 {
				return approvalProblem(ctx, "PLAN_EXPIRED")
			}
			if e = coordinator.HoldApply(ctx, tx, s, "operation", operation); e != nil {
				return applyProblem(ctx, e)
			}
			if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.work_items(id,org_id,operation_id,phase,status,assignment_spec) VALUES($1,$2,$3,'execution','queued',$4)`, work, in.Org, operation, jsonBytes(s)); e != nil {
				return e
			}
			out = &ApplyOutput{Status: 202}
			out.Body.RequestID = request(ctx).ID
			out.Body.SubjectID = p.RunID
			out.Body.OperationID = operation
			out.Body.Revision = 1
			out.Body.Status = "queued"
			if e = store.Audit(ctx, tx, in.Org, actor.ID, "plan.apply", p.ID); e != nil {
				return e
			}
			raw, e := json.Marshal(out)
			if e != nil {
				return e
			}
			d := request(ctx)
			_, e = tx.Exec(ctx, `SELECT cosmoformer.save_browser_request($1,$2,$3,$4::jsonb,false)`, actor.ID, d.Key, d.Digest, string(raw))
			return e
		})
		if e != nil {
			return nil, e
		}
		if clockDenied {
			return nil, approvalProblem(ctx, "CLOCK_UNRELIABLE")
		}
		out.Status = 202
		return out, nil
	})
}
