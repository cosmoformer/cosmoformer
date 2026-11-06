package store

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/access"
	"github.com/jackc/pgx/v5"
)

type Actor struct {
	ID                string    `json:"id" format:"uuid"`
	Username          string    `json:"username"`
	DisplayName       string    `json:"display_name"`
	InstallationAdmin bool      `json:"installation_admin"`
	SessionID         string    `json:"session_id" format:"uuid"`
	CSRFVerifier      string    `json:"csrf_verifier"`
	ExpiresAt         time.Time `json:"expires_at"`
}
type Account struct {
	ID                 string `json:"id" format:"uuid"`
	Username           string `json:"username"`
	DisplayName        string `json:"display_name"`
	Enabled            bool   `json:"enabled"`
	InstallationAdmin  bool   `json:"installation_admin"`
	Pending            bool   `json:"pending"`
	Revision           int64  `json:"revision"`
	PasswordPHC        string `json:"password_phc,omitempty"`
	CredentialRevision int64  `json:"credential_revision,omitempty"`
}
type Organization struct {
	ID       string `json:"id" format:"uuid"`
	Name     string `json:"name"`
	OrgAdmin bool   `json:"org_admin"`
}
type Invitation struct {
	ID    string `json:"id" format:"uuid"`
	OrgID string `json:"org_id" format:"uuid"`
	Name  string `json:"name"`
}
type Directory struct {
	Organizations []Organization `json:"organizations"`
	Invitations   []Invitation   `json:"invitations"`
}
type Project struct {
	ID           string        `json:"id" format:"uuid"`
	Name         string        `json:"name"`
	Environments []Environment `json:"environments"`
	CanManage    bool          `json:"can_manage"`
}
type Environment struct {
	ID   string `json:"id" format:"uuid"`
	Name string `json:"name"`
}
type Member struct {
	ID          string `json:"id" format:"uuid"`
	Username    string `json:"username"`
	DisplayName string `json:"display_name"`
	OrgAdmin    bool   `json:"org_admin"`
	Revision    int64  `json:"revision"`
}
type Team struct {
	ID      string   `json:"id" format:"uuid"`
	Name    string   `json:"name"`
	Members []string `json:"members"`
}
type ScopedGrant struct {
	ID     string `json:"id" format:"uuid"`
	TeamID string `json:"team_id" format:"uuid"`
	access.Grant
}
type AccessState struct {
	Members []Member      `json:"members"`
	Teams   []Team        `json:"teams"`
	Grants  []ScopedGrant `json:"grants"`
}

func loadActor(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, hash []byte) (Actor, error) {
	a, e := JSON[Actor](ctx, q, `SELECT cosmoformer.session_identity($1)`, hash)
	if e == ErrNotFound {
		return a, ErrUnauthenticated
	}
	return a, e
}
func (s *Store) Session(ctx context.Context, hash []byte) (Actor, error) {
	return loadActor(ctx, s.Pool, hash)
}
func (a Actor) CSRFMatches(hash []byte) bool {
	b, e := hex.DecodeString(a.CSRFVerifier)
	return e == nil && subtle.ConstantTimeCompare(b, hash) == 1
}
func (s *Store) LoginAccount(ctx context.Context, name string) (Account, error) {
	return JSON[Account](ctx, s.Pool, `SELECT cosmoformer.login_identity($1)`, name)
}
func (s *Store) Rate(ctx context.Context, subject []byte, outcome string) (int, error) {
	var n int
	e := s.Pool.QueryRow(ctx, `SELECT cosmoformer.auth_rate($1,$2)`, subject, outcome).Scan(&n)
	return n, e
}
func (s *Store) CreateSession(ctx context.Context, id string, rev int64, hash, csrf []byte) (bool, error) {
	var ok bool
	e := s.Pool.QueryRow(ctx, `SELECT cosmoformer.create_session($1,$2,$3,$4)`, id, rev, hash, csrf).Scan(&ok)
	return ok, e
}
func (s *Store) ConsumeProof(ctx context.Context, hash []byte, kind, phc string) (bool, error) {
	var ok bool
	e := s.Pool.QueryRow(ctx, `SELECT cosmoformer.consume_proof($1,$2,$3)`, hash, kind, phc).Scan(&ok)
	return ok, e
}
func DirectoryFor(ctx context.Context, tx pgx.Tx, actor string) (Directory, error) {
	return JSON[Directory](ctx, tx, `SELECT cosmoformer.account_directory($1)`, actor)
}
func OrgAdmin(ctx context.Context, tx pgx.Tx, actor Actor, org string) (bool, error) {
	if actor.InstallationAdmin {
		return true, nil
	}
	var ok bool
	e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.memberships WHERE org_id=$1 AND account_id=$2 AND org_admin)`, org, actor.ID).Scan(&ok)
	return ok, e
}
func Membership(ctx context.Context, tx pgx.Tx, actor Actor, org string) (bool, error) {
	var ok bool
	e := tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.memberships WHERE org_id=$1 AND account_id=$2)`, org, actor.ID).Scan(&ok)
	return ok, e
}
func GrantsFor(ctx context.Context, tx pgx.Tx, actor string) ([]access.Grant, error) {
	return JSON[[]access.Grant](ctx, tx, `SELECT coalesce(jsonb_agg(jsonb_build_object('project_id',g.project_id,'environment_id',coalesce(g.environment_id::text,''),'role',g.role)),'[]') FROM cosmoformer.grants g JOIN cosmoformer.team_members t ON (t.org_id,t.team_id)=(g.org_id,g.team_id) JOIN cosmoformer.memberships m ON (m.org_id,m.account_id)=(t.org_id,t.account_id) WHERE t.account_id=$1 AND g.revoked_at IS NULL`, actor)
}
func CanManage(ctx context.Context, tx pgx.Tx, actor Actor, org, project, environment string) (bool, error) {
	ok, e := OrgAdmin(ctx, tx, actor, org)
	if e != nil || ok {
		return ok, e
	}
	g, e := GrantsFor(ctx, tx, actor.ID)
	return access.Allows(g, project, environment, access.Manage), e
}
func Projects(ctx context.Context, tx pgx.Tx, actor Actor, org string) ([]Project, error) {
	g, e := GrantsFor(ctx, tx, actor.ID)
	if e != nil {
		return nil, e
	}
	admin, e := OrgAdmin(ctx, tx, actor, org)
	if e != nil {
		return nil, e
	}
	rows, e := tx.Query(ctx, `SELECT id::text,name FROM cosmoformer.projects WHERE NOT archived ORDER BY name,id`)
	if e != nil {
		return nil, e
	}
	all := []Project{}
	for rows.Next() {
		var p Project
		if e = rows.Scan(&p.ID, &p.Name); e != nil {
			rows.Close()
			return nil, e
		}
		all = append(all, p)
	}
	rows.Close()
	if e = rows.Err(); e != nil {
		return nil, e
	}
	out := []Project{}
	for _, p := range all {
		p.CanManage = admin || access.Allows(g, p.ID, "", access.Manage)
		p.Environments = []Environment{}
		envs, e := JSON[[]Environment](ctx, tx, `SELECT coalesce(jsonb_agg(jsonb_build_object('id',id,'name',name) ORDER BY name),'[]') FROM cosmoformer.environments WHERE project_id=$1 AND NOT archived`, p.ID)
		if e != nil {
			return nil, e
		}
		visible := p.CanManage || access.Allows(g, p.ID, "", access.View)
		for _, env := range envs {
			if p.CanManage || access.Allows(g, p.ID, env.ID, access.View) || access.Allows(g, p.ID, env.ID, access.Manage) {
				p.Environments = append(p.Environments, env)
				visible = true
			}
		}
		if visible {
			out = append(out, p)
		}
	}
	return out, nil
}
func AccessFor(ctx context.Context, tx pgx.Tx) (AccessState, error) {
	// Account labels go through a scoped helper, never a global account SELECT.
	return JSON[AccessState](ctx, tx, `SELECT jsonb_build_object('members',cosmoformer.member_labels(),'teams',coalesce((SELECT jsonb_agg(jsonb_build_object('id',t.id,'name',t.name,'members',coalesce((SELECT jsonb_agg(m.account_id) FROM cosmoformer.team_members m WHERE m.team_id=t.id),'[]')) ORDER BY t.name) FROM cosmoformer.teams t),'[]'),'grants',coalesce((SELECT jsonb_agg(jsonb_build_object('id',id,'team_id',team_id,'project_id',project_id,'environment_id',coalesce(environment_id::text,''),'role',role)) FROM cosmoformer.grants WHERE revoked_at IS NULL),'[]'))`)
}
func DecodeJSON(b []byte, v any) error { return json.Unmarshal(b, v) }
