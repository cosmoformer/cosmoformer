package httpapi

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"path"
	"strings"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/bitbucket"
	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/coordinator"
	"github.com/cosmoformer/cosmoformer/internal/plan"
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
	return a.reconcileSourceSelection(ctx, org, repo, kind, pr, false)
}
func (a *Server) reconcileSourceSelection(ctx context.Context, org, repo, kind string, pr int, merged bool) error {
	if kind != "branch" && kind != "pr" || kind == "pr" && pr < 1 || kind == "branch" && pr != 0 {
		return store.ErrConflict
	}
	var r RepoFacts
	var observation coordinator.Observation
	var selection pathSelection
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
		if e != nil {
			return e
		}
		return tx.QueryRow(ctx, `SELECT path_selection FROM cosmoformer.source_subjects WHERE id=$1`, observation.SubjectID).Scan(newJSON(&selection))
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
		if facts.State == "MERGED" {
			return a.reconcileSourceSelection(ctx, org, repo, "branch", 0, true)
		}
		return nil
	}
	raw, e := a.Bitbucket.Manifest(ctx, r.URL, r.ExternalID, facts.Commit, token)
	if e != nil {
		return e
	}
	manifest, e := catalog.Parse(raw)
	if e != nil {
		return e
	}
	if selection.Revision != factsRevision {
		selection = pathSelection{Revision: factsRevision}
		// Diff ranges are exact verified commits. Provider pagination/truncation or
		// an unavailable diff falls back conservatively, after authorization below.
		selection.Paths, selection.Complete = a.Bitbucket.ChangedPaths(ctx, r.URL, r.ExternalID, r.Inventory, facts.Commit, token)
	}
	return a.serviceTx(ctx, org, func(tx pgx.Tx) error {
		current, e := repoFacts(ctx, tx, repo)
		if e != nil {
			return e
		}
		if current.Revision != r.Revision || current.IntegrationRevision != r.IntegrationRevision {
			return store.ErrConflict
		}
		if e = a.Coordinator.RequireObservation(ctx, tx, observation, factsRevision); e != nil {
			return e
		}
		if _, e = tx.Exec(ctx, `UPDATE cosmoformer.source_subjects SET path_selection=$2 WHERE id=$1`, observation.SubjectID, jsonBytes(selection)); e != nil {
			return e
		}
		if kind == "branch" && r.Inventory != facts.Commit {
			if _, e = a.importInventory(ctx, tx, org, repo, r, observation, facts, manifest); e != nil {
				return e
			}
		}
		for _, proposed := range manifest.Stacks {
			var stack, revision string
			var tracked runs.Effective
			var compatible bool
			e = tx.QueryRow(ctx, `SELECT s.id::text,s.effective_revision_id::text,r.effective_config,s.assessment='compatible' FROM cosmoformer.stacks s JOIN cosmoformer.stack_revisions r ON r.id=s.effective_revision_id WHERE s.repository_id=$1 AND s.declaration_id=$2 AND NOT s.archived`, repo, proposed.ID).Scan(&stack, &revision, newJSON(&tracked), &compatible)
			if errors.Is(e, pgx.ErrNoRows) {
				continue
			}
			if e != nil {
				return e
			}
			old := tracked.Declaration
			if proposed.ProjectID != old.ProjectID || proposed.EnvironmentID != old.EnvironmentID || proposed.Worker.ID != old.Worker.ID || proposed.Root != old.Root || proposed.Workspace != old.Workspace || proposed.BackendOwnerAccountID != old.BackendOwnerAccountID || proposed.Engine != old.Engine || proposed.TrackedBranch != old.TrackedBranch {
				continue
			}
			proposed.Approvals.Minimum = max(proposed.Approvals.Minimum, old.Approvals.Minimum)
			proposed.AllowPRApply = proposed.AllowPRApply && old.AllowPRApply
			// Repeated sweeps/deliveries of unchanged facts do not create new runs.
			var exists bool
			if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.runs r JOIN cosmoformer.operations o ON o.run_id=r.id WHERE r.stack_id=$1 AND r.source_subject_id=$2 AND r.source_facts_revision=$3 AND o.native_planning)`, stack, observation.SubjectID, factsRevision).Scan(&exists); e != nil {
				return e
			}
			if exists || !compatible {
				continue
			}
			effective, e := coordinator.BuildSpec(ctx, tx, proposed, repo)
			if e != nil {
				return e
			}
			if a.Evidence == nil || !plan.SupportedRuntime(effective.Runtime.Name, effective.Runtime.Version) {
				continue
			}
			var eligible bool
			if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.aws_connections WHERE id=$1 AND revision=$2 AND expected_bootstrap_principal IS NOT NULL AND session_seconds=3600 AND enabled) AND NOT EXISTS(SELECT FROM cosmoformer.recovery_cases WHERE stack_id=$3 AND status='open') AND EXISTS(SELECT FROM cosmoformer.worker_pools WHERE id=$4 AND enabled)`, effective.AWS.ID, effective.AWS.Revision, stack, proposed.Worker.ID).Scan(&eligible); e != nil {
				return e
			}
			if !eligible {
				continue
			}
			selected, err := selectedPaths(ctx, tx, stack, r.Inventory, proposed, selection)
			if err != nil {
				return err
			}
			if !selected && !merged {
				continue
			}
			spec := coordinator.Spec(effective, revision, repo, r.URL, facts.Commit, observation.SubjectID, factsRevision)
			run, op := runs.ID(), runs.ID()
			purpose := "tracked"
			if kind == "pr" {
				purpose = "pr_preview"
			}
			if _, e = tx.Exec(ctx, `UPDATE cosmoformer.operations SET status='superseded',revision=revision+1 WHERE stack_id=$1 AND kind='plan' AND native_planning AND automation_repository_id IS NOT NULL AND status='queued' AND NOT EXISTS(SELECT FROM cosmoformer.attempts WHERE operation_id=cosmoformer.operations.id AND closed_at IS NULL) AND run_id IN(SELECT id FROM cosmoformer.runs WHERE source_subject_id=$2)`, stack, observation.SubjectID); e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, `UPDATE cosmoformer.work_items SET status='retired',revision=revision+1 WHERE status='queued' AND operation_id IN(SELECT id FROM cosmoformer.operations WHERE stack_id=$1 AND status='superseded')`, stack); e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.runs(id,org_id,stack_id,source_subject_id,source_facts_revision,source_commit,purpose,automation_repository_id) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, run, org, stack, observation.SubjectID, factsRevision, facts.Commit, purpose, repo); e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.operations(id,org_id,run_id,stack_id,kind,status,priority,accepted_at,ordering_at,automation_repository_id,native_planning) VALUES($1,$2,$3,$4,'plan','queued','medium',clock_timestamp(),clock_timestamp(),$5,true)`, op, org, run, stack, repo); e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.work_items(org_id,operation_id,phase,status,assignment_spec) VALUES($1,$2,'preflight','queued',$3)`, org, op, jsonBytes(spec)); e != nil {
				return e
			}
			if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.audit_events(org_id,action,subject_id,facts) VALUES($1,'plan.automated',$2,$3)`, org, run, jsonBytes(map[string]any{"repository_id": repo, "source_subject_id": observation.SubjectID, "source_facts_revision": factsRevision, "operation_id": op})); e != nil {
				return e
			}
		}
		return nil
	})
}
func (a *Server) deliverFeedback(ctx context.Context, org string, job sourceJob) error {
	var p struct {
		PR            int    `json:"pr_number"`
		Run           string `json:"run_id,omitempty"`
		Commit        string `json:"commit,omitempty"`
		FactsRevision int64  `json:"facts_revision,omitempty"`
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
	var recoveryMarker string
	creationPending := false
	var body string
	current := false
	if e := a.serviceTx(ctx, org, func(tx pgx.Tx) error {
		var e error
		r, e = repoFacts(ctx, tx, job.Repo)
		if e != nil {
			return e
		}
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.source_subjects s WHERE s.repository_id=$1 AND s.kind='pr' AND s.external_key=$2 AND ($3::bigint=0 OR s.facts_revision=$3) AND ($4='' OR EXISTS(SELECT FROM cosmoformer.runs r WHERE r.id::text=$4 AND r.source_subject_id=s.id AND r.source_commit=$5 AND r.source_facts_revision=s.facts_revision)))`, job.Repo, fmt.Sprint(p.PR), p.FactsRevision, p.Run, p.Commit).Scan(&current); e != nil {
			return e
		}
		if !current {
			return nil
		}
		body, e = a.feedbackBody(ctx, tx, org, job.Repo, p.PR)
		if e != nil {
			return e
		}
		// A random, durable marker exists before even the first recovery lookup.
		// Predictable legacy text cannot establish ownership of an unknown ID.
		if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.feedback_comments(org_id,repository_id,pr_number,comment_id) VALUES($1,$2,$3,'') ON CONFLICT DO NOTHING`, org, job.Repo, p.PR); e != nil {
			return e
		}
		return tx.QueryRow(ctx, `SELECT comment_id,creation_pending,recovery_marker::text FROM cosmoformer.feedback_comments WHERE repository_id=$1 AND pr_number=$2`, job.Repo, p.PR).Scan(&comment, &creationPending, &recoveryMarker)
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
	marker := "<!-- cosmoformer:" + job.Repo + ":" + fmt.Sprint(p.PR) + ":" + recoveryMarker + " -->"
	if comment == "" {
		comment, e = a.Bitbucket.FindComment(ctx, r.URL, r.ExternalID, p.PR, token, marker)
		if e != nil {
			return e
		}
	}
	if comment == "" && creationPending {
		return bitbucket.ErrRemote
	}
	// Persist creation intent or a recovered marker identity before remote writes.
	// Even a pre-send transport failure remains ambiguous. Only marker recovery
	// or positive provider rejection can permit further creation decisions.
	if e = a.serviceTx(ctx, org, func(tx pgx.Tx) error {
		currentRepo, err := repoFacts(ctx, tx, job.Repo)
		if err != nil {
			return err
		}
		if currentRepo.IntegrationRevision != r.IntegrationRevision || currentRepo.Revision != r.Revision {
			return store.ErrConflict
		}
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.source_subjects s WHERE s.repository_id=$1 AND s.kind='pr' AND s.external_key=$2 AND ($3::bigint=0 OR s.facts_revision=$3) AND ($4='' OR EXISTS(SELECT FROM cosmoformer.runs r WHERE r.id::text=$4 AND r.source_subject_id=s.id AND r.source_commit=$5 AND r.source_facts_revision=s.facts_revision)))`, job.Repo, fmt.Sprint(p.PR), p.FactsRevision, p.Run, p.Commit).Scan(&current); err != nil {
			return err
		}
		if !current {
			return nil
		}
		body, err = a.feedbackBody(ctx, tx, org, job.Repo, p.PR)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO cosmoformer.feedback_comments(org_id,repository_id,pr_number,comment_id,creation_pending) VALUES($1,$2,$3,$4,$5) ON CONFLICT(org_id,repository_id,pr_number) DO UPDATE SET comment_id=excluded.comment_id,creation_pending=excluded.creation_pending`, org, job.Repo, p.PR, comment, comment == "")
		return err
	}); e != nil {
		return e
	}
	if !current {
		return nil
	}
	text := marker + "\n" + body
	id, e := a.Bitbucket.Comment(ctx, r.URL, r.ExternalID, p.PR, comment, token, text)
	if e != nil {
		var remote *bitbucket.RemoteError
		if comment == "" && errors.As(e, &remote) && remote.Status >= 400 && remote.Status < 500 && remote.Status != 408 {
			// A definite rejection is different from an ambiguous response or timeout.
			a.serviceTx(ctx, org, func(tx pgx.Tx) error {
				_, err := tx.Exec(ctx, `UPDATE cosmoformer.feedback_comments SET creation_pending=false WHERE repository_id=$1 AND pr_number=$2 AND comment_id=''`, job.Repo, p.PR)
				return err
			})
		}
		return e
	}
	return a.serviceTx(ctx, org, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE cosmoformer.feedback_comments SET comment_id=$3,creation_pending=false WHERE repository_id=$1 AND pr_number=$2`, job.Repo, p.PR, id)
		return err
	})
}

type pathSelection struct {
	Revision int64    `json:"facts_revision"`
	Paths    []string `json:"paths"`
	Complete bool     `json:"complete"`
}

func selectedPaths(ctx context.Context, tx pgx.Tx, stack, inventory string, s catalog.Stack, selection pathSelection) (bool, error) {
	if !selection.Complete {
		return true, nil
	}
	prefixes := append([]string{s.Root}, s.ChangePaths...)
	prefixes = append(prefixes, s.VarFiles...)
	prefixes = append(prefixes, s.BackendFiles...)
	for _, changed := range selection.Paths {
		if changed == "cosmoformer.yaml" {
			return true, nil
		}
	}
	// A verified tracked closure includes nested local modules. Without that
	// closure an outside-root change cannot safely be excluded.
	var raw []byte
	e := tx.QueryRow(ctx, `SELECT w.resolved_preflight FROM cosmoformer.work_items w JOIN cosmoformer.operations o ON o.id=w.operation_id JOIN cosmoformer.runs r ON r.id=o.run_id WHERE o.stack_id=$1 AND r.source_commit=$2 AND w.resolved_preflight IS NOT NULL ORDER BY w.created_at DESC LIMIT 1`, stack, inventory).Scan(&raw)
	if e != nil && !errors.Is(e, pgx.ErrNoRows) {
		return false, e
	}
	for _, changed := range selection.Paths {
		for _, prefix := range prefixes {
			if pathMatches(changed, prefix) {
				return true, nil
			}
		}
	}
	if len(selection.Paths) == 0 {
		return false, nil
	}
	if errors.Is(e, pgx.ErrNoRows) {
		return true, nil
	}
	var prep runs.Prepare
	if runs.Decode(raw, &prep) != nil {
		return true, nil
	}
	for _, module := range prep.Modules {
		if strings.HasPrefix(module.Source, "./") || strings.HasPrefix(module.Source, "../") {
			// Remote module trees use a digest prefix. Conservatively including their
			// paths is safe; only exact root-repository local paths can exclude work.
			prefix := path.Join(path.Dir(module.Path), module.Source)
			if !catalog.RepoPath(prefix) {
				return true, nil
			}
			for _, changed := range selection.Paths {
				if pathMatches(changed, prefix) {
					return true, nil
				}
			}
		}
	}
	return false, nil
}
func pathMatches(changed, prefix string) bool {
	prefix = strings.TrimSuffix(prefix, "/")
	return prefix == "." || changed == prefix || strings.HasPrefix(changed, prefix+"/")
}

// Feedback publishes bounded status vocabulary and opaque authenticated links,
// never evidence, names supplied by Git, input values or provider error bodies.
func (a *Server) feedbackBody(ctx context.Context, tx pgx.Tx, org, repo string, pr int) (string, error) {
	var commit, state, subject, project string
	var facts int64
	if e := tx.QueryRow(ctx, `SELECT s.id::text,s.facts_revision,coalesce(s.facts->>'commit',''),coalesce(s.facts->>'state',''),r.project_id::text FROM cosmoformer.source_subjects s JOIN cosmoformer.repositories r ON r.id=s.repository_id WHERE s.repository_id=$1 AND s.kind='pr' AND s.external_key=$2`, repo, fmt.Sprint(pr)).Scan(&subject, &facts, &commit, &state, &project); e != nil {
		return "", e
	}
	base := a.Origin + "/o/" + org + "/p/" + project
	body := "Cosmoformer native workflow · PR #" + fmt.Sprint(pr) + " · " + state + "\nSource `" + commit + "`. Approval and Apply require separate explicit actions in Cosmoformer.\n[Pull requests](" + base + "?view=pull-requests)\n"
	rows, e := tx.Query(ctx, `SELECT r.id::text,s.environment_id::text,o.status,coalesce(o.terminal_reason,''),coalesce(CASE WHEN p.status='ready' AND p.expires_at<=clock_timestamp() THEN 'expired' ELSE p.status END,''),coalesce(ap.status,'not requested'),o.native_planning,EXISTS(SELECT FROM cosmoformer.recovery_cases rc WHERE rc.stack_id=s.id AND rc.status='open') FROM cosmoformer.runs r JOIN cosmoformer.stacks s ON s.id=r.stack_id JOIN cosmoformer.operations o ON o.run_id=r.id AND o.kind='plan' LEFT JOIN cosmoformer.plans p ON p.operation_id=o.id LEFT JOIN LATERAL(SELECT status FROM cosmoformer.operations WHERE run_id=r.id AND kind='apply' ORDER BY accepted_at DESC,id DESC LIMIT 1) ap ON true WHERE r.id IN(SELECT DISTINCT ON(stack_id) id FROM cosmoformer.runs WHERE source_subject_id=$1 AND (source_facts_revision=$2 OR ($3<>'OPEN' AND source_commit=$4)) ORDER BY stack_id,source_facts_revision DESC,created_at DESC,id DESC) ORDER BY r.created_at DESC,r.id DESC LIMIT 11`, subject, facts, state, commit)
	if e != nil {
		return "", e
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var run, env, planning, reason, review, deployment string
		var native, attention bool
		if e = rows.Scan(&run, &env, &planning, &reason, &review, &deployment, &native, &attention); e != nil {
			return "", e
		}
		count++
		if count > 10 {
			body += "Additional runs are available in the protected project view.\n"
			break
		}
		label := "Planning"
		if !native {
			label = "Source check"
		}
		if reason == "NO_CHANGES" {
			planning = "No changes"
		}
		if review == "ready" {
			review = "ready for review (current eligibility checked in Cosmoformer)"
		}
		if review != "" {
			planning += "; " + review
		}
		if attention {
			deployment += "; recovery needs attention"
		}
		link := base + "/runs/" + run + "?env=" + env
		body += "- " + label + ": " + planning + " · Deployment: " + deployment + " · [Run](" + link + ") · [Changes](" + link + "&view=changes)\n"
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return "", e
	}
	if state == "MERGED" {
		var tracked, env, trackedCommit, status, reason string
		err := tx.QueryRow(ctx, `SELECT r.id::text,s.environment_id::text,r.source_commit,o.status,coalesce(o.terminal_reason,'') FROM cosmoformer.runs r JOIN cosmoformer.source_subjects ss ON ss.id=r.source_subject_id JOIN cosmoformer.stacks s ON s.id=r.stack_id JOIN cosmoformer.operations o ON o.run_id=r.id AND o.kind='plan' AND o.native_planning WHERE ss.repository_id=$1 AND ss.kind='branch' AND r.source_facts_revision=ss.facts_revision ORDER BY r.created_at DESC,r.id DESC LIMIT 1`, repo).Scan(&tracked, &env, &trackedCommit, &status, &reason)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", err
		}
		if err == nil {
			if reason == "NO_CHANGES" {
				status = "No changes"
			}
			body += "Fresh tracked plan: " + status + " · Source `" + trackedCommit + "` · [Run](" + base + "/runs/" + tracked + "?env=" + env + "). PR approvals never transfer.\n"
		}
	}
	if count == 0 {
		body += "No eligible native runs for this source observation. Check protected project details and source delivery.\n"
	}
	return body, rows.Err()
}
