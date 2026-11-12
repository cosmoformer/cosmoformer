// Package coordinator implements worker coordination and the native control
// protocol, source checks and explicitly requested native planning.
package coordinator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/access"
	"github.com/cosmoformer/cosmoformer/internal/bitbucket"
	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/clock"
	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/identity"
	"github.com/cosmoformer/cosmoformer/internal/plan"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/source"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/jackc/pgx/v5"
)

type Coordinator struct {
	Store            *store.Store
	Clock            clock.Source
	Instance         string
	Evidence         *evidence.Custody
	ObserveSource    func(context.Context, string, string) (bitbucket.Facts, error)
	NativeEvidence   NativeEvidenceVerifier
	PlanSeconds      int64
	ApplySeconds     int64
	PreflightSeconds int64
	PlanTTLHours     int64
	ManagedSecrets   func(context.Context, pgx.Tx, string, runs.ExecutionSpec) ([]runs.EnvSecret, error)
}

func New(s *store.Store, c clock.Source) *Coordinator {
	return &Coordinator{Store: s, Clock: c, Instance: runs.ID(), PlanSeconds: 1200, ApplySeconds: 2400, PreflightSeconds: 300, PlanTTLHours: 168, NativeEvidence: plan.Verifier{}}
}
func digest(v any) []byte    { b, _ := catalog.Canonical(v); h := sha256.Sum256(b); return h[:] }
func hexDigest(v any) string { return hex.EncodeToString(digest(v)) }

type Worker struct {
	ID     string `json:"id"`
	OrgID  string `json:"org_id"`
	PoolID string `json:"pool_id"`
}

func (c *Coordinator) workerTx(ctx context.Context, token string, fn func(pgx.Tx, Worker) error) error {
	if c.Store == nil {
		return runs.Fail("UNAVAILABLE")
	}
	if !identity.ValidToken(token) {
		return store.ErrUnauthenticated
	}
	tx, e := c.Store.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, `SELECT id FROM cosmoformer.coordination_gate FOR UPDATE`); e != nil {
		return e
	}
	w, e := store.JSON[Worker](ctx, tx, `SELECT cosmoformer.worker_identity($1)`, identity.Digest(token))
	if e != nil {
		return store.ErrUnauthenticated
	}
	if _, e = tx.Exec(ctx, `SELECT set_config('app.org_id',$1,true)`, w.OrgID); e != nil {
		return e
	}
	if e = fn(tx, w); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (c *Coordinator) current(ctx context.Context, tx pgx.Tx, w Worker, id runs.Identity, positive bool) error {
	if id.WorkerID != w.ID || !catalog.UUID(id.Agent) || id.Generation < 1 || id.Generation > 9007199254740991 {
		return runs.Fail("STALE_AGENT")
	}
	var matches bool
	e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.workers w JOIN cosmoformer.worker_pools p ON (p.org_id,p.id)=(w.org_id,w.pool_id) JOIN cosmoformer.runtime_catalog r ON r.id=w.runtime_id WHERE w.id=$1 AND w.generation=$2 AND w.agent_instance=$3 AND (NOT $4::boolean OR (NOT w.paused AND p.enabled AND r.approved AND NOT r.withdrawn AND NOT EXISTS(SELECT FROM cosmoformer.installation WHERE maintenance OR clock_unreliable))))`, w.ID, id.Generation, id.Agent, positive).Scan(&matches)
	if e != nil {
		return e
	}
	if !matches {
		return runs.Fail("STALE_AGENT")
	}
	return nil
}
func (c *Coordinator) Admit(ctx context.Context, token string, in runs.Admission) (int64, error) {
	var generation int64
	var restarted bool
	if in.Protocol != 1 || !catalog.UUID(in.WorkerID) || !catalog.UUID(in.Agent) || !catalog.UUID(in.RuntimeID) || in.ClockSource != "boottime" || !in.ClockOK || in.Arch != "amd64" || in.GitPackage != "1:2.47.3-0+deb13u1" || in.GitVersion != "git version 2.47.3" || !planningPhases(in.Phases) {
		return 0, runs.Fail("RUNTIME_UNSUPPORTED")
	}
	if _, e := c.Clock.Now(); e != nil {
		return 0, runs.Fail("CLOCK_UNAVAILABLE")
	}
	e := c.workerTx(ctx, token, func(tx pgx.Tx, w Worker) error {
		if w.ID != in.WorkerID {
			return store.ErrForbidden
		}
		var allowed bool
		e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.runtime_catalog WHERE id=$1 AND engine=$2 AND version=$3 AND architecture=$4 AND image_digest=$5 AND approved AND NOT withdrawn)`, in.RuntimeID, in.Engine, in.Version, in.Arch, in.ImageDigest).Scan(&allowed)
		if e != nil {
			return e
		}
		if !allowed {
			return runs.Fail("RUNTIME_UNSUPPORTED")
		}
		var oldAgent string
		var oldCaps []byte
		var paused, reserved, healthy bool
		e = tx.QueryRow(ctx, `SELECT generation,coalesce(agent_instance::text,''),capabilities,paused,EXISTS(SELECT FROM cosmoformer.worker_reservations WHERE worker_id=w.id),coalesce(last_contact_at>clock_timestamp()-interval '30 seconds',false) FROM cosmoformer.workers w WHERE id=$1`, w.ID).Scan(&generation, &oldAgent, &oldCaps, &paused, &reserved, &healthy)
		if e != nil {
			return e
		}
		if paused {
			return runs.Fail("WORKER_PAUSED")
		}
		if oldAgent == in.Agent {
			var previous runs.Admission
			if json.Unmarshal(oldCaps, &previous) != nil || hexDigest(previous) != hexDigest(in) {
				return runs.Fail("MESSAGE_CONFLICT")
			}
		} else {
			if reserved {
				var attempt string
				if e = tx.QueryRow(ctx, `SELECT attempt_id::text FROM cosmoformer.worker_reservations WHERE worker_id=$1`, w.ID).Scan(&attempt); e != nil {
					return e
				}
				restarted = true
				return c.attention(ctx, tx, attempt)
			}
			if healthy {
				return runs.Fail("WORKER_BUSY")
			}
			generation++
		}
		_, e = tx.Exec(ctx, `UPDATE cosmoformer.workers SET generation=$2,agent_instance=$3,runtime_id=$4,capabilities=$5,last_contact_at=clock_timestamp(),revision=revision+1 WHERE id=$1`, w.ID, generation, in.Agent, in.RuntimeID, mustJSON(in))
		return e
	})
	if e == nil && restarted {
		e = runs.Fail("RECOVERY_REQUIRED")
	}
	return generation, e
}
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
func (c *Coordinator) Heartbeat(ctx context.Context, token string, id runs.Identity) error {
	return c.workerTx(ctx, token, func(tx pgx.Tx, w Worker) error {
		if e := c.current(ctx, tx, w, id, false); e != nil {
			return e
		}
		_, e := tx.Exec(ctx, `UPDATE cosmoformer.workers SET last_contact_at=clock_timestamp() WHERE id=$1`, w.ID)
		return e
	})
}

func (c *Coordinator) Poll(ctx context.Context, token string, in runs.Poll) (*runs.Assignment, error) {
	if !catalog.UUID(in.RequestID) || in.Wait != 20 {
		return nil, runs.Fail("INVALID_REQUEST")
	}
	var out *runs.Assignment
	var clockDenied error
	e := c.workerTx(ctx, token, func(tx pgx.Tx, w Worker) error {
		if e := c.current(ctx, tx, w, in.Identity, true); e != nil {
			return e
		}
		tick, e := c.Clock.Now()
		if e != nil {
			clockDenied = runs.Fail("CLOCK_UNRELIABLE")
			_, e = tx.Exec(ctx, `SELECT cosmoformer.execution_clock_failed()`)
			return e
		}
		var old []byte
		var oldDigest []byte
		e = tx.QueryRow(ctx, `SELECT response,request_digest FROM cosmoformer.worker_requests WHERE worker_id=$1 AND request_id=$2`, w.ID, in.RequestID).Scan(&old, &oldDigest)
		if e == nil {
			if !identity.Equal(oldDigest, digest(in)) {
				return runs.Fail("MESSAGE_CONFLICT")
			}
			if json.Unmarshal(old, &out) != nil {
				return runs.Fail("MESSAGE_CONFLICT")
			}
			return c.remaining(ctx, tx, out, tick)
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		var attempt string
		e = tx.QueryRow(ctx, `SELECT attempt_id::text FROM cosmoformer.worker_reservations WHERE worker_id=$1 AND generation=$2 AND agent_instance=$3`, w.ID, in.Generation, in.Agent).Scan(&attempt)
		if e == nil {
			out, e = c.assignment(ctx, tx, attempt)
			if e != nil {
				return e
			}
		} else if !errors.Is(e, pgx.ErrNoRows) {
			return e
		} else {
			type candidate struct {
				WorkID, OperationID, RunID, StackID string
				Spec                                runs.PreflightSpec
				Phase                               string
				Execution                           *runs.ExecutionSpec
			}
			rows, e := tx.Query(ctx, `SELECT wi.id::text,o.id::text,o.run_id::text,o.stack_id::text,wi.assignment_spec,wi.phase FROM cosmoformer.work_items wi JOIN cosmoformer.operations o ON o.id=wi.operation_id JOIN cosmoformer.stacks s ON s.id=o.stack_id WHERE wi.status='queued' AND wi.phase IN ('preflight','execution') AND (wi.phase='preflight' OR (o.native_planning AND o.kind='plan' OR o.kind='apply') AND s.assessment='compatible') AND EXISTS(SELECT FROM cosmoformer.workers cw WHERE cw.id=$2 AND cw.capabilities->'supported_work_phases' ? wi.phase) AND o.status='queued' AND NOT o.cancel_requested AND NOT s.archived AND wi.assignment_spec->'pool'->>'id'=$1 AND wi.assignment_spec->'engine'->>'runtime_id'=(SELECT runtime_id::text FROM cosmoformer.workers WHERE id=$2) ORDER BY CASE o.priority WHEN 'high' THEN 0 WHEN 'medium' THEN 1 ELSE 2 END,o.ordering_at,o.id`, w.PoolID, w.ID)
			if e != nil {
				return e
			}
			all := []candidate{}
			for rows.Next() {
				var v candidate
				var b []byte
				if e = rows.Scan(&v.WorkID, &v.OperationID, &v.RunID, &v.StackID, &b, &v.Phase); e != nil {
					rows.Close()
					return e
				}
				if v.Phase == "execution" {
					v.Execution = &runs.ExecutionSpec{}
					if runs.Decode(b, v.Execution) != nil || v.Execution.Validate() != nil {
						rows.Close()
						return runs.Fail("INVALID_SPEC")
					}
					v.Spec = v.Execution.Preflight()
				}
				if v.Execution == nil && json.Unmarshal(b, &v.Spec) != nil {
					rows.Close()
					return runs.Fail("INVALID_SPEC")
				}
				all = append(all, v)
			}
			rows.Close()
			if e = rows.Err(); e != nil {
				return e
			}
			for _, v := range all {
				if v.Execution != nil && v.Execution.RetainedPlan != nil {
					good := false
					tick, e = c.Clock.Now()
					if e != nil {
						clockDenied = runs.Fail("CLOCK_UNRELIABLE")
						_, e = tx.Exec(ctx, `SELECT cosmoformer.execution_clock_failed()`)
						return e
					}
					if e = tx.QueryRow(ctx, `SELECT cosmoformer.execution_clock_observe($1,$2)`, c.Instance, int64(tick)).Scan(&good); e != nil {
						return e
					}
					if !good {
						clockDenied = runs.Fail("CLOCK_UNRELIABLE")
						return nil
					}
					e = ApplyEligibility(ctx, tx, v.StackID, *v.Execution)
					if e == nil {
						e = applyOperationHolds(ctx, tx, *v.Execution, v.OperationID)
					}
					if e != nil {
						if runs.Code(e) == "" {
							return e
						}
						if _, e = tx.Exec(ctx, `UPDATE cosmoformer.operations SET status='retired',retired_at=clock_timestamp(),terminal_reason='APPLY_ELIGIBILITY_LOST',revision=revision+1 WHERE id=$1`, v.OperationID); e != nil {
							return e
						}
						if _, e = tx.Exec(ctx, `UPDATE cosmoformer.work_items SET status='retired',revision=revision+1 WHERE id=$1`, v.WorkID); e != nil {
							return e
						}
						continue
					}
				}
				if e = c.authority(ctx, tx, v.OperationID, v.Spec); e != nil {
					if errors.Is(e, store.ErrForbidden) || runs.Code(e) == "INPUTS_CHANGED" || runs.Code(e) == "SOURCE_CHANGED" || runs.Code(e) == "RECOVERY_EVIDENCE_CHANGED" {
						_, err := tx.Exec(ctx, `UPDATE cosmoformer.operations SET status='retired',retired_at=clock_timestamp(),terminal_reason='ELIGIBILITY_CHANGED',revision=revision+1 WHERE id=$1`, v.OperationID)
						if err != nil {
							return err
						}
						if _, err = tx.Exec(ctx, `UPDATE cosmoformer.work_items SET status='retired',revision=revision+1 WHERE id=$1`, v.WorkID); err != nil {
							return err
						}
						continue
					}
					return e
				}
				var tags []string
				if e = tx.QueryRow(ctx, `SELECT tags FROM cosmoformer.workers WHERE id=$1`, w.ID).Scan(&tags); e != nil {
					return e
				}
				matched := true
				for _, wanted := range v.Spec.Pool.Tags {
					found := false
					for _, tag := range tags {
						if wanted == tag {
							found = true
						}
					}
					if !found {
						matched = false
					}
				}
				if !matched {
					continue
				}
				var target any
				capSeconds := c.PreflightSeconds
				if capSeconds == 0 {
					capSeconds = 300
				}
				if v.Execution != nil {
					capSeconds = c.PlanSeconds
					if v.Execution.RetainedPlan != nil {
						capSeconds = c.ApplySeconds
					}
					if capSeconds == 0 {
						capSeconds = 1200
					}
					var targetID string
					var busy bool
					if e = tx.QueryRow(ctx, `SELECT target_id::text,EXISTS(SELECT FROM cosmoformer.backend_reservations WHERE target_id=b.target_id) OR EXISTS(SELECT FROM cosmoformer.recovery_cases r WHERE target_id=b.target_id AND status='open' AND NOT EXISTS(SELECT FROM cosmoformer.operations o WHERE o.id=$2 AND o.recovery_case_id=r.id AND o.recovery_evidence_revision=r.evidence_revision AND r.stop_verified)) FROM cosmoformer.stack_bindings b WHERE stack_id=$1`, v.StackID, v.OperationID).Scan(&targetID, &busy); e != nil {
						return e
					}
					if busy {
						continue
					}
					target = targetID
				}
				attempt = runs.ID()
				deadline := tick + time.Duration(capSeconds)*time.Second
				_, e = tx.Exec(ctx, `INSERT INTO cosmoformer.attempts(id,org_id,work_item_id,operation_id,stack_id,worker_id,generation,agent_instance,state,budget_platform_instance,accepted_tick,deadline_tick,cap_seconds,display_deadline,target_id,execution_spec,execution_spec_digest) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'claimed',$9,$10,$11,$12,clock_timestamp()+$12::bigint*interval '1 second',$13,$14,$15)`, attempt, w.OrgID, v.WorkID, v.OperationID, v.StackID, w.ID, in.Generation, in.Agent, c.Instance, int64(tick), int64(deadline), capSeconds, target, executionSpecJSON(v.Execution), executionSpecHash(v.Execution))
				if e != nil {
					return e
				}
				if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.worker_reservations(org_id,worker_id,attempt_id,generation,agent_instance) VALUES($1,$2,$3,$4,$5)`, w.OrgID, w.ID, attempt, in.Generation, in.Agent); e != nil {
					return e
				}
				if v.Execution != nil {
					if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.backend_reservations(org_id,target_id,attempt_id,stack_id) VALUES($1,$2,$3,$4)`, w.OrgID, target, attempt, v.StackID); e != nil {
						return e
					}
					claimed, e := c.assignment(ctx, tx, attempt)
					if e != nil {
						return e
					}
					if v.Execution.RetainedPlan == nil {
						if tag, insertErr := tx.Exec(ctx, `INSERT INTO cosmoformer.plans(org_id,operation_id,attempt_id,stack_id,target_id,status,provenance,source_facts_revision,state_epoch,rule_version,profile_id,recovery_case_id,recovery_evidence_revision) VALUES($1,$2,$3,$4,$5,'assembling',$10,$6,$7,$8,$9,$11,$12) ON CONFLICT(operation_id) DO UPDATE SET attempt_id=excluded.attempt_id,state_epoch=excluded.state_epoch,revision=cosmoformer.plans.revision+1 WHERE cosmoformer.plans.status='assembling' AND NOT EXISTS(SELECT FROM cosmoformer.attempts WHERE id=cosmoformer.plans.attempt_id AND ever_permitted)`, w.OrgID, v.OperationID, attempt, v.StackID, target, v.Spec.Source.FactsRevision, claimed.StateEpoch, v.Execution.RuleVersion, v.Execution.ProfileID, v.Execution.Provenance().Kind, nullable(v.Execution.Provenance().Case), nullableRevision(v.Execution.Provenance().Revision)); insertErr != nil {
							return insertErr
						} else if tag.RowsAffected() != 1 {
							return runs.Fail("PLAN_NOT_ASSEMBLING")
						}
					} else if e = HoldApply(ctx, tx, *v.Execution, "attempt", attempt); e != nil {
						return e
					}
					if e = c.executionAuthority(ctx, tx, *claimed); e != nil {
						return e
					}
				}
				if _, e = tx.Exec(ctx, `UPDATE cosmoformer.work_items SET status='claimed',attempt_count=attempt_count+1,revision=revision+1 WHERE id=$1`, v.WorkID); e != nil {
					return e
				}
				if _, e = tx.Exec(ctx, `UPDATE cosmoformer.operations SET status='preparing',revision=revision+1 WHERE id=$1`, v.OperationID); e != nil {
					return e
				}
				out, e = c.assignment(ctx, tx, attempt)
				if e != nil {
					return e
				}
				break
			}
		}
		if out == nil {
			return nil
		}
		if e = c.remaining(ctx, tx, out, tick); e != nil {
			return e
		}
		_, e = tx.Exec(ctx, `INSERT INTO cosmoformer.worker_requests(org_id,worker_id,request_id,generation,agent_instance,request_digest,response) VALUES($1,$2,$3,$4,$5,$6,$7)`, w.OrgID, w.ID, in.RequestID, in.Generation, in.Agent, digest(in), mustJSON(out))
		return e
	})
	if e == nil && clockDenied != nil {
		return nil, clockDenied
	}
	return out, e
}
func (c *Coordinator) assignment(ctx context.Context, tx pgx.Tx, id string) (*runs.Assignment, error) {
	var a runs.Assignment
	var b []byte
	var specDigest string
	e := tx.QueryRow(ctx, `SELECT a.id::text,a.operation_id::text,a.work_item_id::text,o.kind,wi.phase,o.run_id::text,a.stack_id::text,coalesce(a.execution_spec,wi.assignment_spec),a.cap_seconds,a.budget_platform_instance::text,coalesce(encode(a.execution_spec_digest,'hex'),''),coalesce((SELECT max(sequence)+1 FROM cosmoformer.attempt_messages WHERE attempt_id=a.id),1) FROM cosmoformer.attempts a JOIN cosmoformer.operations o ON o.id=a.operation_id JOIN cosmoformer.work_items wi ON wi.id=a.work_item_id WHERE a.id=$1`, id).Scan(&a.AttemptID, &a.OperationID, &a.WorkItemID, &a.Kind, &a.Phase, &a.RunID, &a.StackID, &b, &a.CapSeconds, &a.Platform, &specDigest, &a.NextSeq)
	if e != nil {
		return nil, e
	}
	if a.Phase == "execution" {
		a.Execution = &runs.ExecutionSpec{}
		if runs.Decode(b, a.Execution) != nil || a.Execution.Validate() != nil {
			return nil, runs.Fail("INVALID_SPEC")
		}
		a.Spec = a.Execution.Preflight()
		if e = tx.QueryRow(ctx, `SELECT cosmoformer.execution_target_epoch($1),p.minimum_approvals FROM cosmoformer.stacks st JOIN cosmoformer.projects p ON p.id=st.project_id WHERE st.id=$2`, id, a.StackID).Scan(&a.StateEpoch, &a.ApprovalFloor); e != nil {
			return nil, e
		}
	} else if runs.Decode(b, &a.Spec) != nil {
		return nil, runs.Fail("INVALID_SPEC")
	}
	a.Source = a.Spec.Source
	a.StackRevisionID = a.Spec.StackRevisionID
	a.RuntimeID = a.Spec.Engine.ID
	a.Digest = hexDigest(a.Spec)
	if a.Execution != nil {
		a.Digest = runs.Digest(a.Execution)
		if a.Digest != specDigest {
			return nil, runs.Fail("INVALID_SPEC")
		}
	}
	return &a, nil
}
func (c *Coordinator) remaining(ctx context.Context, tx pgx.Tx, a *runs.Assignment, tick time.Duration) error {
	if a == nil {
		return nil
	}
	if a.Platform != c.Instance {
		a.RemainingMS = 0
		return nil
	}
	var deadline int64
	var closed bool
	e := tx.QueryRow(ctx, `SELECT deadline_tick,closed_at IS NOT NULL FROM cosmoformer.attempts WHERE id=$1`, a.AttemptID).Scan(&deadline, &closed)
	if e != nil {
		return e
	}
	a.RemainingMS = max(0, (deadline-int64(tick))/int64(time.Millisecond))
	if closed {
		a.RemainingMS = 0
	}
	return nil
}
func (c *Coordinator) authority(ctx context.Context, tx pgx.Tx, operation string, s runs.PreflightSpec) error {
	if s.Recovery != nil {
		var valid bool
		if e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.operations o JOIN cosmoformer.recovery_cases r ON r.id=o.recovery_case_id WHERE o.id=$1 AND r.id=$2 AND o.stack_id=r.stack_id AND o.recovery_evidence_revision=$3 AND r.evidence_revision=$3 AND r.status='open' AND r.stop_verified)`, operation, s.Recovery.Case, s.Recovery.Revision).Scan(&valid); e != nil {
			return e
		}
		if !valid {
			return runs.Fail("RECOVERY_EVIDENCE_CHANGED")
		}
	}

	var requester, project, env, repo, kind string
	var enabled bool
	e := tx.QueryRow(ctx, `SELECT coalesce(o.requester_id::text,''),s.project_id::text,s.environment_id::text,s.repository_id::text,coalesce(cosmoformer.requester_enabled(o.requester_id),false),o.kind FROM cosmoformer.operations o JOIN cosmoformer.stacks s ON s.id=o.stack_id JOIN cosmoformer.repositories r ON r.id=s.repository_id WHERE o.id=$1 AND NOT s.archived AND r.enabled AND s.effective_revision_id=$2 AND NOT (o.native_planning AND o.recovery_case_id IS NULL AND EXISTS(SELECT FROM cosmoformer.recovery_cases rc WHERE rc.stack_id=o.stack_id AND rc.status='cleared' AND o.accepted_at<=rc.cleared_at))`, operation, s.StackRevisionID).Scan(&requester, &project, &env, &repo, &enabled, &kind)
	if errors.Is(e, pgx.ErrNoRows) {
		return runs.Fail("INPUTS_CHANGED")
	}
	if e != nil {
		return e
	}
	if requester != "" {
		if !enabled {
			return store.ErrForbidden
		}
		g, e := store.GrantsFor(ctx, tx, requester)
		if e != nil {
			return e
		}
		action := access.Plan
		if kind == "apply" {
			action = access.Apply
		}
		if !access.Allows(g, project, env, action) {
			return store.ErrForbidden
		}
	}
	for _, ref := range s.Secrets {
		var valid bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.secrets WHERE id=$1 AND semantic_revision=$2 AND enabled AND (project_id IS NULL OR project_id=$3))`, ref.ID, ref.Revision, project).Scan(&valid); e != nil {
			return e
		}
		if !valid {
			return runs.Fail("INPUTS_CHANGED")
		}
	}
	for _, ref := range s.SourceCredentials {
		var valid bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.source_credentials WHERE id=$1 AND repository_id=$2 AND semantic_revision=$3 AND enabled)`, ref.ID, ref.RepositoryID, ref.Revision).Scan(&valid); e != nil {
			return e
		}
		if !valid {
			return runs.Fail("INPUTS_CHANGED")
		}
	}
	var connection bool
	if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.aws_connections WHERE id=$1 AND revision=$2 AND role_arn=$3 AND expected_account=$4 AND enabled AND project_id=$5 AND (environment_id IS NULL OR environment_id=$6))`, s.AWS.ID, s.AWS.Revision, s.AWS.RoleARN, s.AWS.Account, project, env).Scan(&connection); e != nil {
		return e
	}
	if !connection {
		return runs.Fail("INPUTS_CHANGED")
	}
	var facts int64
	var commit string
	e = tx.QueryRow(ctx, `SELECT facts_revision,facts->>'commit' FROM cosmoformer.source_subjects WHERE id=$1`, s.Source.SubjectID).Scan(&facts, &commit)
	if e != nil {
		return e
	}
	if facts != s.Source.FactsRevision || commit != s.Source.Commit {
		return runs.Fail("SOURCE_CHANGED")
	}
	for _, r := range s.Grants {
		var valid bool
		e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.resource_grants WHERE id=$1 AND revision=$2 AND enabled AND project_id=$3 AND (environment_id IS NULL OR environment_id=$4))`, r.ID, r.Revision, project, env).Scan(&valid)
		if e != nil {
			return e
		}
		if !valid {
			return runs.Fail("INPUTS_CHANGED")
		}
	}
	var runtime bool
	e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.runtime_catalog WHERE id=$1 AND approved AND NOT withdrawn AND image_digest=$2)`, s.Engine.ID, s.Engine.ImageDigest).Scan(&runtime)
	if e != nil {
		return e
	}
	if !runtime {
		return runs.Fail("INPUTS_CHANGED")
	}
	return nil
}

// Observe records ticketed source facts only after a bounded read outside SQL.
type Observation struct {
	SubjectID, Instance string
	Baseline, Ticket    int64
	Started             time.Duration
}

func (c *Coordinator) BeginObservation(ctx context.Context, tx pgx.Tx, org, repo, kind, key string) (Observation, error) {
	var o Observation
	tick, e := c.Clock.Now()
	if e != nil {
		return o, e
	}
	o.Instance = c.Instance
	o.Started = tick
	e = tx.QueryRow(ctx, `INSERT INTO cosmoformer.source_subjects(org_id,repository_id,kind,external_key,facts) VALUES($1,$2,$3,$4,'{}') ON CONFLICT(org_id,repository_id,kind,external_key) DO UPDATE SET next_ticket=cosmoformer.source_subjects.next_ticket RETURNING id::text,facts_revision,next_ticket`, org, repo, kind, key).Scan(&o.SubjectID, &o.Baseline, &o.Ticket)
	if e != nil {
		return o, e
	}
	_, e = tx.Exec(ctx, `UPDATE cosmoformer.source_subjects SET next_ticket=next_ticket+1 WHERE id=$1`, o.SubjectID)
	return o, e
}
func (c *Coordinator) AcceptObservation(ctx context.Context, tx pgx.Tx, org string, o Observation, f bitbucket.Facts) (int64, error) {
	tick, e := c.Clock.Now()
	if e != nil {
		return 0, e
	}
	var revision, accepted int64
	var old []byte
	e = tx.QueryRow(ctx, `SELECT facts_revision,accepted_ticket,facts FROM cosmoformer.source_subjects WHERE id=$1`, o.SubjectID).Scan(&revision, &accepted, &old)
	if e != nil {
		return 0, e
	}
	if !runs.CanObserve(o.Instance, c.Instance, o.Started, tick, o.Baseline, revision, o.Ticket, accepted) {
		return 0, runs.Fail("SOURCE_CHANGED")
	}
	var prior bitbucket.Facts
	json.Unmarshal(old, &prior)
	if hexDigest(prior) != hexDigest(f) {
		revision++
		if _, e = tx.Exec(ctx, `UPDATE cosmoformer.plans SET status='invalidated',invalidation_reason='SOURCE_CHANGED',revision=revision+1 WHERE status='ready' AND operation_id IN (SELECT o.id FROM cosmoformer.operations o JOIN cosmoformer.runs r ON r.id=o.run_id WHERE r.source_subject_id=$1)`, o.SubjectID); e != nil {
			return 0, e
		}
	}
	if _, e = tx.Exec(ctx, `UPDATE cosmoformer.source_subjects SET accepted_ticket=$2,facts_revision=$3,facts=$4,revision=revision+1 WHERE id=$1`, o.SubjectID, o.Ticket, revision, mustJSON(f)); e != nil {
		return 0, e
	}
	_, e = tx.Exec(ctx, `INSERT INTO cosmoformer.source_observations(org_id,subject_id,ticket,baseline_facts_revision,resulting_facts_revision,facts,platform_instance,read_started_tick,received_tick,observed_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,clock_timestamp())`, org, o.SubjectID, o.Ticket, o.Baseline, revision, mustJSON(f), o.Instance, int64(o.Started), int64(tick))
	return revision, e
}

// RequireObservation fences later inventory/work creation against the facts
// already committed before manifest processing. A failed import cannot undo
// source invalidation, and a newer observation cannot be overwritten by it.
func (c *Coordinator) RequireObservation(ctx context.Context, tx pgx.Tx, o Observation, revision int64) error {
	var current, ticket int64
	if e := tx.QueryRow(ctx, `SELECT facts_revision,accepted_ticket FROM cosmoformer.source_subjects WHERE id=$1`, o.SubjectID).Scan(&current, &ticket); e != nil {
		return e
	}
	if current != revision || ticket != o.Ticket {
		return runs.Fail("SOURCE_CHANGED")
	}
	// Include the reference query's elapsed time in the freshness boundary.
	now, e := c.Clock.Now()
	if e != nil {
		return e
	}
	if o.Instance != c.Instance || now < o.Started || now-o.Started >= 30*time.Second {
		return runs.Fail("SOURCE_CHANGED")
	}
	return nil
}

func (c *Coordinator) Transition(ctx context.Context, token, route string, m runs.Message) (*runs.Transition, error) {
	if m.Protocol != 1 || !catalog.UUID(m.MessageID) || !catalog.UUID(m.AttemptID) || m.Seq < 1 || m.Seq > 9007199254740991 {
		return nil, runs.Fail("INVALID_REQUEST")
	}
	var native bool
	if e := c.workerTx(ctx, token, func(tx pgx.Tx, w Worker) error {
		if e := c.current(ctx, tx, w, m.Identity, false); e != nil {
			return e
		}
		return tx.QueryRow(ctx, `SELECT wi.phase='execution' FROM cosmoformer.attempts a JOIN cosmoformer.work_items wi ON wi.id=a.work_item_id WHERE a.id=$1 AND a.worker_id=$2 AND a.generation=$3 AND a.agent_instance=$4`, m.AttemptID, w.ID, m.Generation, m.Agent).Scan(&native)
	}); e != nil {
		return nil, e
	}
	if native {
		return c.executionTransition(ctx, token, route, m)
	}
	var out runs.Transition
	var conflict bool
	e := c.workerTx(ctx, token, func(tx pgx.Tx, w Worker) error {
		if e := c.current(ctx, tx, w, m.Identity, false); e != nil {
			return e
		}
		var state, op, work, stack string
		var generation int64
		var agent, worker string
		var closed, permitted bool
		var revision int64
		var deadline int64
		var instance string
		e := tx.QueryRow(ctx, `SELECT state,operation_id::text,work_item_id::text,stack_id::text,generation,agent_instance::text,worker_id::text,closed_at IS NOT NULL,ever_permitted,revision,deadline_tick,budget_platform_instance::text FROM cosmoformer.attempts WHERE id=$1`, m.AttemptID).Scan(&state, &op, &work, &stack, &generation, &agent, &worker, &closed, &permitted, &revision, &deadline, &instance)
		if e != nil {
			return store.ErrNotFound
		}
		if worker != w.ID || generation != m.Generation || agent != m.Agent {
			return store.ErrForbidden
		}
		payloadDigest := digest(struct {
			Route   string       `json:"route"`
			Message runs.Message `json:"message"`
		}{route, m})
		var oldDigest, old []byte
		e = tx.QueryRow(ctx, `SELECT digest,payload FROM cosmoformer.attempt_messages WHERE attempt_id=$1 AND (sequence=$2 OR message_id=$3)`, m.AttemptID, m.Seq, m.MessageID).Scan(&oldDigest, &old)
		if e == nil {
			if identity.Equal(oldDigest, payloadDigest) {
				return json.Unmarshal(old, &out)
			}
			conflict = true
			if !closed {
				_, e = tx.Exec(ctx, `UPDATE cosmoformer.attempts SET state='uncertain',revision=revision+1 WHERE id=$1`, m.AttemptID)
				if e != nil {
					return e
				}
				_, e = tx.Exec(ctx, `UPDATE cosmoformer.operations SET status='needs_attention',revision=revision+1 WHERE id=$1`, op)
			}
			return e
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		if closed {
			return runs.Fail("ATTEMPT_CLOSED")
		}
		var expected int64
		if e = tx.QueryRow(ctx, `SELECT coalesce(max(sequence),0)+1 FROM cosmoformer.attempt_messages WHERE attempt_id=$1`, m.AttemptID).Scan(&expected); e != nil {
			return e
		}
		if expected != m.Seq {
			return &runs.Error{Code: "EXPECTED_SEQUENCE", ExpectedSeq: expected}
		}
		if permitted {
			return runs.Fail("INVALID_STATE")
		}
		out = runs.Transition{Accepted: true, NextSeq: m.Seq + 1, AttemptState: state, OperationStatus: "preparing", Revision: revision + 1}
		switch route {
		case "ack":
			var p struct {
				WorkItemID string `json:"work_item_id"`
			}
			if runs.Decode(m.Payload, &p) != nil || p.WorkItemID != work || state != "claimed" {
				return runs.Fail("INVALID_STATE")
			}
			out.AttemptState = "preparing"
		case "controls/ack":
			var p runs.ControlAck
			if e = runs.Decode(m.Payload, &p); e != nil {
				return e
			}
			if e = c.controlAck(ctx, tx, m, p); e != nil {
				return e
			}
		case "prepare-result":
			if e = c.canLaunch(ctx, tx, m.AttemptID, op); e != nil {
				return e
			}
			var p runs.Prepare
			if runs.Decode(m.Payload, &p) != nil || state != "preparing" || p.Phase != "preflight" || !p.NoStateChild || !p.Reaped || p.EverPermitted {
				return runs.Fail("INVALID_STATE")
			}
			if p.Result == "failure" {
				return runs.Fail("PREPARATION_FAILED_USE_RESULT")
			}
			if p.Result != "success" || len(p.Digest) != 64 {
				return runs.Fail("INVALID_REQUEST")
			}
			a, e := c.assignment(ctx, tx, m.AttemptID)
			if e != nil {
				return e
			}
			if e = c.current(ctx, tx, w, m.Identity, true); e != nil {
				return e
			}
			if e = c.authority(ctx, tx, op, a.Spec); e != nil {
				return e
			}
			tick, e := c.Clock.Now()
			if e != nil || instance != c.Instance || int64(tick) >= deadline {
				return runs.Fail("ATTEMPT_BUDGET_INSTANCE_ENDED")
			}
			// Independently derive claims from literal attributes rather than trusting
			// a worker-supplied namespace, profile or role.
			verified, e := source.CanonicalBackend(p.Backend.Attributes, source.BackendContext{Engine: catalog.Engine{Name: a.Spec.Engine.Name, Version: a.Spec.Engine.Version}, Workspace: a.Spec.Workspace, ExpectedOwner: a.Spec.BackendOwnerAccountID}, p.Backend.TableARN)
			if e != nil || hexDigest(verified) != hexDigest(p.Backend) {
				return runs.Fail("BACKEND_MISMATCH")
			}
			if a.Spec.ExpectedBinding != nil && hexDigest(*a.Spec.ExpectedBinding) != hexDigest(verified) {
				return runs.Fail("BACKEND_MISMATCH")
			}
			for _, claim := range verified.Claims {
				found := false
				for _, grant := range a.Spec.Grants {
					if claim.Kind == "s3_object" && grant.Kind == "s3_object" && grant.Namespace == claim.Namespace && grant.Key == claim.Key {
						found = true
					}
					if claim.Kind == "ddb_item" && grant.Kind == "ddb_table" && grant.Namespace == claim.Namespace {
						found = true
					}
				}
				if !found {
					return store.ErrForbidden
				}
			}
			var binding []byte
			e = tx.QueryRow(ctx, `SELECT cosmoformer.bind_backend($1,$2,$3,$4,$5,$6,$7)`, w.OrgID, stack, verified.Target.Partition, verified.Target.Bucket, verified.Target.Key, verified.Profile, nullable(verified.TableARN)).Scan(&binding)
			if e != nil {
				return store.ErrConflict
			}
			if _, e = tx.Exec(ctx, `UPDATE cosmoformer.work_items SET resolved_preflight=$2,revision=revision+1 WHERE id=$1`, work, mustJSON(p)); e != nil {
				return e
			}
			out.AttemptState = "ready"
			out.Ready = true
		case "result":
			if state == "uncertain" {
				return runs.Fail("INDEPENDENT_STOP_PROOF_REQUIRED")
			}
			var cancelled bool
			if e = tx.QueryRow(ctx, `SELECT cancel_requested FROM cosmoformer.operations WHERE id=$1`, op).Scan(&cancelled); e != nil {
				return e
			}
			var p runs.Result
			if runs.Decode(m.Payload, &p) != nil || !p.NoStateChild || !p.Reaped || (p.Outcome != "success" && p.Outcome != "preparation_failed" && p.Outcome != "cancelled") {
				return runs.Fail("INVALID_REQUEST")
			}
			switch p.Reason {
			case "", "SOURCE_CHECKOUT_FAILED", "MANIFEST_INVALID", "DECLARATION_MISMATCH", "SOURCE_INSPECTION_FAILED", "ATTEMPT_DIRECTORY_UNAVAILABLE", "CREDENTIAL_HELPER_UNAVAILABLE":
			default:
				return runs.Fail("INVALID_REQUEST")
			}
			if p.Outcome == "success" {
				if state != "ready" {
					return runs.Fail("INVALID_STATE")
				}
				var prepared runs.Prepare
				var raw []byte
				if e = tx.QueryRow(ctx, `SELECT resolved_preflight FROM cosmoformer.work_items WHERE id=$1`, work).Scan(&raw); e != nil {
					return e
				}
				json.Unmarshal(raw, &prepared)
				if prepared.Digest != p.Digest {
					return runs.Fail("MESSAGE_CONFLICT")
				}
			}
			// Source checks and reconciliation remain source-only. Only immutable
			// explicit native intent can create the second phase.
			nativeQueued := false
			nativeBlocked := ""
			if p.Outcome == "success" && !cancelled {
				var native bool
				if e = tx.QueryRow(ctx, `SELECT native_planning FROM cosmoformer.operations WHERE id=$1`, op).Scan(&native); e != nil {
					return e
				}
				if native {
					assigned, e := c.assignment(ctx, tx, m.AttemptID)
					if e != nil {
						return e
					}
					if e = c.authority(ctx, tx, op, assigned.Spec); e != nil {
						return e
					}
					var prepared runs.Prepare
					var raw []byte
					if e = tx.QueryRow(ctx, `SELECT resolved_preflight FROM cosmoformer.work_items WHERE id=$1`, work).Scan(&raw); e != nil {
						return e
					}
					if json.Unmarshal(raw, &prepared) != nil {
						return runs.Fail("INVALID_SPEC")
					}
					execution, e := ResolveExecutionSpec(ctx, tx, assigned.Spec, prepared.Backend)
					if e != nil {
						return e
					}
					execution.RuleVersion = plan.RuleVersion
					execution.ProfileID = plan.ProfileID(execution.Engine.Name)
					if e = execution.Validate(); e != nil {
						return e
					}
					if kms, _ := execution.Backend.Attributes["kms_key_id"].(string); kms != "" {
						nativeBlocked = "KMS_GRANT_PREREQUISITES_UNAVAILABLE"
					} else {
						if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.work_items(org_id,operation_id,phase,status,assignment_spec) VALUES($1,$2,'execution','queued',$3)`, w.OrgID, op, mustJSON(execution)); e != nil {
							return e
						}
						nativeQueued = true
					}
				}
			}
			out.AttemptState = "terminal"
			out.OperationStatus = "succeeded"
			outcome := "success"
			if p.Outcome != "success" {
				out.OperationStatus = "failed"
				outcome = "preparation_failed"
			}
			if cancelled {
				outcome = "cancelled"
				out.OperationStatus = "cancelled"
			}
			if _, e = tx.Exec(ctx, `UPDATE cosmoformer.attempts SET state='terminal',outcome=$2,closed_at=clock_timestamp(),stop_proof=$3,revision=revision+1 WHERE id=$1`, m.AttemptID, outcome, mustJSON(p)); e != nil {
				return e
			}
			if p.LogFinal != nil {
				f := *p.LogFinal
				if f.Stream != "console" || f.LastSequence < -1 || f.LastSequence > 9007199254740991 || f.Complete && f.Unknown {
					return runs.Fail("INVALID_REQUEST")
				}
				var last int64
				var gaps bool
				if e = tx.QueryRow(ctx, `SELECT coalesce(max(sequence),-1),coalesce(bool_or(gap),false) FROM cosmoformer.log_segments WHERE attempt_id=$1 AND stream='console'`, m.AttemptID).Scan(&last, &gaps); e != nil {
					return e
				}
				if f.LastSequence != last || f.Complete && gaps {
					return runs.Fail("INVALID_REQUEST")
				}
			}
			if p.LogFinal == nil || p.LogFinal.Unknown {
				if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.log_segments(org_id,attempt_id,stream,sequence,first_offset,last_offset,gap,gap_reason,unknown_extent) SELECT org_id,id,'console',coalesce((SELECT max(sequence)+1 FROM cosmoformer.log_segments WHERE attempt_id=$1),0),coalesce((SELECT max(last_offset) FROM cosmoformer.log_segments WHERE attempt_id=$1),0),coalesce((SELECT max(last_offset) FROM cosmoformer.log_segments WHERE attempt_id=$1),0),true,'worker_final_unknown',true FROM cosmoformer.attempts WHERE id=$1`, m.AttemptID); e != nil {
					return e
				}
			}
			if p.LogFinal != nil {
				if _, e = tx.Exec(ctx, `UPDATE cosmoformer.attempts SET log_final=$2 WHERE id=$1`, m.AttemptID, mustJSON(p.LogFinal)); e != nil {
					return e
				}
			}
			if _, e = tx.Exec(ctx, `UPDATE cosmoformer.artifact_holds SET released_at=clock_timestamp() WHERE owner_kind='attempt' AND owner_id=$1 AND released_at IS NULL`, m.AttemptID); e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, `DELETE FROM cosmoformer.worker_reservations WHERE worker_id=$1 AND attempt_id=$2 AND generation=$3 AND agent_instance=$4`, w.ID, m.AttemptID, m.Generation, m.Agent); e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, `UPDATE cosmoformer.work_items SET status='done',revision=revision+1 WHERE id=$1`, work); e != nil {
				return e
			}
			reason := "PREFLIGHT_ONLY"
			if nativeQueued {
				out.OperationStatus = "queued"
				reason = "NATIVE_PLANNING_QUEUED"
			}
			if nativeBlocked != "" {
				out.OperationStatus = "failed"
				reason = nativeBlocked
			}
			if p.Outcome != "success" {
				reason = p.Reason
				if reason == "" {
					reason = "PREPARATION_FAILED"
				}
			}
			if cancelled {
				reason = "CANCELLED"
			}
			if _, e = tx.Exec(ctx, `UPDATE cosmoformer.operations SET status=$2,terminal_reason=$3,revision=revision+1 WHERE id=$1`, op, out.OperationStatus, reason); e != nil {
				return e
			}
		default:
			return runs.Fail("INVALID_REQUEST")
		}
		if out.AttemptState != "terminal" {
			if _, e = tx.Exec(ctx, `UPDATE cosmoformer.attempts SET state=$2,revision=revision+1 WHERE id=$1`, m.AttemptID, out.AttemptState); e != nil {
				return e
			}
		}
		_, e = tx.Exec(ctx, `INSERT INTO cosmoformer.attempt_messages(org_id,attempt_id,sequence,message_id,digest,payload,command_ordinal,kind) VALUES($1,$2,$3,$4,$5,$6,0,$7)`, w.OrgID, m.AttemptID, m.Seq, m.MessageID, payloadDigest, mustJSON(out), route)
		return e
	})
	if e == nil && conflict {
		e = runs.Fail("MESSAGE_CONFLICT")
	}
	return &out, e
}
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// WithAttempt scopes read-like capabilities without consuming transition seq.
func (c *Coordinator) WithAttempt(ctx context.Context, token string, id runs.Identity, attempt string, fn func(pgx.Tx, Worker, runs.Assignment) error) error {
	return c.workerTx(ctx, token, func(tx pgx.Tx, w Worker) error {
		if e := c.current(ctx, tx, w, id, true); e != nil {
			return e
		}
		var valid bool
		e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.attempts WHERE id=$1 AND worker_id=$2 AND generation=$3 AND agent_instance=$4 AND closed_at IS NULL AND state<>'uncertain')`, attempt, w.ID, id.Generation, id.Agent).Scan(&valid)
		if e != nil {
			return e
		}
		if !valid {
			return store.ErrForbidden
		}
		a, e := c.assignment(ctx, tx, attempt)
		if e != nil {
			return e
		}
		tick, e := c.Clock.Now()
		if e != nil {
			return e
		}
		if e = c.remaining(ctx, tx, a, tick); e != nil {
			return e
		}
		if a.RemainingMS <= 0 {
			return runs.Fail("ATTEMPT_BUDGET_INSTANCE_ENDED")
		}
		if a.Execution != nil {
			e = c.executionAuthority(ctx, tx, *a)
		} else {
			e = c.authority(ctx, tx, a.OperationID, a.Spec)
		}
		if e != nil {
			return e
		}
		return fn(tx, w, *a)
	})
}

// BuildSpec resolves administrator-selected references and immutable revision
// facts. It never returns plaintext secrets or assumed-role credentials.
func BuildSpec(ctx context.Context, tx pgx.Tx, s catalog.Stack, repo string) (runs.Effective, error) {
	out := runs.Effective{Declaration: s, Grants: []runs.ResourceRef{}, Secrets: []runs.SecretRef{}, Credentials: []runs.SourceCredential{}}
	var min int
	var branch string
	var enabled bool
	e := tx.QueryRow(ctx, `SELECT p.minimum_approvals,r.tracked_branch,r.enabled FROM cosmoformer.projects p JOIN cosmoformer.repositories r ON r.project_id=p.id WHERE p.id=$1 AND r.id=$2 AND NOT p.archived AND EXISTS(SELECT FROM cosmoformer.environments WHERE id=$3 AND project_id=p.id AND NOT archived)`, s.ProjectID, repo, s.EnvironmentID).Scan(&min, &branch, &enabled)
	if e != nil || !enabled || branch != s.TrackedBranch {
		return out, store.ErrForbidden
	}
	out.Declaration.Approvals.Minimum = max(min, s.Approvals.Minimum)
	e = tx.QueryRow(ctx, `SELECT id::text,engine,version,architecture,image_digest FROM cosmoformer.runtime_catalog WHERE engine=$1 AND version=$2 AND architecture='amd64' AND approved AND NOT withdrawn ORDER BY id LIMIT 1`, s.Engine.Name, s.Engine.Version).Scan(&out.Runtime.ID, &out.Runtime.Name, &out.Runtime.Version, &out.Runtime.Arch, &out.Runtime.ImageDigest)
	if e != nil {
		return out, store.ErrForbidden
	}
	rows, e := tx.Query(ctx, `SELECT id::text,kind,coalesce(resource_id::text,''),revision,coalesce(namespace,''),coalesce(resource_key,'') FROM cosmoformer.resource_grants WHERE project_id=$1 AND (environment_id IS NULL OR environment_id=$2) AND enabled ORDER BY id`, s.ProjectID, s.EnvironmentID)
	if e != nil {
		return out, e
	}
	for rows.Next() {
		var r runs.ResourceRef
		if e = rows.Scan(&r.ID, &r.Kind, &r.ResourceID, &r.Revision, &r.Namespace, &r.Key); e != nil {
			rows.Close()
			return out, e
		}
		out.Grants = append(out.Grants, r)
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		return out, e
	}
	repoOK, poolOK := false, false
	for _, g := range out.Grants {
		if g.Kind == "repository" && g.ResourceID == repo {
			repoOK = true
		}
		if g.Kind == "pool" && g.ResourceID == s.Worker.ID {
			poolOK = true
		}
	}
	if !repoOK || !poolOK {
		return out, store.ErrForbidden
	}
	for _, secret := range s.Secrets {
		allowed := false
		for _, g := range out.Grants {
			if g.Kind == "secret" && g.ResourceID == secret.Source.ID {
				allowed = true
			}
		}
		if !allowed {
			return out, store.ErrForbidden
		}
		var revision int64
		var project string
		e = tx.QueryRow(ctx, `SELECT semantic_revision,coalesce(project_id::text,'') FROM cosmoformer.secrets WHERE id=$1 AND enabled`, secret.Source.ID).Scan(&revision, &project)
		if e != nil || (secret.Source.Scope == "org" && project != "") || (secret.Source.Scope == "project" && project != s.ProjectID) {
			return out, store.ErrForbidden
		}
		out.Secrets = append(out.Secrets, runs.SecretRef{ID: secret.Source.ID, Scope: secret.Source.Scope, Revision: revision, Destination: secret.Env})
	}
	// An AWS connection reference is metadata only during source preflight.
	e = tx.QueryRow(ctx, `SELECT id::text,revision,role_arn,expected_account FROM cosmoformer.aws_connections WHERE project_id=$1 AND (environment_id IS NULL OR environment_id=$2) AND enabled ORDER BY environment_id NULLS LAST LIMIT 1`, s.ProjectID, s.EnvironmentID).Scan(&out.AWS.ID, &out.AWS.Revision, &out.AWS.RoleARN, &out.AWS.Account)
	if e != nil {
		return out, store.ErrForbidden
	}
	repos := []string{}
	for _, g := range out.Grants {
		if g.Kind == "repository" {
			repos = append(repos, g.ResourceID)
		}
	}
	rows, e = tx.Query(ctx, `SELECT id::text,repository_id::text,semantic_revision FROM cosmoformer.source_credentials WHERE repository_id=ANY($1::uuid[]) AND enabled ORDER BY id`, repos)
	if e != nil {
		return out, e
	}
	for rows.Next() {
		var r runs.SourceCredential
		if e = rows.Scan(&r.ID, &r.RepositoryID, &r.Revision); e != nil {
			rows.Close()
			return out, e
		}
		out.Credentials = append(out.Credentials, r)
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		return out, e
	}
	limits := catalog.Limits{ProjectID: s.ProjectID, Environments: []string{s.EnvironmentID}, Branch: branch, Minimum: min, AllowPRApply: true, Engines: []catalog.Engine{{Name: out.Runtime.Name, Version: out.Runtime.Version}}}
	for _, g := range out.Grants {
		if g.Kind == "pool" {
			limits.Pools = append(limits.Pools, g.ResourceID)
		}
	}
	for _, secret := range out.Secrets {
		limits.Secrets = append(limits.Secrets, catalog.SecretSource{Scope: secret.Scope, ID: secret.ID})
	}
	out.Declaration, e = limits.Resolve(s)
	return out, e
}
func Spec(e runs.Effective, revision, repo, url, commit, subject string, facts int64) runs.PreflightSpec {
	s := e.Declaration
	poolRev := int64(0)
	for _, g := range e.Grants {
		if g.Kind == "pool" && g.ResourceID == s.Worker.ID {
			poolRev = g.Revision
		}
	}
	return runs.PreflightSpec{Version: 1, Phase: "preflight", StackRevisionID: revision, Source: runs.Source{RepositoryID: repo, URL: url, Commit: commit, Root: s.Root, SubjectID: subject, FactsRevision: facts}, Engine: e.Runtime, Workspace: s.Workspace, BackendOwnerAccountID: s.BackendOwnerAccountID, VarFiles: s.VarFiles, BackendFiles: s.BackendFiles, Pool: runs.Pool{ID: s.Worker.ID, Tags: s.Worker.Tags, GrantRevision: poolRev}, Secrets: e.Secrets, SourceCredentials: e.Credentials, Grants: e.Grants, Review: runs.ReviewPolicy{Minimum: s.Approvals.Minimum, Revision: 1, AllowPRApply: s.AllowPRApply}, RuleVersion: "none", ProfileID: "source-only-v1", AWS: e.AWS}
}

// WithEvidenceAttempt accepts custody/log reports from the exact still-reserved
// process even after a deadline, pause, or authority revocation. Reporting does
// not grant permission to start a command or renew any lease.
func (c *Coordinator) WithEvidenceAttempt(ctx context.Context, token string, id runs.Identity, attempt string, fn func(pgx.Tx, Worker, runs.Assignment) error) error {
	return c.workerTx(ctx, token, func(tx pgx.Tx, w Worker) error {
		if e := c.current(ctx, tx, w, id, false); e != nil {
			return e
		}
		var valid bool
		if e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.attempts a JOIN cosmoformer.worker_reservations r ON r.attempt_id=a.id AND r.worker_id=a.worker_id AND r.generation=a.generation AND r.agent_instance=a.agent_instance WHERE a.id=$1 AND a.worker_id=$2 AND a.generation=$3 AND a.agent_instance=$4 AND a.closed_at IS NULL)`, attempt, w.ID, id.Generation, id.Agent).Scan(&valid); e != nil {
			return e
		}
		if !valid {
			return store.ErrForbidden
		}
		a, e := c.assignment(ctx, tx, attempt)
		if e != nil {
			return e
		}
		return fn(tx, w, *a)
	})
}

func planningPhases(phases []string) bool {
	return len(phases) == 1 && phases[0] == "preflight" || len(phases) == 2 && phases[0] == "preflight" && phases[1] == "execution"
}
func executionSpecJSON(s *runs.ExecutionSpec) any {
	if s == nil {
		return nil
	}
	return mustJSON(s)
}
func executionSpecHash(s *runs.ExecutionSpec) any {
	if s == nil {
		return nil
	}
	return digest(s)
}
