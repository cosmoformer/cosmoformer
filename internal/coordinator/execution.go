package coordinator

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/access"
	"github.com/cosmoformer/cosmoformer/internal/bitbucket"
	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/clock"
	"github.com/cosmoformer/cosmoformer/internal/evidence"
	"github.com/cosmoformer/cosmoformer/internal/identity"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/source"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/jackc/pgx/v5"
)

// NativeEvidenceVerifier is the exact native schema/profile boundary. The
// planning pipeline installs the verified profile; protocol tests inject I/O.
type NativeEvidenceVerifier interface {
	VerifySchema(context.Context, runs.ExecutionSpec, runs.Manifest, []byte) error
}

// executionAuthority checks current scope, authority, input revisions and binding.
// It performs no external I/O and cannot extend an immutable assignment.
func (c *Coordinator) executionAuthority(ctx context.Context, tx pgx.Tx, a runs.Assignment) error {
	if a.Execution == nil || a.Execution.Validate() != nil {
		return runs.Fail("INVALID_SPEC")
	}
	s := *a.Execution

	if a.Kind != "plan" {
		return runs.Fail("APPLY_PREREQUISITES_UNAVAILABLE")
	}
	if kms, _ := s.Backend.Attributes["kms_key_id"].(string); kms != "" {
		return runs.Fail("KMS_GRANT_PREREQUISITES_UNAVAILABLE")
	}
	var compatible bool
	if e := tx.QueryRow(ctx, `SELECT assessment='compatible' FROM cosmoformer.stacks WHERE id=$1`, a.StackID).Scan(&compatible); e != nil {
		return e
	}
	if !compatible {
		return runs.Fail("COMPANY_FIT_NOT_COMPATIBLE")
	}
	if e := c.authority(ctx, tx, a.OperationID, s.Preflight()); e != nil {
		return e
	}
	var project, env, requester, automation, kind string
	var floor int
	var revision int64
	e := tx.QueryRow(ctx, `SELECT st.project_id::text,st.environment_id::text,coalesce(o.requester_id::text,''),coalesce(o.automation_repository_id::text,''),o.kind,p.minimum_approvals,p.approval_policy_revision FROM cosmoformer.operations o JOIN cosmoformer.stacks st ON st.id=o.stack_id JOIN cosmoformer.projects p ON p.id=st.project_id JOIN cosmoformer.environments env ON env.id=st.environment_id WHERE o.id=$1 AND NOT p.archived AND NOT env.archived AND EXISTS(SELECT FROM cosmoformer.runs r WHERE r.id=o.run_id AND r.source_subject_id=$2 AND r.source_commit=$3 AND r.source_facts_revision=$4)`, a.OperationID, s.Source.SubjectID, s.Source.Commit, s.Source.FactsRevision).Scan(&project, &env, &requester, &automation, &kind, &floor, &revision)
	if e != nil {
		return runs.Fail("INPUTS_CHANGED")
	}
	if revision != s.Review.Revision || floor > s.Review.Minimum {
		return runs.Fail("INPUTS_CHANGED")
	}
	if requester != "" {
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
	} else {
		var valid bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.repositories WHERE id=$1 AND id=$2 AND enabled AND project_id=$3)`, automation, s.Source.RepositoryID, project).Scan(&valid); e != nil {
			return e
		}
		if !valid {
			return store.ErrForbidden
		}
	}
	// Compare the exact grant facts, not merely an enabled row with the same ID.
	for _, g := range s.Grants {
		var valid bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.resource_grants WHERE id=$1 AND revision=$2 AND enabled AND project_id=$3 AND (environment_id IS NULL OR environment_id=$4) AND kind=$5 AND coalesce(resource_id::text,'')=$6 AND coalesce(namespace,'')=$7 AND coalesce(resource_key,'')=$8)`, g.ID, g.Revision, project, env, g.Kind, g.ResourceID, g.Namespace, g.Key).Scan(&valid); e != nil {
			return e
		}
		if !valid {
			return runs.Fail("INPUTS_CHANGED")
		}
	}
	granted := func(kind, id string) bool {
		for _, g := range s.Grants {
			if g.Kind == kind && g.ResourceID == id {
				return true
			}
		}
		return false
	}
	if !granted("pool", s.Pool.ID) || !granted("repository", s.Source.RepositoryID) {
		return store.ErrForbidden
	}
	for _, r := range s.Secrets {
		if !granted("secret", r.ID) {
			return store.ErrForbidden
		}
		var valid bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.secrets WHERE id=$1 AND semantic_revision=$2 AND enabled AND (($3='org' AND project_id IS NULL) OR ($3='project' AND project_id=$4)))`, r.ID, r.Revision, r.Scope, project).Scan(&valid); e != nil {
			return e
		}
		if !valid {
			return runs.Fail("INPUTS_CHANGED")
		}
	}
	for _, r := range s.SourceCredentials {
		if !granted("repository", r.RepositoryID) {
			return store.ErrForbidden
		}
	}
	var valid bool
	e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.aws_connections WHERE id=$1 AND revision=$2 AND enabled AND expected_bootstrap_principal=$3 AND session_seconds=$4) AND EXISTS(SELECT FROM cosmoformer.repositories WHERE id=$5 AND url=$6 AND enabled) AND EXISTS(SELECT FROM cosmoformer.runtime_catalog WHERE id=$7 AND engine=$8 AND version=$9 AND architecture=$10 AND image_digest=$11 AND approved AND NOT withdrawn)`, s.AWS.ID, s.AWS.Revision, s.AWS.Bootstrap, s.AWS.SessionSeconds, s.Source.RepositoryID, s.Source.URL, s.Engine.ID, s.Engine.Name, s.Engine.Version, s.Engine.Arch, s.Engine.ImageDigest).Scan(&valid)
	if e != nil {
		return e
	}
	if !valid {
		return runs.Fail("INPUTS_CHANGED")
	}
	if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.workers w JOIN cosmoformer.attempts t ON t.worker_id=w.id WHERE t.id=$1 AND w.runtime_id=$2 AND w.pool_id=$3 AND w.tags @> $4::text[])`, a.AttemptID, s.Engine.ID, s.Pool.ID, s.Pool.Tags).Scan(&valid); e != nil {
		return e
	}
	if !valid {
		return runs.Fail("STALE_AGENT")
	}
	// Read-only boolean check; authority revalidation must not add permanent
	// backend claims or reveal installation-wide target records.
	if e = tx.QueryRow(ctx, `SELECT cosmoformer.execution_binding_matches($1,$2,$3,$4,$5,$6) AND EXISTS(SELECT FROM cosmoformer.plans WHERE attempt_id=$1 AND status='assembling' AND state_epoch=cosmoformer.execution_target_epoch($1) AND profile_id=$7 AND rule_version=$8)`, a.AttemptID, s.Backend.Target.Partition, s.Backend.Target.Bucket, s.Backend.Target.Key, s.Backend.Profile, nullable(s.Backend.TableARN), s.ProfileID, s.RuleVersion).Scan(&valid); e != nil {
		return e
	}
	if !valid {
		return runs.Fail("BACKEND_MISMATCH")
	}
	for _, claim := range s.Backend.Claims {
		found := false
		for _, g := range s.Grants {
			if claim.Kind == "s3_object" && g.Kind == "s3_object" && g.Namespace == claim.Namespace && g.Key == claim.Key || claim.Kind == "ddb_item" && g.Kind == "ddb_table" && g.Namespace == claim.Namespace {
				found = true
			}
		}
		if !found {
			return store.ErrForbidden
		}
	}
	return nil
}
func (c *Coordinator) ExecutionInputs(ctx context.Context, token, attempt string, in runs.ExecutionInputs, receive func(pgx.Tx, Worker, runs.ExecutionSpec) error) error {
	if !catalog.UUID(in.RequestID) || !runs.ValidDigest(in.SpecDigest) {
		return runs.Fail("INVALID_REQUEST")
	}
	return c.WithAttempt(ctx, token, in.Identity, attempt, func(tx pgx.Tx, w Worker, a runs.Assignment) error {
		if a.Execution == nil {
			return store.ErrForbidden
		}
		if in.SpecDigest != a.Digest || runs.Digest(in.Secrets) != runs.Digest(a.Execution.ExpectedSecrets()) {
			return runs.Fail("INPUTS_CHANGED")
		}
		var cancelled bool
		if e := tx.QueryRow(ctx, `SELECT cancel_requested OR EXISTS(SELECT FROM cosmoformer.control_requests WHERE attempt_id=$2) FROM cosmoformer.operations WHERE id=$1`, a.OperationID, attempt).Scan(&cancelled); e != nil {
			return e
		}
		if cancelled {
			return runs.Fail("CANCEL_REQUESTED")
		}
		return receive(tx, w, *a.Execution)
	})
}

type executionEvidence struct {
	Pre      *runs.PreInitSpec
	Manifest *runs.Manifest
	Lock     []byte
	Records  []evidence.Artifact
	Planning *planningEvidence
}

func (c *Coordinator) readExecutionArtifact(ctx context.Context, token string, m runs.Message, id, kind string) (evidence.Artifact, []byte, error) {
	if !catalog.UUID(id) {
		return evidence.Artifact{}, nil, runs.Fail("EVIDENCE_REQUIRED")
	}
	auth := func(fn func(pgx.Tx) error) error {
		return c.WithEvidenceAttempt(ctx, token, m.Identity, m.AttemptID, func(tx pgx.Tx, _ Worker, a runs.Assignment) error {
			if a.Execution == nil {
				return store.ErrForbidden
			}
			var valid bool
			if e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.artifacts WHERE id=$1 AND attempt_id=$2 AND kind=$3 AND state='finalized' AND NOT EXISTS(SELECT FROM cosmoformer.cleanup_claims WHERE artifact_id=$1))`, id, m.AttemptID, kind).Scan(&valid); e != nil {
				return e
			}
			if !valid {
				return runs.Fail("EVIDENCE_REQUIRED")
			}
			return fn(tx)
		})
	}
	a, f, e := c.Evidence.Read(ctx, auth, id)
	if e != nil {
		return a, nil, runs.Fail("EVIDENCE_UNAVAILABLE")
	}
	defer func() { f.Close(); os.Remove(f.Name()) }()
	b, e := io.ReadAll(io.LimitReader(f, 67108865))
	if e != nil || len(b) > 67108864 {
		return a, nil, runs.Fail("INVALID_MANIFEST")
	}
	return a, b, nil
}
func canonicalEvidence(b []byte, v any) error {
	if runs.Decode(b, v) != nil {
		return runs.Fail("INVALID_MANIFEST")
	}
	canonical, e := catalog.Canonical(v)
	if e != nil || !bytes.Equal(b, canonical) {
		return runs.Fail("INVALID_MANIFEST")
	}
	return nil
}
func (c *Coordinator) executionEvidence(ctx context.Context, token, route string, m runs.Message) (executionEvidence, error) {
	var out executionEvidence
	read := func(id, kind string, v any) error {
		a, b, e := c.readExecutionArtifact(ctx, token, m, id, kind)
		if e != nil {
			return e
		}
		out.Records = append(out.Records, a)
		if v != nil {
			return canonicalEvidence(b, v)
		}
		out.Lock = b
		return nil
	}
	switch route {
	case "result":
		var p runs.ExecutionResult
		if e := runs.Decode(m.Payload, &p); e != nil {
			return out, e
		}
		if p.Outcome == "success" {
			var e error
			out.Planning, e = c.readPlanningEvidence(ctx, token, m, p)
			if e != nil {
				return out, e
			}
		}
	case "prepare-result":
		var p runs.ExecutionPrepare
		if e := runs.Decode(m.Payload, &p); e != nil {
			return out, e
		}
		if p.Result == "failure" {
			return out, nil
		}
		out.Pre = &runs.PreInitSpec{}
		if e := read(p.Artifact, "pre_init_spec", out.Pre); e != nil {
			return out, e
		}
	case "start-permission":
		var p runs.CommandRequest
		if e := runs.Decode(m.Payload, &p); e != nil {
			return out, e
		}
		if p.Command != "plan" && p.Command != "show" {
			break
		}
		kind := "provider_schema"
		ordinal := int64(2)
		if p.Command == "show" {
			kind = "binary_plan"
			ordinal = 3
		}
		var artifact, manifest string
		e := c.WithEvidenceAttempt(ctx, token, m.Identity, m.AttemptID, func(tx pgx.Tx, _ Worker, a runs.Assignment) error {
			return tx.QueryRow(ctx, `SELECT (SELECT id::text FROM cosmoformer.artifacts WHERE attempt_id=$1 AND kind=$2 AND command_ordinal=$3 AND state='finalized' ORDER BY created_at DESC,id DESC LIMIT 1),sealed_manifest_artifact_id::text FROM cosmoformer.attempts WHERE id=$1`, m.AttemptID, kind, ordinal).Scan(&artifact, &manifest)
		})
		if e != nil {
			return out, runs.Fail("COMMAND_EVIDENCE_REQUIRED")
		}
		record, b, e := c.readExecutionArtifact(ctx, token, m, artifact, kind)
		if e != nil {
			return out, e
		}
		out.Records = append(out.Records, record)
		if len(b) == 0 {
			return out, runs.Fail("COMMAND_EVIDENCE_REQUIRED")
		}
		if p.Command == "plan" {
			if c.NativeEvidence == nil {
				return out, runs.Fail("NATIVE_PROFILE_UNAVAILABLE")
			}
			out.Manifest = &runs.Manifest{}
			if e = read(manifest, "manifest", out.Manifest); e != nil {
				return out, e
			}
			var a *runs.Assignment
			if e = c.WithEvidenceAttempt(ctx, token, m.Identity, m.AttemptID, func(tx pgx.Tx, _ Worker, assigned runs.Assignment) error { a = &assigned; return nil }); e != nil {
				return out, e
			}
			if e = c.NativeEvidence.VerifySchema(ctx, *a.Execution, *out.Manifest, b); e != nil {
				return out, runs.Fail("NATIVE_PROFILE_UNSUPPORTED")
			}
		}
	case "manifest-seal":
		var p runs.ManifestSeal
		if e := runs.Decode(m.Payload, &p); e != nil {
			return out, e
		}
		var pre string
		if e := c.WithEvidenceAttempt(ctx, token, m.Identity, m.AttemptID, func(tx pgx.Tx, _ Worker, _ runs.Assignment) error {
			return tx.QueryRow(ctx, `SELECT pre_init_spec_artifact_id::text FROM cosmoformer.attempts WHERE id=$1`, m.AttemptID).Scan(&pre)
		}); e != nil {
			return out, e
		}
		out.Pre = &runs.PreInitSpec{}
		if e := read(pre, "pre_init_spec", out.Pre); e != nil {
			return out, e
		}
		out.Manifest = &runs.Manifest{}
		if e := read(p.Artifact, "manifest", out.Manifest); e != nil {
			return out, e
		}
		if p.LockArtifact != "" {
			if e := read(p.LockArtifact, "provider_lock", nil); e != nil {
				return out, e
			}
		}
	}
	return out, nil
}
func (c *Coordinator) verifyExecutionEvidence(ctx context.Context, tx pgx.Tx, attempt string, records []evidence.Artifact) error {
	for _, a := range records {
		var valid bool
		e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.artifacts WHERE id=$1 AND attempt_id=$2 AND state='finalized' AND kind=$3 AND bucket=$4 AND object_key=$5 AND version_id=$6 AND encode(sha256,'hex')=$7 AND byte_count=$8 AND NOT EXISTS(SELECT FROM cosmoformer.cleanup_claims WHERE artifact_id=$1))`, a.ID, attempt, a.Kind, a.Bucket, a.Key, a.Version, a.Hash, a.Size).Scan(&valid)
		if e != nil {
			return e
		}
		if !valid {
			return runs.Fail("EVIDENCE_UNAVAILABLE")
		}
	}
	return nil
}

func (c *Coordinator) executionReplay(ctx context.Context, token, route string, m runs.Message, payload any) (*runs.Transition, error) {
	var out *runs.Transition
	conflict := false
	e := c.workerTx(ctx, token, func(tx pgx.Tx, w Worker) error {
		if e := c.current(ctx, tx, w, m.Identity, false); e != nil {
			return e
		}
		a, e := c.assignment(ctx, tx, m.AttemptID)
		if e != nil {
			return store.ErrNotFound
		}
		if e = c.owns(ctx, tx, w, m, false); e != nil {
			return e
		}
		var d, b []byte
		e = tx.QueryRow(ctx, `SELECT digest,payload FROM cosmoformer.attempt_messages WHERE (attempt_id=$1 AND sequence=$2) OR message_id=$3`, m.AttemptID, m.Seq, m.MessageID).Scan(&d, &b)
		if e == nil {
			if !identity.Equal(d, nativeMessageDigest(route, m, payload)) {
				conflict = true
				return c.attention(ctx, tx, m.AttemptID)
			}
			out = &runs.Transition{}
			if json.Unmarshal(b, out) != nil {
				return runs.Fail("MESSAGE_CONFLICT")
			}
			if out.Permission != nil {
				if e = c.current(ctx, tx, w, m.Identity, true); e != nil {
					return e
				}
				if e = c.canLaunch(ctx, tx, m.AttemptID, a.OperationID); e != nil {
					return e
				}
				if e = c.executionAuthority(ctx, tx, *a); e != nil {
					return e
				}
				return c.permissionRemaining(ctx, tx, m.AttemptID, out.Permission)
			}
			return nil
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return e
		}
		var expected int64
		var closed bool
		if e = tx.QueryRow(ctx, `SELECT closed_at IS NOT NULL,coalesce((SELECT max(sequence)+1 FROM cosmoformer.attempt_messages WHERE attempt_id=$1),1) FROM cosmoformer.attempts WHERE id=$1`, m.AttemptID).Scan(&closed, &expected); e != nil {
			return e
		}
		if expected != m.Seq {
			return &runs.Error{Code: "EXPECTED_SEQUENCE", ExpectedSeq: expected}
		}
		if closed {
			conflict, e = c.closedExecutionConflict(ctx, tx, m.AttemptID, payload)
			if e != nil {
				return e
			}
			if conflict {
				return c.attention(ctx, tx, m.AttemptID)
			}
			return runs.Fail("ATTEMPT_CLOSED")
		}
		return nil
	})
	if e == nil && conflict {
		e = runs.Fail("MESSAGE_CONFLICT")
	}
	return out, e
}
func nativeMessageDigest(route string, m runs.Message, p any) []byte {
	return digest(struct {
		Route   string
		ID      string
		Worker  runs.Identity
		Attempt string
		Seq     int64
		Payload any
	}{route, m.MessageID, m.Identity, m.AttemptID, m.Seq, p})
}
func (c *Coordinator) owns(ctx context.Context, tx pgx.Tx, w Worker, m runs.Message, reserved bool) error {
	var valid bool
	e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.attempts a WHERE a.id=$1 AND a.worker_id=$2 AND a.generation=$3 AND a.agent_instance=$4 AND (NOT $5::boolean OR EXISTS(SELECT FROM cosmoformer.worker_reservations r WHERE r.attempt_id=a.id AND r.worker_id=a.worker_id AND r.generation=a.generation AND r.agent_instance=a.agent_instance)))`, m.AttemptID, w.ID, m.Generation, m.Agent, reserved).Scan(&valid)
	if e != nil {
		return e
	}
	if !valid {
		return store.ErrForbidden
	}
	return nil
}
func (c *Coordinator) attention(ctx context.Context, tx pgx.Tx, attempt string) error {
	// Contradictory late terminal facts preserve history but also open the
	// target hold. A released old attempt must not silently authorize new work.
	_, e := tx.Exec(ctx, `SELECT cosmoformer.execution_attention($1)`, attempt)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `INSERT INTO cosmoformer.artifact_holds(org_id,artifact_id,owner_kind,owner_id,reason) SELECT a.org_id,a.id,'recovery',r.id,'execution_uncertain' FROM cosmoformer.artifacts a JOIN cosmoformer.recovery_cases r ON r.origin_attempt_id=$1 AND r.status='open' WHERE a.attempt_id=$1 AND a.state IN ('uploading','finalized') AND NOT EXISTS(SELECT FROM cosmoformer.cleanup_claims WHERE artifact_id=a.id) ON CONFLICT DO NOTHING`, attempt)
	return e
}
func (c *Coordinator) canLaunch(ctx context.Context, tx pgx.Tx, attempt, op string) error {
	var cancelled, uncertain, held bool
	e := tx.QueryRow(ctx, `SELECT o.cancel_requested OR EXISTS(SELECT FROM cosmoformer.control_requests WHERE attempt_id=a.id),a.state='uncertain',EXISTS(SELECT FROM cosmoformer.recovery_cases WHERE target_id=a.target_id AND status='open') FROM cosmoformer.attempts a JOIN cosmoformer.operations o ON o.id=a.operation_id WHERE a.id=$1 AND o.id=$2`, attempt, op).Scan(&cancelled, &uncertain, &held)
	if e != nil {
		return e
	}
	if cancelled {
		return runs.Fail("CANCEL_REQUESTED")
	}
	if uncertain || held {
		return runs.Fail("EXECUTION_UNCERTAIN")
	}
	return nil
}
func (c *Coordinator) executionBudget(ctx context.Context, tx pgx.Tx, attempt string, credentials bool) (time.Duration, error) {
	tick, e := c.Clock.Now()
	if e != nil {
		return 0, runs.Fail("CLOCK_UNAVAILABLE")
	}
	var instance string
	var deadline, accepted int64
	var ci *string
	var cd *int64
	e = tx.QueryRow(ctx, `SELECT budget_platform_instance::text,accepted_tick,deadline_tick,credential_platform_instance::text,credential_deadline_tick FROM cosmoformer.attempts WHERE id=$1`, attempt).Scan(&instance, &accepted, &deadline, &ci, &cd)
	if e != nil {
		return 0, e
	}
	if instance != c.Instance {
		return 0, runs.Fail("ATTEMPT_BUDGET_INSTANCE_ENDED")
	}
	if int64(tick) < accepted || int64(tick) >= deadline {
		return 0, runs.Fail("ATTEMPT_EXPIRED")
	}
	if credentials && (ci == nil || *ci != c.Instance || cd == nil || *cd < deadline) {
		return 0, runs.Fail("CREDENTIAL_BUDGET_REQUIRED")
	}
	return tick, nil
}
func (c *Coordinator) permissionRemaining(ctx context.Context, tx pgx.Tx, attempt string, p *runs.Permission) error {
	if e := c.exactReservations(ctx, tx, attempt); e != nil {
		return e
	}
	if p.Platform != c.Instance {
		return runs.Fail("PERMISSION_INSTANCE_ENDED")
	}
	tick, e := c.executionBudget(ctx, tx, attempt, true)
	if e != nil {
		return e
	}
	var expiry int64
	var status string
	if e = tx.QueryRow(ctx, `SELECT expires_tick,status FROM cosmoformer.start_permissions WHERE id=$1 AND attempt_id=$2`, p.ID, attempt).Scan(&expiry, &status); e != nil {
		return e
	}
	p.RemainingMS = max(0, (expiry-int64(tick))/int64(time.Millisecond))
	if p.RemainingMS <= 0 {
		return runs.Fail("PERMISSION_EXPIRED")
	}
	if status != "issued" {
		return runs.Fail("PERMISSION_ACCOUNTED")
	}
	return nil
}

func (c *Coordinator) executionTransition(ctx context.Context, token, route string, m runs.Message) (*runs.Transition, error) {
	payload, e := runs.ExecutionPayload(route, m.Payload)
	if e != nil {
		return nil, e
	}
	if replay, e := c.executionReplay(ctx, token, route, m, payload); e != nil || replay != nil {
		return replay, e
	}
	ev, e := c.executionEvidence(ctx, token, route, m)
	if e != nil {
		return nil, e
	}
	var observation *Observation
	var facts bitbucket.Facts
	if route == "start-permission" || route == "result" && payload.(*runs.ExecutionResult).Outcome == "success" {
		// Persist clock failure independently of a rejected command transaction.
		good := false
		e = c.workerTx(ctx, token, func(tx pgx.Tx, w Worker) error {
			if e := c.current(ctx, tx, w, m.Identity, true); e != nil {
				return e
			}
			tick, e := c.Clock.Now()
			if e != nil {
				return runs.Fail("CLOCK_UNAVAILABLE")
			}
			return tx.QueryRow(ctx, `SELECT cosmoformer.execution_clock_observe($1,$2)`, c.Instance, int64(tick)).Scan(&good)
		})
		if e != nil {
			return nil, e
		}
		if !good {
			return nil, runs.Fail("CLOCK_UNRELIABLE")
		}
		stateGate := route == "result"
		if p, ok := payload.(*runs.CommandRequest); ok {
			stateGate = p.Command == "init" || p.Command == "plan" || p.Command == "apply"
		}
		if stateGate {
			if c.ObserveSource == nil {
				return nil, runs.Fail("SOURCE_OBSERVATION_REQUIRED")
			}
			var org string
			e = c.WithAttempt(ctx, token, m.Identity, m.AttemptID, func(tx pgx.Tx, w Worker, a runs.Assignment) error {
				if e := c.canLaunch(ctx, tx, m.AttemptID, a.OperationID); e != nil {
					return e
				}
				var kind, key string
				if e := tx.QueryRow(ctx, `SELECT kind,external_key FROM cosmoformer.source_subjects WHERE id=$1 AND repository_id=$2`, a.Source.SubjectID, a.Source.RepositoryID).Scan(&kind, &key); e != nil {
					return e
				}
				o, e := c.BeginObservation(ctx, tx, w.OrgID, a.Source.RepositoryID, kind, key)
				if e != nil {
					return e
				}
				observation = &o
				org = w.OrgID
				return nil
			})
			if e != nil {
				return nil, e
			}
			facts, e = c.ObserveSource(ctx, org, observation.SubjectID)
			contextChanged := errors.Is(e, bitbucket.ErrContextChanged)
			if e != nil && !contextChanged {
				return nil, runs.Fail("SOURCE_UNAVAILABLE")
			}
			// Commit authoritative changed facts even if this permission is rejected.
			e = c.workerTx(ctx, token, func(tx pgx.Tx, w Worker) error {
				if e := c.current(ctx, tx, w, m.Identity, true); e != nil {
					return e
				}
				_, e := c.AcceptObservation(ctx, tx, w.OrgID, *observation, facts)
				return e
			})
			if e != nil {
				return nil, e
			}
			if contextChanged {
				return nil, runs.Fail("SOURCE_CHANGED")
			}
		}
	}
	var out runs.Transition
	attention := false
	e = c.workerTx(ctx, token, func(tx pgx.Tx, w Worker) error {
		if e := c.current(ctx, tx, w, m.Identity, false); e != nil {
			return e
		}
		if e := c.owns(ctx, tx, w, m, false); e != nil {
			return e
		}
		a, e := c.assignment(ctx, tx, m.AttemptID)
		if e != nil {
			return e
		}
		if a.Execution == nil {
			return store.ErrForbidden
		}
		var state, stage, pre, sealed, status string
		var ordinal, revision int64
		var permitted, closed bool
		e = tx.QueryRow(ctx, `SELECT a.state,a.manifest_stage,coalesce(encode(a.pre_init_spec_digest,'hex'),''),coalesce(encode(a.sealed_manifest_digest,'hex'),''),a.command_ordinal,a.revision,a.ever_permitted,a.closed_at IS NOT NULL,o.status FROM cosmoformer.attempts a JOIN cosmoformer.operations o ON o.id=a.operation_id WHERE a.id=$1`, m.AttemptID).Scan(&state, &stage, &pre, &sealed, &ordinal, &revision, &permitted, &closed, &status)
		if e != nil {
			return e
		}
		// Evidence I/O may race an identical in-flight request. Recheck accepted
		// identity under this same gate before requiring live reservations.
		var priorDigest, priorResponse []byte
		replayErr := tx.QueryRow(ctx, `SELECT digest,payload FROM cosmoformer.attempt_messages WHERE (attempt_id=$1 AND sequence=$2) OR message_id=$3`, m.AttemptID, m.Seq, m.MessageID).Scan(&priorDigest, &priorResponse)
		if replayErr == nil {
			if !identity.Equal(priorDigest, nativeMessageDigest(route, m, payload)) {
				attention = true
				return c.attention(ctx, tx, m.AttemptID)
			}
			if e = json.Unmarshal(priorResponse, &out); e != nil {
				return e
			}
			if out.Permission != nil {
				if e = c.current(ctx, tx, w, m.Identity, true); e != nil {
					return e
				}
				if e = c.canLaunch(ctx, tx, m.AttemptID, a.OperationID); e != nil {
					return e
				}
				if e = c.executionAuthority(ctx, tx, *a); e != nil {
					return e
				}
				return c.permissionRemaining(ctx, tx, m.AttemptID, out.Permission)
			}
			return nil
		}
		if !errors.Is(replayErr, pgx.ErrNoRows) {
			return replayErr
		}
		// A second request can win while evidence/source I/O is in progress.
		var expected int64
		if e = tx.QueryRow(ctx, `SELECT coalesce(max(sequence),0)+1 FROM cosmoformer.attempt_messages WHERE attempt_id=$1`, m.AttemptID).Scan(&expected); e != nil {
			return e
		}
		if expected != m.Seq {
			return &runs.Error{Code: "EXPECTED_SEQUENCE", ExpectedSeq: expected}
		}
		if closed {
			attention, e = c.closedExecutionConflict(ctx, tx, m.AttemptID, payload)
			if e != nil {
				return e
			}
			if attention {
				return c.attention(ctx, tx, m.AttemptID)
			}
			return runs.Fail("ATTEMPT_CLOSED")
		}
		if e = c.owns(ctx, tx, w, m, true); e != nil {
			return e
		}
		out = runs.Transition{Accepted: true, NextSeq: m.Seq + 1, AttemptState: state, OperationStatus: status, Revision: revision + 1}
		if route == "prepare-result" || route == "manifest-seal" || route == "start-permission" {
			if e = c.current(ctx, tx, w, m.Identity, true); e != nil {
				return e
			}
			if e = c.canLaunch(ctx, tx, m.AttemptID, a.OperationID); e != nil {
				return e
			}
			if e = c.executionAuthority(ctx, tx, *a); e != nil {
				return e
			}
			if _, e = c.executionBudget(ctx, tx, m.AttemptID, route != "prepare-result"); e != nil {
				return e
			}
			if e = c.exactReservations(ctx, tx, m.AttemptID); e != nil {
				return e
			}
		}
		switch route {
		case "ack":
			var p struct {
				Work string `json:"work_item_id"`
			}
			_ = runs.Decode(m.Payload, &p)
			if p.Work != a.WorkItemID || state != "claimed" {
				return runs.Fail("INVALID_STATE")
			}
			out.AttemptState = "preparing"
		case "prepare-result":
			p := payload.(*runs.ExecutionPrepare)
			if state != "preparing" || permitted || pre != "" {
				return runs.Fail("REPREPARE_REQUIRED")
			}
			if p.Result != "success" {
				return runs.Fail("PREPARATION_FAILED_USE_RESULT")
			}
			if p.Phase != "execution" || p.SpecDigest != a.Digest || p.Commit != a.Source.Commit || p.Bootstrap != a.Execution.AWS.Bootstrap || p.Role != a.Execution.AWS.RoleARN || p.CredentialMS <= 0 || p.CredentialMS > 3300000 || p.Reason != "" || p.NoStateChild || p.Reaped || p.EverPermitted || ev.Pre == nil || p.Digest != runs.Digest(ev.Pre) {
				return runs.Fail("REPREPARE_REQUIRED")
			}
			var epoch int64
			if e = tx.QueryRow(ctx, `SELECT cosmoformer.execution_target_epoch($1)`, m.AttemptID).Scan(&epoch); e != nil {
				return e
			}
			if e = ev.Pre.Validate(*a.Execution, a.Kind, a.StackID, epoch); e != nil {
				return e
			}
			if p.Lock.Origin != ev.Pre.LockOrigin || p.Lock.Present != (ev.Pre.LockOrigin != "absent") || p.Lock.Hash != ev.Pre.LockHash {
				return runs.Fail("PROVIDER_LOCK_INVALID")
			}
			if e = c.verifyExecutionEvidence(ctx, tx, m.AttemptID, ev.Records); e != nil {
				return e
			}
			tick, e := c.executionBudget(ctx, tx, m.AttemptID, false)
			if e != nil {
				return e
			}
			var acceptedTick int64
			if e = tx.QueryRow(ctx, `SELECT accepted_tick FROM cosmoformer.attempts WHERE id=$1`, m.AttemptID).Scan(&acceptedTick); e != nil {
				return e
			}
			_ = tick
			// Anchor earlier than the worker's report: transit/gate delay cannot renew
			// credential authority, and rate uncertainty only shortens this budget.
			deadline := acceptedTick + int64(clock.Conservative(p.CredentialMS))
			var attemptDeadline int64
			if e = tx.QueryRow(ctx, `SELECT deadline_tick FROM cosmoformer.attempts WHERE id=$1`, m.AttemptID).Scan(&attemptDeadline); e != nil {
				return e
			}
			if deadline < attemptDeadline {
				return runs.Fail("CREDENTIAL_BUDGET_REQUIRED")
			}
			_, e = tx.Exec(ctx, `UPDATE cosmoformer.attempts SET pre_init_spec_digest=decode($2,'hex'),pre_init_spec_artifact_id=$3,execution_preparation=$4,credential_platform_instance=$5,credential_deadline_tick=$6 WHERE id=$1`, m.AttemptID, p.Digest, p.Artifact, mustJSON(p), c.Instance, deadline)
			if e != nil {
				return e
			}
			out.AttemptState = "ready"
			out.Ready = true
			out.NextCommandOrdinal = 1
		case "manifest-seal":
			p := payload.(*runs.ManifestSeal)
			if stage == "sealed" {
				var oldManifest, oldLock string
				if e = tx.QueryRow(ctx, `SELECT sealed_manifest_artifact_id::text,coalesce(provider_lock_artifact_id::text,'') FROM cosmoformer.attempts WHERE id=$1`, m.AttemptID).Scan(&oldManifest, &oldLock); e != nil {
					return e
				}
				if p.PreInitDigest != pre || p.Digest != sealed || p.Artifact != oldManifest || p.LockArtifact != oldLock || ev.Manifest == nil || runs.Digest(ev.Manifest.Providers) != runs.Digest(p.Providers) {
					return runs.Fail("MANIFEST_CONFLICT")
				}
				out.Ready = true
				out.NextCommandOrdinal = 2
				break
			}
			if state != "ready" || ordinal != 1 || pre == "" || p.PreInitDigest != pre || ev.Pre == nil || runs.Digest(ev.Pre) != pre || ev.Manifest == nil || runs.Digest(ev.Manifest) != p.Digest || runs.Digest(ev.Pre.ReviewedInputs) != runs.Digest(ev.Manifest.ReviewedInputs) {
				return runs.Fail("MANIFEST_STAGE")
			}
			var stopped bool
			if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.start_permissions WHERE attempt_id=$1 AND ordinal=1 AND command='init' AND status='stopped' AND (stop_facts->>'exit_code')::int=0)`, m.AttemptID).Scan(&stopped); e != nil {
				return e
			}
			if !stopped {
				return runs.Fail("INIT_STOP_REQUIRED")
			}
			if ev.Manifest.Lock.Present != (p.LockArtifact != "") {
				return runs.Fail("PROVIDER_LOCK_REQUIRED")
			}
			if e = ev.Manifest.ValidateLock(); e != nil {
				return e
			}
			if runs.Digest(p.Providers) != runs.Digest(ev.Manifest.Providers) {
				return runs.Fail("PROVIDER_LOCK_INVALID")
			}
			if e = verifyProviderLock(ev.Manifest, ev.Lock); e != nil {
				return e
			}
			if ev.Pre.LockOrigin != "absent" && ev.Manifest.Lock.Hash != ev.Pre.LockHash {
				return runs.Fail("PROVIDER_LOCK_CHANGED")
			}
			if a.Kind == "apply" && (a.Execution.RetainedPlan == nil || p.Digest != a.Execution.RetainedPlan.ManifestDigest) {
				return runs.Fail("REPREPARE_REQUIRED")
			}
			var epoch int64
			if e = tx.QueryRow(ctx, `SELECT cosmoformer.execution_target_epoch($1)`, m.AttemptID).Scan(&epoch); e != nil {
				return e
			}
			if e = ev.Manifest.ReviewedInputs.Validate(*a.Execution, a.StackID, epoch); e != nil {
				return e
			}
			if e = c.verifyExecutionEvidence(ctx, tx, m.AttemptID, ev.Records); e != nil {
				return e
			}
			_, e = tx.Exec(ctx, `UPDATE cosmoformer.attempts SET manifest_stage='sealed',sealed_manifest_digest=decode($2,'hex'),sealed_manifest_artifact_id=$3,provider_lock_artifact_id=$4 WHERE id=$1`, m.AttemptID, p.Digest, p.Artifact, nullable(p.LockArtifact))
			if e != nil {
				return e
			}
			out.Ready = true
			out.NextCommandOrdinal = 2
		case "start-permission":
			p := payload.(*runs.CommandRequest)
			descriptor, e := runs.Command(*a.Execution, a.Kind, *p)
			if e != nil {
				return e
			}
			if state != "ready" || p.Ordinal != ordinal+1 {
				return runs.Fail("COMMAND_ORDER")
			}
			if p.PreInitDigest != pre || pre == "" || p.Ordinal == 1 && stage != "pre_init" || p.Ordinal > 1 && (stage != "sealed" || p.ManifestDigest != sealed) {
				return runs.Fail("MANIFEST_STAGE")
			}
			if runs.Digest(descriptor) != p.Digest {
				return runs.Fail("COMMAND_DIGEST_MISMATCH")
			}
			if a.Kind == "apply" {
				return runs.Fail("APPLY_PREREQUISITES_UNAVAILABLE")
			}
			if p.Ordinal > 1 {
				var okay bool
				if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.start_permissions WHERE attempt_id=$1 AND ordinal=$2 AND status='stopped' AND ((stop_facts->>'exit_code')::int=0 OR (command='plan' AND (stop_facts->>'exit_code')::int=2)))`, m.AttemptID, p.Ordinal-1).Scan(&okay); e != nil {
					return e
				}
				if !okay {
					return runs.Fail("PREVIOUS_COMMAND_FAILED")
				}
			}
			if p.Command == "plan" || p.Command == "show" {
				if e = c.verifyExecutionEvidence(ctx, tx, m.AttemptID, ev.Records); e != nil {
					return e
				}
				if p.Command == "plan" && (ev.Manifest == nil || runs.Digest(ev.Manifest) != sealed) {
					return runs.Fail("MANIFEST_STAGE")
				}
			}
			if e = c.issuePermission(ctx, tx, w, *a, m, *p, observation, &out); e != nil {
				return e
			}
		case "controls/ack":
			if e = c.controlAck(ctx, tx, m, *payload.(*runs.ControlAck)); e != nil {
				return e
			}
		case "command-events":
			p := payload.(*runs.CommandEvent)
			conflict, e := c.commandEvent(ctx, tx, *a, m, *p, &out)
			if e != nil {
				return e
			}
			if conflict {
				attention = true
				return c.attention(ctx, tx, m.AttemptID)
			}
			// Every late event may reconcile process facts, including identical
			// observations, but only recovery clearance can remove attention.
			if e = c.preserveExecutionAttention(ctx, tx, m.AttemptID, &out); e != nil {
				return e
			}
		case "result":
			p := payload.(*runs.ExecutionResult)
			if p.Outcome == "success" {
				if observation == nil {
					return runs.Fail("SOURCE_OBSERVATION_REQUIRED")
				}
				if e = c.RequireObservation(ctx, tx, *observation, a.Source.FactsRevision); e != nil {
					return e
				}
			}
			conflict, e := c.executionResult(ctx, tx, *a, m, *p, pre, sealed, permitted, ev.Planning, &out)
			if e != nil {
				return e
			}
			if conflict {
				attention = true
				return c.attention(ctx, tx, m.AttemptID)
			}
		default:
			return runs.Fail("COMMAND_UNSUPPORTED")
		}
		if _, e = tx.Exec(ctx, `UPDATE cosmoformer.attempts SET state=$2,revision=revision+1 WHERE id=$1`, m.AttemptID, out.AttemptState); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `UPDATE cosmoformer.operations SET status=$2,revision=revision+1 WHERE id=$1`, a.OperationID, out.OperationStatus); e != nil {
			return e
		}
		messageOrdinal := ordinal
		switch p := payload.(type) {
		case *runs.CommandRequest:
			messageOrdinal = p.Ordinal
		case *runs.CommandEvent:
			messageOrdinal = p.Ordinal
		case *runs.ControlAck:
			messageOrdinal = p.Ordinal
		}
		_, e = tx.Exec(ctx, `INSERT INTO cosmoformer.attempt_messages(org_id,attempt_id,sequence,message_id,digest,payload,command_ordinal,kind) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, w.OrgID, m.AttemptID, m.Seq, m.MessageID, nativeMessageDigest(route, m, payload), mustJSON(out), messageOrdinal, route)
		return e
	})
	if e == nil && attention {
		e = runs.Fail("MESSAGE_CONFLICT")
	}
	if e == nil && out.Permission != nil {
		// Time spent committing the permission cannot extend a serialized lease.
		e = c.workerTx(ctx, token, func(tx pgx.Tx, _ Worker) error { return c.permissionRemaining(ctx, tx, m.AttemptID, out.Permission) })
	}
	return &out, e
}
func (c *Coordinator) exactReservations(ctx context.Context, tx pgx.Tx, attempt string) error {
	var valid bool
	e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.attempts a JOIN cosmoformer.worker_reservations w ON w.attempt_id=a.id AND w.worker_id=a.worker_id AND w.generation=a.generation AND w.agent_instance=a.agent_instance JOIN cosmoformer.backend_reservations b ON b.attempt_id=a.id AND b.target_id=a.target_id AND b.stack_id=a.stack_id WHERE a.id=$1)`, attempt).Scan(&valid)
	if e != nil {
		return e
	}
	if !valid {
		return runs.Fail("RESERVATION_REQUIRED")
	}
	return nil
}
func (c *Coordinator) issuePermission(ctx context.Context, tx pgx.Tx, w Worker, a runs.Assignment, m runs.Message, p runs.CommandRequest, observation *Observation, out *runs.Transition) error {
	tick, e := c.executionBudget(ctx, tx, m.AttemptID, true)
	if e != nil {
		return e
	}
	var deadline, credential int64
	if e = tx.QueryRow(ctx, `SELECT deadline_tick,credential_deadline_tick FROM cosmoformer.attempts WHERE id=$1`, m.AttemptID).Scan(&deadline, &credential); e != nil {
		return e
	}
	remaining := min(int64(10000), (deadline-int64(tick))/int64(time.Millisecond), (credential-int64(tick))/int64(time.Millisecond))
	var observationID any
	if observation != nil {
		if e = c.RequireObservation(ctx, tx, *observation, a.Source.FactsRevision); e != nil {
			return e
		}
		remaining = min(remaining, (int64(observation.Started+30*time.Second)-int64(tick))/int64(time.Millisecond))
		var id string
		if e = tx.QueryRow(ctx, `SELECT id::text FROM cosmoformer.source_observations WHERE subject_id=$1 AND ticket=$2 AND resulting_facts_revision=$3`, observation.SubjectID, observation.Ticket, a.Source.FactsRevision).Scan(&id); e != nil {
			return e
		}
		observationID = id
	}
	remaining -= 100
	if remaining <= 0 {
		return runs.Fail("PERMISSION_EXPIRED")
	}
	out.Permission = &runs.Permission{ID: runs.ID(), Ordinal: p.Ordinal, Digest: p.Digest, Platform: c.Instance, RemainingMS: remaining}
	e = tx.QueryRow(ctx, `INSERT INTO cosmoformer.start_permissions(id,org_id,attempt_id,ordinal,command,command_digest,platform_instance,expires_tick,issued_at,display_expires_at,facts_revision,observation_id,status) VALUES($1,$2,$3,$4,$5,decode($6,'hex'),$7,$8,clock_timestamp(),clock_timestamp()+$9*interval '1 millisecond',$10,$11,'issued') RETURNING display_expires_at`, out.Permission.ID, w.OrgID, m.AttemptID, p.Ordinal, p.Command, p.Digest, c.Instance, int64(tick)+remaining*int64(time.Millisecond), remaining, a.Source.FactsRevision, observationID).Scan(&out.Permission.Expires)
	if e != nil {
		return e
	}
	_, e = tx.Exec(ctx, `UPDATE cosmoformer.attempts SET ever_permitted=true,command_ordinal=$2 WHERE id=$1`, m.AttemptID, p.Ordinal)
	if e != nil {
		return e
	}
	out.AttemptState = "permitted"
	out.OperationStatus = "executing"
	return nil
}

// ResolveExecutionSpec resolves metadata for an execution claim. It does
// not create work, claim ownership, admit a worker or contact an AWS service.
func ResolveExecutionSpec(ctx context.Context, tx pgx.Tx, p runs.PreflightSpec, b source.Backend) (runs.ExecutionSpec, error) {
	s := runs.ExecutionSpec{Version: 1, Phase: "execution", StackRevisionID: p.StackRevisionID, Source: p.Source, Engine: p.Engine, Workspace: p.Workspace, VarFiles: p.VarFiles, BackendFiles: p.BackendFiles, Pool: p.Pool, Backend: b, AWS: runs.ExecutionAWS{AWSRef: p.AWS}, Secrets: p.Secrets, SourceCredentials: p.SourceCredentials, Grants: p.Grants, Review: p.Review, RuleVersion: p.RuleVersion, ProfileID: p.ProfileID}
	e := tx.QueryRow(ctx, `SELECT expected_bootstrap_principal,session_seconds FROM cosmoformer.aws_connections WHERE id=$1 AND revision=$2 AND enabled`, p.AWS.ID, p.AWS.Revision).Scan(&s.AWS.Bootstrap, &s.AWS.SessionSeconds)
	if e != nil {
		return s, runs.Fail("AWS_EXECUTION_METADATA_REQUIRED")
	}
	e = tx.QueryRow(ctx, `SELECT p.approval_policy_revision FROM cosmoformer.projects p JOIN cosmoformer.stacks st ON st.project_id=p.id WHERE st.effective_revision_id=$1`, p.StackRevisionID).Scan(&s.Review.Revision)
	if e != nil {
		return s, e
	}
	return s, s.Validate()
}
