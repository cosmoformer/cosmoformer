package httpapi

import (
	"context"

	"github.com/cosmoformer/cosmoformer/internal/access"
	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
)

type DependencyPublic struct {
	ID          string `json:"id" format:"uuid"`
	Name        string `json:"name"`
	Project     string `json:"project_id" format:"uuid"`
	Environment string `json:"environment_id" format:"uuid"`
	Archived    bool   `json:"archived"`
}
type DependenciesOutput struct {
	Body struct {
		Stack        string             `json:"stack_id" format:"uuid"`
		Revision     string             `json:"stack_revision_id" format:"uuid"`
		Dependencies []DependencyPublic `json:"dependencies" nullable:"false"`
	}
}

// Git declarations own these metadata edges. Resolve only unambiguous accepted
// stack declaration IDs in this tenant, after the complete inventory is imported.
// Cycles are valid manual coordination metadata; no scheduling reads this table.
func syncDependencies(ctx context.Context, tx pgx.Tx, manifest catalog.Manifest, stacks []string) error {
	for i, s := range manifest.Stacks {
		if _, e := tx.Exec(ctx, `DELETE FROM cosmoformer.stack_dependencies WHERE stack_id=$1`, stacks[i]); e != nil {
			return e
		}
		for _, declaration := range s.DependsOn {
			rows, e := tx.Query(ctx, `SELECT id::text FROM cosmoformer.stacks WHERE declaration_id=$1 AND NOT archived ORDER BY id LIMIT 2`, declaration)
			if e != nil {
				return e
			}
			ids := []string{}
			for rows.Next() {
				var id string
				if e = rows.Scan(&id); e != nil {
					rows.Close()
					return e
				}
				ids = append(ids, id)
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return e
			}
			if len(ids) != 1 || ids[0] == stacks[i] {
				return store.ErrConflict
			}
			if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.stack_dependencies(org_id,stack_id,depends_on_id) SELECT org_id,id,$2 FROM cosmoformer.stacks WHERE id=$1`, stacks[i], ids[0]); e != nil {
				return e
			}
		}
	}
	return nil
}
func (a *Server) registerDependencies(api huma.API) {
	register(api, "GET", "/organizations/{org}/stacks/{id}/dependencies", "stack-dependencies", func(ctx context.Context, in *OrgIDPath) (*DependenciesOutput, error) {
		out := &DependenciesOutput{}
		out.Body.Dependencies = []DependencyPublic{}
		e := a.tx(ctx, in.Org, func(tx pgx.Tx, actor store.Actor) error {
			grants, e := store.GrantsFor(ctx, tx, actor.ID)
			if e != nil {
				return e
			}
			var project, env string
			e = tx.QueryRow(ctx, `SELECT id::text,effective_revision_id::text,project_id::text,environment_id::text FROM cosmoformer.stacks WHERE id=$1 AND effective_revision_id IS NOT NULL`, in.ID).Scan(&out.Body.Stack, &out.Body.Revision, &project, &env)
			if e == pgx.ErrNoRows {
				return store.ErrNotFound
			}
			if e != nil {
				return e
			}
			if !access.Allows(grants, project, env, access.View) && !access.Allows(grants, project, env, access.Manage) {
				return store.ErrNotFound
			}
			rows, e := tx.Query(ctx, `SELECT s.id::text,s.display_name,s.project_id::text,s.environment_id::text,s.archived FROM cosmoformer.stack_dependencies d JOIN cosmoformer.stacks s ON s.id=d.depends_on_id WHERE d.stack_id=$2 AND `+visibleStack+` ORDER BY s.display_name,s.id LIMIT 64`, viewScopes(grants, true), in.ID)
			if e != nil {
				return e
			}
			defer rows.Close()
			for rows.Next() {
				var d DependencyPublic
				if e = rows.Scan(&d.ID, &d.Name, &d.Project, &d.Environment, &d.Archived); e != nil {
					return e
				}
				out.Body.Dependencies = append(out.Body.Dependencies, d)
			}
			return rows.Err()
		})
		return out, e
	})
}
