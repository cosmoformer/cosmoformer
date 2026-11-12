package httpapi

import (
	"context"

	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
)

type PullRequestsInput struct {
	WorkspaceListInput
	Head      string `query:"head" format:"uuid"`
	RunBefore string `query:"run_before" format:"uuid"`
}
type PRRun struct {
	Run       RunPublic `json:"run"`
	Attention bool      `json:"needs_attention"`
}
type PullRequestPublic struct {
	ID             string  `json:"id" format:"uuid"`
	RepositoryID   string  `json:"repository_id" format:"uuid"`
	RepositoryUUID string  `json:"repository_uuid"`
	RepositoryURL  string  `json:"repository_url"`
	Number         string  `json:"number"`
	Commit         string  `json:"commit"`
	State          string  `json:"state"`
	Current        bool    `json:"current"`
	Feedback       string  `json:"feedback_status"`
	Runs           []PRRun `json:"runs" nullable:"false"`
	RunsNext       string  `json:"runs_next,omitempty"`
	subject        string
	facts          int64
}
type PullRequestsOutput struct {
	Body struct {
		PullRequests []PullRequestPublic `json:"pull_requests" nullable:"false"`
		Next         string              `json:"next_cursor,omitempty"`
	}
}

// Grouping, cursors and the child page all use the current visible run set.
// A hidden run cannot supply a group ID, change a count, or anchor a cursor.
func (a *Server) registerPullRequests(api huma.API) {
	register(api, "GET", "/organizations/{org}/pull-requests", "pull-requests", func(ctx context.Context, in *PullRequestsInput) (*PullRequestsOutput, error) {
		out := &PullRequestsOutput{}
		out.Body.PullRequests = []PullRequestPublic{}
		e := a.tx(ctx, in.Org, func(tx pgx.Tx, actor store.Actor) error {
			grants, e := store.GrantsFor(ctx, tx, actor.ID)
			if e != nil {
				return e
			}
			const groups = `WITH visible AS (SELECT r.* FROM cosmoformer.runs r JOIN cosmoformer.stacks s ON s.id=r.stack_id JOIN cosmoformer.source_subjects ss ON ss.id=r.source_subject_id WHERE ss.kind='pr' AND ` + workspaceScope + `), heads AS (SELECT min(id::text) AS id,source_subject_id,source_facts_revision,source_commit,max(created_at) AS at FROM visible GROUP BY source_subject_id,source_facts_revision,source_commit) `
			rows, e := tx.Query(ctx, groups+`SELECT h.id,h.source_subject_id::text,h.source_facts_revision,h.source_commit,ss.repository_id::text,rp.external_id,rp.url,ss.external_key,coalesce(ss.facts->>'state',''),h.source_facts_revision=ss.facts_revision AND h.source_commit=ss.facts->>'commit',coalesce((SELECT status FROM cosmoformer.outbox WHERE kind='pr_feedback' AND subject_id=rp.id AND payload->>'pr_number'=ss.external_key ORDER BY created_at DESC,id DESC LIMIT 1),'not queued') FROM heads h JOIN cosmoformer.source_subjects ss ON ss.id=h.source_subject_id JOIN cosmoformer.repositories rp ON rp.id=ss.repository_id WHERE ($5='' OR (h.at,h.id)<(SELECT at,id FROM heads WHERE id=$5)) AND ($7='' OR h.id=$7) ORDER BY h.at DESC,h.id DESC LIMIT $6`, viewScopes(grants, false), in.Project, in.Environment, in.Stack, in.Before, in.Limit+1, in.Head)
			if e != nil {
				return e
			}
			for rows.Next() {
				var p PullRequestPublic
				p.Runs = []PRRun{}
				if e = rows.Scan(&p.ID, &p.subject, &p.facts, &p.Commit, &p.RepositoryID, &p.RepositoryUUID, &p.RepositoryURL, &p.Number, &p.State, &p.Current, &p.Feedback); e != nil {
					rows.Close()
					return e
				}
				out.Body.PullRequests = append(out.Body.PullRequests, p)
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return e
			}
			if len(out.Body.PullRequests) > in.Limit {
				out.Body.PullRequests = out.Body.PullRequests[:in.Limit]
				out.Body.Next = out.Body.PullRequests[in.Limit-1].ID
			}
			for i := range out.Body.PullRequests {
				p := &out.Body.PullRequests[i]
				rows, e = tx.Query(ctx, `SELECT `+runColumns+runTables+` WHERE `+workspaceScope+` AND r.source_subject_id=$5 AND r.source_facts_revision=$6 AND ($7='' OR (r.created_at,r.id)<(SELECT r.created_at,r.id`+runTables+` WHERE `+workspaceScope+` AND r.source_subject_id=$5 AND r.source_facts_revision=$6 AND r.id::text=$7)) ORDER BY r.created_at DESC,r.id DESC LIMIT 51`, viewScopes(grants, false), in.Project, in.Environment, in.Stack, p.subject, p.facts, in.RunBefore)
				if e != nil {
					return e
				}
				for rows.Next() {
					var r PRRun
					if e = scanRun(rows, &r.Run); e != nil {
						rows.Close()
						return e
					}
					p.Runs = append(p.Runs, r)
				}
				e = rows.Err()
				rows.Close()
				if e != nil {
					return e
				}
				if len(p.Runs) > 50 {
					p.Runs = p.Runs[:50]
					p.RunsNext = p.Runs[49].Run.ID
				}
				for j := range p.Runs {
					r := &p.Runs[j]
					if e = readRunPlan(ctx, tx, &r.Run); e != nil {
						return e
					}
					if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.recovery_cases WHERE stack_id=$1 AND status='open')`, r.Run.StackID).Scan(&r.Attention); e != nil {
						return e
					}
				}
			}
			return nil
		})
		return out, e
	})
}
