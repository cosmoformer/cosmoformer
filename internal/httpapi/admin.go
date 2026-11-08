package httpapi

import (
	"context"

	"github.com/cosmoformer/cosmoformer/internal/access"
	"github.com/cosmoformer/cosmoformer/internal/identity"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
)

type OrgPath struct {
	Org string `path:"org" format:"uuid" pattern:"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$" doc:"Lowercase hyphenated organization UUID."`
}
type OrgIDPath struct {
	OrgPath
	ID string `path:"id" format:"uuid" pattern:"^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$" doc:"Lowercase hyphenated resource UUID."`
}
type AccountPath struct {
	ID string `path:"id" format:"uuid"`
}
type NameInput struct {
	Body struct {
		Name string `json:"name" minLength:"1" maxLength:"100"`
	}
}
type OrgNameInput struct {
	OrgPath
	Body struct {
		Name string `json:"name" minLength:"1" maxLength:"100"`
	}
}
type AccountCreate struct {
	Body struct {
		Username    string `json:"username" minLength:"3" maxLength:"64"`
		DisplayName string `json:"display_name" minLength:"1" maxLength:"100"`
	}
}
type AccountUpdate struct {
	AccountPath
	Body struct {
		Revision          int64  `json:"revision" minimum:"1"`
		Enabled           bool   `json:"enabled"`
		InstallationAdmin bool   `json:"installation_admin"`
		DisplayName       string `json:"display_name" minLength:"1" maxLength:"100"`
	}
}
type AccountPublic struct {
	ID                string `json:"id" format:"uuid"`
	Username          string `json:"username"`
	DisplayName       string `json:"display_name"`
	Enabled           bool   `json:"enabled"`
	InstallationAdmin bool   `json:"installation_admin"`
	Pending           bool   `json:"pending"`
	Revision          int64  `json:"revision"`
}
type AccountsOutput struct {
	Body struct {
		Accounts []AccountPublic `json:"accounts"`
	}
}
type OrganizationsOutput struct{ Body store.Directory }
type CodeOutput struct {
	Body struct {
		Code             string `json:"code"`
		Purpose          string `json:"purpose"`
		ExpiresInSeconds int    `json:"expires_in_seconds"`
	}
}
type InvitationInput struct {
	OrgPath
	Body struct {
		Username string `json:"username" minLength:"3" maxLength:"64"`
		OrgAdmin bool   `json:"org_admin"`
	}
}
type TeamMemberInput struct {
	OrgIDPath
	Body struct {
		AccountID string `json:"account_id" format:"uuid"`
	}
}
type TeamMemberPath struct {
	OrgIDPath
	Account string `path:"account" format:"uuid"`
}
type TeamUpdate struct {
	OrgIDPath
	Body struct {
		Name     string `json:"name" minLength:"1" maxLength:"100"`
		Revision int64  `json:"revision" minimum:"1"`
	}
}
type GrantInput struct {
	OrgPath
	Body struct {
		TeamID        string `json:"team_id" format:"uuid"`
		ProjectID     string `json:"project_id" format:"uuid"`
		EnvironmentID string `json:"environment_id,omitempty"`
		Role          string `json:"role" enum:"viewer,planner,deployer,administrator,approver,raw_evidence_read"`
	}
}
type EnvironmentInput struct {
	OrgPath
	Project string `path:"project" format:"uuid"`
	Body    struct {
		Name string `json:"name" minLength:"1" maxLength:"100"`
	}
}
type ProjectsOutput struct {
	Body struct {
		Projects  []store.Project `json:"projects"`
		CanManage bool            `json:"can_manage"`
	}
}
type AccessOutput struct{ Body store.AccessState }

func orgAdmin(ctx context.Context, tx pgx.Tx, actor store.Actor, org string) error {
	ok, e := store.OrgAdmin(ctx, tx, actor, org)
	if e != nil {
		return e
	}
	if !ok {
		return store.ErrNotFound
	}
	return nil
}
func manager(ctx context.Context, tx pgx.Tx, actor store.Actor, org, project, environment string) error {
	ok, e := store.CanManage(ctx, tx, actor, org, project, environment)
	if e != nil {
		return e
	}
	if !ok {
		return store.ErrNotFound
	}
	return nil
}
func (a *Server) registerAdmin(api huma.API) {
	register(api, "GET", "/admin/accounts", "accounts", func(ctx context.Context, _ *Empty) (*AccountsOutput, error) {
		out := &AccountsOutput{}
		e := a.tx(ctx, "", func(tx pgx.Tx, actor store.Actor) error {
			if !actor.InstallationAdmin {
				return store.ErrForbidden
			}
			accounts, e := store.JSON[[]AccountPublic](ctx, tx, `SELECT cosmoformer.list_accounts($1)`, actor.ID)
			out.Body.Accounts = accounts
			return e
		})
		return out, e
	})
	register(api, "POST", "/admin/accounts", "create-account", func(ctx context.Context, in *AccountCreate) (*IDResult, error) {
		name, e := identity.Username(in.Body.Username)
		if e != nil {
			return nil, problem(ctx, 422, e.Error())
		}
		return mutate(a, ctx, "", false, installGate, func(tx pgx.Tx, actor store.Actor) (*IDResult, error) {
			if !actor.InstallationAdmin {
				return nil, store.ErrForbidden
			}
			var id string
			e := tx.QueryRow(ctx, `SELECT cosmoformer.create_account($1,$2,$3)::text`, actor.ID, name, in.Body.DisplayName).Scan(&id)
			return idResult(id), e
		})
	})
	register(api, "PATCH", "/admin/accounts/{id}", "update-account", func(ctx context.Context, in *AccountUpdate) (*OK, error) {
		return mutate(a, ctx, "", false, installGate, func(tx pgx.Tx, actor store.Actor) (*OK, error) {
			if !actor.InstallationAdmin {
				return nil, store.ErrForbidden
			}
			var accepted bool
			e := tx.QueryRow(ctx, `SELECT cosmoformer.update_account($1,$2,$3,$4,$5,$6)`, actor.ID, in.ID, in.Body.Revision, in.Body.Enabled, in.Body.InstallationAdmin, in.Body.DisplayName).Scan(&accepted)
			if e == nil && !accepted {
				e = store.ErrConflict
			}
			return ok(), e
		})
	})
	for _, purpose := range []string{"setup", "reset"} {
		register(api, "POST", "/admin/accounts/{id}/"+purpose+"-code", "issue-"+purpose+"-code", func(ctx context.Context, in *AccountPath) (*CodeOutput, error) {
			return mutate(a, ctx, "", true, installGate, func(tx pgx.Tx, actor store.Actor) (*CodeOutput, error) {
				if !actor.InstallationAdmin {
					return nil, store.ErrForbidden
				}
				out := &CodeOutput{}
				out.Body.Code = identity.Token()
				out.Body.Purpose = purpose
				out.Body.ExpiresInSeconds = 1800
				if purpose == "setup" {
					out.Body.ExpiresInSeconds = 86400
				}
				var accepted bool
				e := tx.QueryRow(ctx, `SELECT cosmoformer.issue_proof($1,$2,$3,$4)`, actor.ID, in.ID, purpose, identity.Digest(out.Body.Code)).Scan(&accepted)
				if e == nil && !accepted {
					e = store.ErrConflict
				}
				return out, e
			})
		})
	}
	register(api, "GET", "/organizations", "organizations", func(ctx context.Context, _ *Empty) (*OrganizationsOutput, error) {
		out := &OrganizationsOutput{}
		e := a.tx(ctx, "", func(tx pgx.Tx, actor store.Actor) error {
			dir, e := store.DirectoryFor(ctx, tx, actor.ID)
			out.Body = dir
			return e
		})
		return out, e
	})
	register(api, "POST", "/organizations", "create-organization", func(ctx context.Context, in *NameInput) (*IDResult, error) {
		return mutate(a, ctx, "", false, installGate, func(tx pgx.Tx, actor store.Actor) (*IDResult, error) {
			if !actor.InstallationAdmin {
				return nil, store.ErrForbidden
			}
			var id string
			if e := tx.QueryRow(ctx, `SELECT gen_random_uuid()::text`).Scan(&id); e != nil {
				return nil, e
			}
			if _, e := tx.Exec(ctx, `SELECT set_config('app.org_id',$1,true)`, id); e != nil {
				return nil, e
			}
			if _, e := tx.Exec(ctx, `INSERT INTO cosmoformer.organizations(id,name) VALUES($1,$2)`, id, in.Body.Name); e != nil {
				return nil, e
			}
			if _, e := tx.Exec(ctx, `INSERT INTO cosmoformer.memberships(org_id,account_id,org_admin) VALUES($1,$2,true)`, id, actor.ID); e != nil {
				return nil, e
			}
			return idResult(id), store.Audit(ctx, tx, id, actor.ID, "organization.create", id)
		})
	})
	register(api, "POST", "/organizations/{org}/invitations", "invite", func(ctx context.Context, in *InvitationInput) (*IDResult, error) {
		return mutate(a, ctx, in.Org, false, func(tx pgx.Tx, actor store.Actor) error { return orgAdmin(ctx, tx, actor, in.Org) }, func(tx pgx.Tx, actor store.Actor) (*IDResult, error) {
			if e := orgAdmin(ctx, tx, actor, in.Org); e != nil {
				return nil, e
			}
			name, e := identity.Username(in.Body.Username)
			if e != nil {
				return nil, store.ErrConflict
			}
			target, e := store.JSON[store.Account](ctx, tx, `SELECT cosmoformer.login_identity($1)`, name)
			if e != nil || !target.Enabled {
				return nil, store.ErrNotFound
			}
			var existing bool
			if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.memberships WHERE account_id=$1)`, target.ID).Scan(&existing); e != nil {
				return nil, e
			}
			if existing {
				return nil, store.ErrConflict
			}
			var id string
			e = tx.QueryRow(ctx, `INSERT INTO cosmoformer.invitations(org_id,account_id,org_admin) VALUES($1,$2,$3) RETURNING id::text`, in.Org, target.ID, in.Body.OrgAdmin).Scan(&id)
			if e != nil {
				return nil, e
			}
			return idResult(id), store.Audit(ctx, tx, in.Org, actor.ID, "invitation.create", id)
		})
	})
	register(api, "POST", "/organizations/{org}/invitations/{id}/accept", "accept-invitation", func(ctx context.Context, in *OrgIDPath) (*OK, error) {
		return mutate(a, ctx, in.Org, false, func(tx pgx.Tx, actor store.Actor) error {
			var own bool
			e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.invitations WHERE id=$1 AND account_id=$2)`, in.ID, actor.ID).Scan(&own)
			if e != nil {
				return e
			}
			if !own {
				return store.ErrNotFound
			}
			return nil
		}, func(tx pgx.Tx, actor store.Actor) (*OK, error) {
			var admin bool
			e := tx.QueryRow(ctx, `UPDATE cosmoformer.invitations SET accepted_at=clock_timestamp() WHERE id=$1 AND account_id=$2 AND accepted_at IS NULL RETURNING org_admin`, in.ID, actor.ID).Scan(&admin)
			if e != nil {
				return nil, e
			}
			if _, e = tx.Exec(ctx, `INSERT INTO cosmoformer.memberships(org_id,account_id,org_admin) VALUES($1,$2,$3)`, in.Org, actor.ID, admin); e != nil {
				return nil, e
			}
			return ok(), store.Audit(ctx, tx, in.Org, actor.ID, "invitation.accept", in.ID)
		})
	})
	register(api, "GET", "/organizations/{org}/access", "access", func(ctx context.Context, in *OrgPath) (*AccessOutput, error) {
		out := &AccessOutput{}
		e := a.tx(ctx, in.Org, func(tx pgx.Tx, actor store.Actor) error {
			if e := orgAdmin(ctx, tx, actor, in.Org); e != nil {
				return e
			}
			state, e := store.AccessFor(ctx, tx)
			out.Body = state
			return e
		})
		return out, e
	})
	register(api, "POST", "/organizations/{org}/teams", "create-team", func(ctx context.Context, in *OrgNameInput) (*IDResult, error) {
		return mutate(a, ctx, in.Org, false, func(tx pgx.Tx, actor store.Actor) error { return orgAdmin(ctx, tx, actor, in.Org) }, func(tx pgx.Tx, actor store.Actor) (*IDResult, error) {
			if e := orgAdmin(ctx, tx, actor, in.Org); e != nil {
				return nil, e
			}
			var id string
			e := tx.QueryRow(ctx, `INSERT INTO cosmoformer.teams(org_id,name) VALUES($1,$2) RETURNING id::text`, in.Org, in.Body.Name).Scan(&id)
			if e != nil {
				return nil, e
			}
			return idResult(id), store.Audit(ctx, tx, in.Org, actor.ID, "team.create", id)
		})
	})
	register(api, "PATCH", "/organizations/{org}/teams/{id}", "update-team", func(ctx context.Context, in *TeamUpdate) (*OK, error) {
		return mutate(a, ctx, in.Org, false, func(tx pgx.Tx, actor store.Actor) error { return orgAdmin(ctx, tx, actor, in.Org) }, func(tx pgx.Tx, actor store.Actor) (*OK, error) {
			if e := orgAdmin(ctx, tx, actor, in.Org); e != nil {
				return nil, e
			}
			r, e := tx.Exec(ctx, `UPDATE cosmoformer.teams SET name=$1,revision=revision+1 WHERE id=$2 AND revision=$3`, in.Body.Name, in.ID, in.Body.Revision)
			if e != nil {
				return nil, e
			}
			if r.RowsAffected() != 1 {
				return nil, store.ErrConflict
			}
			return ok(), store.Audit(ctx, tx, in.Org, actor.ID, "team.update", in.ID)
		})
	})
	register(api, "POST", "/organizations/{org}/teams/{id}/members", "add-team-member", func(ctx context.Context, in *TeamMemberInput) (*OK, error) {
		return mutate(a, ctx, in.Org, false, func(tx pgx.Tx, actor store.Actor) error { return orgAdmin(ctx, tx, actor, in.Org) }, func(tx pgx.Tx, actor store.Actor) (*OK, error) {
			if e := orgAdmin(ctx, tx, actor, in.Org); e != nil {
				return nil, e
			}
			_, e := tx.Exec(ctx, `INSERT INTO cosmoformer.team_members(org_id,team_id,account_id) VALUES($1,$2,$3)`, in.Org, in.ID, in.Body.AccountID)
			if e != nil {
				return nil, e
			}
			return ok(), store.Audit(ctx, tx, in.Org, actor.ID, "team.member.add", in.ID)
		})
	})
	register(api, "DELETE", "/organizations/{org}/teams/{id}/members/{account}", "remove-team-member", func(ctx context.Context, in *TeamMemberPath) (*OK, error) {
		return mutate(a, ctx, in.Org, false, func(tx pgx.Tx, actor store.Actor) error { return orgAdmin(ctx, tx, actor, in.Org) }, func(tx pgx.Tx, actor store.Actor) (*OK, error) {
			if e := orgAdmin(ctx, tx, actor, in.Org); e != nil {
				return nil, e
			}
			r, e := tx.Exec(ctx, `DELETE FROM cosmoformer.team_members WHERE team_id=$1 AND account_id=$2`, in.ID, in.Account)
			if e != nil {
				return nil, e
			}
			if r.RowsAffected() != 1 {
				return nil, store.ErrNotFound
			}
			return ok(), store.Audit(ctx, tx, in.Org, actor.ID, "team.member.remove", in.ID)
		})
	})
	register(api, "POST", "/organizations/{org}/grants", "create-grant", func(ctx context.Context, in *GrantInput) (*IDResult, error) {
		return mutate(a, ctx, in.Org, false, func(tx pgx.Tx, actor store.Actor) error {
			return manager(ctx, tx, actor, in.Org, in.Body.ProjectID, in.Body.EnvironmentID)
		}, func(tx pgx.Tx, actor store.Actor) (*IDResult, error) {
			if !access.ValidRole(in.Body.Role) {
				return nil, store.ErrConflict
			}
			if e := manager(ctx, tx, actor, in.Org, in.Body.ProjectID, in.Body.EnvironmentID); e != nil {
				return nil, e
			}
			var id string
			e := tx.QueryRow(ctx, `INSERT INTO cosmoformer.grants(org_id,team_id,project_id,environment_id,role) VALUES($1,$2,$3,nullif($4,'')::uuid,$5) RETURNING id::text`, in.Org, in.Body.TeamID, in.Body.ProjectID, in.Body.EnvironmentID, in.Body.Role).Scan(&id)
			if e != nil {
				return nil, e
			}
			return idResult(id), store.Audit(ctx, tx, in.Org, actor.ID, "grant.create", id)
		})
	})
	register(api, "DELETE", "/organizations/{org}/grants/{id}", "revoke-grant", func(ctx context.Context, in *OrgIDPath) (*OK, error) {
		return mutate(a, ctx, in.Org, false, func(tx pgx.Tx, actor store.Actor) error {
			var p, env string
			if e := tx.QueryRow(ctx, `SELECT project_id::text,coalesce(environment_id::text,'') FROM cosmoformer.grants WHERE id=$1`, in.ID).Scan(&p, &env); e != nil {
				return e
			}
			return manager(ctx, tx, actor, in.Org, p, env)
		}, func(tx pgx.Tx, actor store.Actor) (*OK, error) {
			var p, env string
			e := tx.QueryRow(ctx, `SELECT project_id::text,coalesce(environment_id::text,'') FROM cosmoformer.grants WHERE id=$1`, in.ID).Scan(&p, &env)
			if e != nil {
				return nil, e
			}
			if e = manager(ctx, tx, actor, in.Org, p, env); e != nil {
				return nil, e
			}
			if _, e = tx.Exec(ctx, `UPDATE cosmoformer.grants SET revoked_at=clock_timestamp(),revision=revision+1 WHERE id=$1 AND revoked_at IS NULL`, in.ID); e != nil {
				return nil, e
			}
			return ok(), store.Audit(ctx, tx, in.Org, actor.ID, "grant.revoke", in.ID)
		})
	})
	register(api, "GET", "/organizations/{org}/projects", "projects", func(ctx context.Context, in *OrgPath) (*ProjectsOutput, error) {
		out := &ProjectsOutput{}
		e := a.tx(ctx, in.Org, func(tx pgx.Tx, actor store.Actor) error {
			member, e := store.Membership(ctx, tx, actor, in.Org)
			if e != nil {
				return e
			}
			if !member && !actor.InstallationAdmin {
				return store.ErrNotFound
			}
			out.Body.CanManage, e = store.OrgAdmin(ctx, tx, actor, in.Org)
			if e != nil {
				return e
			}
			out.Body.Projects, e = store.Projects(ctx, tx, actor, in.Org)
			return e
		})
		return out, e
	})
	register(api, "POST", "/organizations/{org}/projects", "create-project", func(ctx context.Context, in *OrgNameInput) (*IDResult, error) {
		return mutate(a, ctx, in.Org, false, func(tx pgx.Tx, actor store.Actor) error { return orgAdmin(ctx, tx, actor, in.Org) }, func(tx pgx.Tx, actor store.Actor) (*IDResult, error) {
			if e := orgAdmin(ctx, tx, actor, in.Org); e != nil {
				return nil, e
			}
			var id string
			e := tx.QueryRow(ctx, `INSERT INTO cosmoformer.projects(org_id,name) VALUES($1,$2) RETURNING id::text`, in.Org, in.Body.Name).Scan(&id)
			if e != nil {
				return nil, e
			}
			return idResult(id), store.Audit(ctx, tx, in.Org, actor.ID, "project.create", id)
		})
	})
	register(api, "POST", "/organizations/{org}/projects/{project}/environments", "create-environment", func(ctx context.Context, in *EnvironmentInput) (*IDResult, error) {
		return mutate(a, ctx, in.Org, false, func(tx pgx.Tx, actor store.Actor) error { return manager(ctx, tx, actor, in.Org, in.Project, "") }, func(tx pgx.Tx, actor store.Actor) (*IDResult, error) {
			if e := manager(ctx, tx, actor, in.Org, in.Project, ""); e != nil {
				return nil, e
			}
			var id string
			e := tx.QueryRow(ctx, `INSERT INTO cosmoformer.environments(org_id,project_id,name) VALUES($1,$2,$3) RETURNING id::text`, in.Org, in.Project, in.Body.Name).Scan(&id)
			if e != nil {
				return nil, e
			}
			return idResult(id), store.Audit(ctx, tx, in.Org, actor.ID, "environment.create", id)
		})
	})
	register(api, "PATCH", "/organizations/{org}/members/{id}", "update-membership", func(ctx context.Context, in *MembershipUpdate) (*OK, error) {
		return mutate(a, ctx, in.Org, false, func(tx pgx.Tx, actor store.Actor) error { return orgAdmin(ctx, tx, actor, in.Org) }, func(tx pgx.Tx, actor store.Actor) (*OK, error) {
			if actor.ID == in.ID && !in.Body.OrgAdmin {
				return nil, store.ErrConflict
			}
			result, e := tx.Exec(ctx, `UPDATE cosmoformer.memberships SET org_admin=$1,revision=revision+1 WHERE account_id=$2 AND revision=$3`, in.Body.OrgAdmin, in.ID, in.Body.Revision)
			if e != nil {
				return nil, e
			}
			if result.RowsAffected() != 1 {
				return nil, store.ErrConflict
			}
			return ok(), store.Audit(ctx, tx, in.Org, actor.ID, "membership.update", in.ID)
		})
	})
	register(api, "DELETE", "/organizations/{org}/members/{id}", "remove-membership", func(ctx context.Context, in *OrgIDPath) (*OK, error) {
		return mutate(a, ctx, in.Org, false, func(tx pgx.Tx, actor store.Actor) error { return orgAdmin(ctx, tx, actor, in.Org) }, func(tx pgx.Tx, actor store.Actor) (*OK, error) {
			if actor.ID == in.ID {
				return nil, store.ErrConflict
			}
			if _, e := tx.Exec(ctx, `DELETE FROM cosmoformer.team_members WHERE account_id=$1`, in.ID); e != nil {
				return nil, e
			}
			result, e := tx.Exec(ctx, `DELETE FROM cosmoformer.memberships WHERE account_id=$1`, in.ID)
			if e != nil {
				return nil, e
			}
			if result.RowsAffected() != 1 {
				return nil, store.ErrNotFound
			}
			return ok(), store.Audit(ctx, tx, in.Org, actor.ID, "membership.remove", in.ID)
		})
	})

}

func installGate(_ pgx.Tx, actor store.Actor) error {
	if !actor.InstallationAdmin {
		return store.ErrForbidden
	}
	return nil
}

type MembershipUpdate struct {
	OrgIDPath
	Body struct {
		Revision int64 `json:"revision" minimum:"1"`
		OrgAdmin bool  `json:"org_admin"`
	}
}
