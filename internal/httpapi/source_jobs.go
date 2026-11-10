package httpapi

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/bitbucket"
	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/coordinator"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/jackc/pgx/v5"
)

type sourceJob struct {
	ID, Repo, Claim, Kind string
	Payload               []byte
	Attempts              int
}

// The queue has a closed action set: source reconciliation and PR feedback.
// No job can execute shell text, a native command, an approval or an apply.
func (a *Server) sourceJobs(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	sweep := true
	nextSweep := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		sweep = !time.Now().Before(nextSweep)
		if sweep {
			nextSweep = time.Now().Add(5 * time.Minute)
		}
		rows, e := a.Store.Pool.Query(ctx, `SELECT cosmoformer.source_reconciliation_tenants()::text`)
		if e != nil {
			continue
		}
		tenants := []string{}
		for rows.Next() {
			var org string
			if rows.Scan(&org) == nil {
				tenants = append(tenants, org)
			}
		}
		rows.Close()
		for _, org := range tenants {
			if ctx.Err() != nil {
				return
			}
			a.sourceJobTick(ctx, org, sweep)
		}
	}
}
func (a *Server) sourceJobTick(ctx context.Context, org string, sweep bool) {
	var job sourceJob
	e := a.serviceTx(ctx, org, func(tx pgx.Tx) error {
		if sweep {
			if _, e := tx.Exec(ctx, `INSERT INTO cosmoformer.outbox(org_id,kind,subject_id,payload,status,next_attempt_at,delivery_key) SELECT org_id,'source_reconcile',id,'{"source_kind":"branch","pr_number":0}', 'pending',clock_timestamp(),'sweep-'||(extract(epoch FROM clock_timestamp())::bigint/300)::text FROM cosmoformer.repositories WHERE enabled ON CONFLICT DO NOTHING`); e != nil {
				return e
			}
			// Known open PRs need their own reads: an unchanged tracked branch says
			// nothing about a missed PR update, retarget or closure event.
			if _, e := tx.Exec(ctx, `INSERT INTO cosmoformer.outbox(org_id,kind,subject_id,payload,status,next_attempt_at,delivery_key) SELECT s.org_id,'source_reconcile',s.repository_id,jsonb_build_object('source_kind','pr','pr_number',s.external_key::bigint),'pending',clock_timestamp(),'sweep-pr-'||s.external_key||'-'||(extract(epoch FROM clock_timestamp())::bigint/300)::text FROM cosmoformer.source_subjects s JOIN cosmoformer.repositories r ON r.org_id=s.org_id AND r.id=s.repository_id WHERE s.kind='pr' AND s.facts->>'state'='OPEN' AND r.enabled ON CONFLICT DO NOTHING`); e != nil {
				return e
			}
		}
		// These are retryable API/read jobs, never process ownership reservations.
		if _, e := tx.Exec(ctx, `UPDATE cosmoformer.outbox SET claim_id=NULL,claimed_at=NULL WHERE status='pending' AND claimed_at<clock_timestamp()-interval '60 seconds'`); e != nil {
			return e
		}
		e := tx.QueryRow(ctx, `SELECT id::text,subject_id::text,kind,payload,attempt_count FROM cosmoformer.outbox WHERE status='pending' AND kind IN('source_reconcile','pr_feedback') AND claim_id IS NULL AND next_attempt_at<=clock_timestamp() ORDER BY next_attempt_at,id LIMIT 1`).Scan(&job.ID, &job.Repo, &job.Kind, &job.Payload, &job.Attempts)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		if e != nil {
			return e
		}
		job.Claim = runs.ID()
		_, e = tx.Exec(ctx, `UPDATE cosmoformer.outbox SET claim_id=$2,claimed_at=clock_timestamp(),attempt_count=attempt_count+1,revision=revision+1 WHERE id=$1`, job.ID, job.Claim)
		return e
	})
	if e != nil || job.ID == "" {
		return
	}
	jobContext, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	var result error
	if job.Kind == "source_reconcile" {
		var payload struct {
			Kind string `json:"source_kind"`
			PR   int    `json:"pr_number"`
		}
		if runs.Decode(job.Payload, &payload) != nil {
			result = store.ErrConflict
		} else {
			result = a.reconcileSource(jobContext, org, job.Repo, payload.Kind, payload.PR)
		}
	} else {
		result = a.deliverFeedback(jobContext, org, job)
	}
	a.serviceTx(ctx, org, func(tx pgx.Tx) error {
		if result == nil {
			_, e := tx.Exec(ctx, `UPDATE cosmoformer.outbox SET status='delivered',delivered_at=clock_timestamp(),claim_id=NULL,claimed_at=NULL,revision=revision+1 WHERE id=$1 AND claim_id=$2`, job.ID, job.Claim)
			return e
		}
		delay := time.Duration(min(300, 1<<min(job.Attempts, 8))) * time.Second
		delay = delay/2 + time.Duration(rand.Int64N(int64(delay)))
		var remote *bitbucket.RemoteError
		permanent := false
		if errors.As(result, &remote) {
			delay = max(delay, remote.RetryAfter)
			permanent = remote.Status >= 400 && remote.Status < 500 && remote.Status != 429
		}
		status := "pending"
		if permanent || job.Attempts >= 11 || errors.Is(result, store.ErrForbidden) || errors.Is(result, catalog.ErrManifest) {
			status = "failed"
		}
		_, e := tx.Exec(ctx, `UPDATE cosmoformer.outbox SET status=$3,next_attempt_at=clock_timestamp()+$4::interval,claim_id=NULL,claimed_at=NULL,revision=revision+1 WHERE id=$1 AND claim_id=$2`, job.ID, job.Claim, status, fmt.Sprintf("%f seconds", delay.Seconds()))
		return e
	})
}
func (a *Server) reconcileSource(ctx context.Context, org, repo, kind string, pr int) error {
	if kind != "branch" && kind != "pr" || kind == "pr" && pr < 1 || kind == "branch" && pr != 0 {
		return store.ErrConflict
	}
	var r RepoFacts
	var observation coordinator.Observation
	if e := a.serviceTx(ctx, org, func(tx pgx.Tx) error {
		var e error
		r, e = repoFacts(ctx, tx, repo)
		if e != nil {
			return e
		}
		key := r.Branch
		if kind == "pr" {
			key = fmt.Sprint(pr)
		}
		observation, e = a.Coordinator.BeginObservation(ctx, tx, org, repo, kind, key)
		return e
	}); e != nil {
		return e
	}
	token, e := a.apiToken(ctx, org, r.IntegrationID)
	if e != nil {
		return e
	}
	facts, e := a.Bitbucket.Read(ctx, r.URL, r.ExternalID, r.Branch, pr, token)
	contextChanged := errors.Is(e, bitbucket.ErrContextChanged)
	if e != nil && !contextChanged {
		return e
	}
	facts.InventoryRevision = r.Revision
	if kind == "branch" && r.Inventory != facts.Commit {
		facts.InventoryRevision++
	}
	factsRevision, e := a.acceptSourceFacts(ctx, org, repo, r, observation, facts)
	if e != nil {
		return e
	}
	if contextChanged {
		return bitbucket.ErrContextChanged
	}
	// Changed/closed facts revoke old authority even if the manifest cannot be
	// read, parsed or imported. Inventory/work creation is a later transaction.
	if kind == "pr" && (facts.State != "OPEN" || facts.Destination != r.Branch) {
		return nil
	}
	raw, e := a.Bitbucket.Manifest(ctx, r.URL, facts.Commit, token)
	if e != nil {
		return e
	}
	manifest, e := catalog.Parse(raw)
	if e != nil {
		return e
	}
	return a.serviceTx(ctx, org, func(tx pgx.Tx) error {
		current, e := repoFacts(ctx, tx, repo)
		if e != nil {
			return e
		}
		if current.Revision != r.Revision {
			return store.ErrConflict
		}
		if e = a.Coordinator.RequireObservation(ctx, tx, observation, factsRevision); e != nil {
			return e
		}
		if kind == "branch" {
			if _, e = a.importInventory(ctx, tx, org, repo, r, observation, facts, manifest); e != nil {
				return e
			}
		}
		for _, proposed := range manifest.Stacks {
			var stack, revision string
			var tracked runs.Effective
			e = tx.QueryRow(ctx, `SELECT s.id::text,s.effective_revision_id::text,r.effective_config FROM cosmoformer.stacks s JOIN cosmoformer.stack_revisions r ON r.id=s.effective_revision_id WHERE s.repository_id=$1 AND s.declaration_id=$2 AND NOT s.archived`, repo, proposed.ID).Scan(&stack, &revision, newJSON(&tracked))
			if errors.Is(e, pgx.ErrNoRows) {
				continue
			}
			if e != nil {
				return e
			}
			old := tracked.Declaration
			if proposed.ProjectID != old.ProjectID || proposed.EnvironmentID != old.EnvironmentID || proposed.Worker.ID != old.Worker.ID || proposed.Root != old.Root || proposed.Workspace != old.Workspace || proposed.Engine != old.Engine || proposed.TrackedBranch != old.TrackedBranch {
				continue
			}
			proposed.Approvals.Minimum = max(proposed.Approvals.Minimum, old.Approvals.Minimum)
			proposed.AllowPRApply = proposed.AllowPRApply && old.AllowPRApply
			// Repeated sweeps/deliveries of unchanged facts do not create new runs.
			var exists bool
			if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.runs WHERE stack_id=$1 AND source_subject_id=$2 AND source_facts_revision=$3)`, stack, observation.SubjectID, factsRevision).Scan(&exists); e != nil {
				return e
			}
			if exists {
				continue
			}
			effective, e := coordinator.BuildSpec(ctx, tx, proposed, repo)
			if e != nil {
				return e
			}
			spec := coordinator.Spec(effective, revision, repo, r.URL, facts.Commit, observation.SubjectID, factsRevision)
			run, op := runs.ID(), runs.ID()
			purpose := "tracked"
			if kind == "pr" {
				purpose = "pr_preview"
			}
			if _, e = tx.Exec(ctx, `UPDATE cosmoformer.operations SET status='superseded',revision=revision+1 WHERE stack_id=$1 AND kind='plan' AND status='queued' AND run_id IN(SELECT id FROM cosmoformer.runs WHERE source_subject_id=$2)`, stack, observation.SubjectID); e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, `UPDATE cosmoformer.work_items SET status='retired',revision=revision+1 WHERE status='queued' AND operation_id IN(SELECT id FROM cosmoformer.operations WHERE stack_id=$1 AND status='superseded')`, stack); e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.runs(id,org_id,stack_id,source_subject_id,source_facts_revision,source_commit,purpose,automation_repository_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, run, org, stack, observation.SubjectID, factsRevision, facts.Commit, purpose, repo); e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.operations(id,org_id,run_id,stack_id,kind,status,priority,accepted_at,ordering_at,automation_repository_id) VALUES($1,$2,$3,$4,'plan','queued','medium',clock_timestamp(),clock_timestamp(),$5)`, op, org, run, stack, repo); e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.work_items(org_id,operation_id,phase,status,assignment_spec) VALUES($1,$2,'preflight','queued',$3)`, org, op, jsonBytes(spec)); e != nil {
				return e
			}
			if kind == "pr" {
				if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.outbox(org_id,kind,subject_id,payload,status,next_attempt_at,delivery_key) VALUES($1,'pr_feedback',$2,$3,'pending',clock_timestamp(),$4) ON CONFLICT DO NOTHING`, org, repo, jsonBytes(map[string]any{"pr_number": pr, "run_id": run, "commit": facts.Commit}), run); e != nil {
					return e
				}
			}
		}
		return nil
	})
}
func (a *Server) deliverFeedback(ctx context.Context, org string, job sourceJob) error {
	var p struct {
		PR     int    `json:"pr_number"`
		Run    string `json:"run_id"`
		Commit string `json:"commit"`
	}
	if runs.Decode(job.Payload, &p) != nil || p.PR < 1 {
		return store.ErrConflict
	}
	// Serialize remote writes per PR across platform processes without holding
	// the global coordination transaction over provider I/O. A delayed old job
	// checks current source only after acquiring the same lock as newer jobs.
	conn, e := a.Store.Pool.Acquire(ctx)
	if e != nil {
		return e
	}
	defer conn.Release()
	lockKey := org + "/" + job.Repo + "/" + fmt.Sprint(p.PR)
	var locked bool
	if e = conn.QueryRow(ctx, `SELECT pg_try_advisory_lock(hashtextextended($1,0))`, lockKey).Scan(&locked); e != nil {
		// A cancelled response can leave lock acquisition uncertain. Never put
		// that session back in the pool where the lock could outlive this job.
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		conn.Conn().Close(cleanup)
		cancel()
		return e
	}
	if !locked {
		return store.ErrConflict
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var released bool
		if err := conn.QueryRow(cleanup, `SELECT pg_advisory_unlock(hashtextextended($1,0))`, lockKey).Scan(&released); err != nil || !released {
			conn.Conn().Close(cleanup)
		}
	}()
	var r RepoFacts
	comment := ""
	current := false
	if e := a.serviceTx(ctx, org, func(tx pgx.Tx) error {
		var e error
		r, e = repoFacts(ctx, tx, job.Repo)
		if e != nil {
			return e
		}
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.runs r JOIN cosmoformer.source_subjects s ON s.id=r.source_subject_id WHERE r.id=$1 AND s.repository_id=$2 AND s.kind='pr' AND s.external_key=$3 AND r.source_commit=$4 AND s.facts_revision=r.source_facts_revision AND s.facts->>'commit'=r.source_commit AND s.facts->>'state'='OPEN')`, p.Run, job.Repo, fmt.Sprint(p.PR), p.Commit).Scan(&current); e != nil {
			return e
		}
		if !current {
			return nil
		}
		e = tx.QueryRow(ctx, `SELECT comment_id FROM cosmoformer.feedback_comments WHERE repository_id=$1 AND pr_number=$2`, job.Repo, p.PR).Scan(&comment)
		if errors.Is(e, pgx.ErrNoRows) {
			return nil
		}
		return e
	}); e != nil {
		return e
	}
	if !current {
		return nil
	}
	token, e := a.apiToken(ctx, org, r.IntegrationID)
	if e != nil {
		return e
	}
	marker := "<!-- cosmoformer:" + job.Repo + ":" + fmt.Sprint(p.PR) + " -->"
	if comment == "" {
		comment, e = a.Bitbucket.FindComment(ctx, r.URL, p.PR, token, marker)
		if e != nil {
			return e
		}
	}
	text := marker + "\nCosmoformer source preflight (no native plan or apply). Commit `" + p.Commit + "`.\n" + a.Origin + "/organizations?org=" + org + "&page=settings&run=" + p.Run
	id, e := a.Bitbucket.Comment(ctx, r.URL, p.PR, comment, token, text)
	if e != nil {
		return e
	}
	return a.serviceTx(ctx, org, func(tx pgx.Tx) error {
		_, e := tx.Exec(ctx, `INSERT INTO cosmoformer.feedback_comments(org_id,repository_id,pr_number,comment_id) VALUES($1,$2,$3,$4) ON CONFLICT(org_id,repository_id,pr_number) DO UPDATE SET comment_id=excluded.comment_id`, org, job.Repo, p.PR, id)
		return e
	})
}
