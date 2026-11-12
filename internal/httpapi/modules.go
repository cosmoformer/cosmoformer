package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/cosmoformer/cosmoformer/internal/access"
	"github.com/cosmoformer/cosmoformer/internal/catalog"
	"github.com/cosmoformer/cosmoformer/internal/source"
	"github.com/cosmoformer/cosmoformer/internal/store"
	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"
)

type ModuleFields struct {
	Repository   string   `json:"repository_id" format:"uuid"`
	Commit       string   `json:"commit" pattern:"^[0-9a-f]{40}$"`
	Directory    string   `json:"subdirectory" minLength:"1" maxLength:"4096"`
	Name         string   `json:"name" minLength:"1" maxLength:"100"`
	Instructions string   `json:"instructions" minLength:"1" maxLength:"16000"`
	ExamplePaths []string `json:"example_paths" nullable:"false" maxItems:"16" uniqueItems:"true"`
	Archived     bool     `json:"archived"`
}
type ModuleCreate struct {
	OrgPath
	Body struct {
		Project string `json:"project_id,omitempty" format:"uuid"`
		ModuleFields
	}
}
type ModuleUpdate struct {
	OrgIDPath
	Body struct {
		Revision int64 `json:"expected_revision" minimum:"1" maximum:"9007199254740991"`
		ModuleFields
	}
}
type ModuleExample struct {
	Path string `json:"path"`
	URL  string `json:"url"`
}
type ModulePublic struct {
	ModuleFields
	ID            string          `json:"id" format:"uuid"`
	Project       string          `json:"project_id,omitempty" format:"uuid"`
	Revision      int64           `json:"revision" minimum:"1" maximum:"9007199254740991"`
	RepositoryURL string          `json:"repository_url"`
	SourceURL     string          `json:"source_url"`
	Source        string          `json:"source"`
	Snippet       string          `json:"snippet"`
	Examples      []ModuleExample `json:"examples" nullable:"false"`
	Available     bool            `json:"available"`
	CanManage     bool            `json:"can_manage"`
}
type ModuleOutput struct{ Body ModulePublic }
type ModuleListInput struct {
	OrgPath
	Project  string `query:"project_id" format:"uuid"`
	Query    string `query:"query" maxLength:"100"`
	Before   string `query:"before" format:"uuid"`
	Limit    int    `query:"limit" minimum:"1" maximum:"50" default:"20"`
	Archived bool   `query:"include_archived"`
}
type ModuleRepository struct {
	ID  string `json:"id" format:"uuid"`
	URL string `json:"url"`
}
type ModulesOutput struct {
	Body struct {
		Modules           []ModulePublic     `json:"modules" nullable:"false"`
		Repositories      []ModuleRepository `json:"repositories" nullable:"false"`
		CanRegister       bool               `json:"can_register"`
		CanRegisterShared bool               `json:"can_register_shared"`
		Next              string             `json:"next_cursor,omitempty"`
	}
}
type ModuleReadInput struct {
	OrgIDPath
	Revision int64 `query:"expected_revision" minimum:"1" maximum:"9007199254740991"`
}

// Catalog metadata is configuration: scoped administrators may manage it without
// acquiring execution/evidence rights. An environment Viewer can discover its
// project's catalog, but no catalog entry grants repository credentials.
func moduleScopes(ctx context.Context, tx pgx.Tx, actor store.Actor, org string) ([]string, []string, bool, error) {
	g, e := store.GrantsFor(ctx, tx, actor.ID)
	if e != nil {
		return nil, nil, false, e
	}
	admin, e := store.OrgAdmin(ctx, tx, actor, org)
	if e != nil {
		return nil, nil, false, e
	}
	visible, managed := []string{}, []string{}
	for _, v := range g {
		if access.Allows([]access.Grant{v}, v.ProjectID, v.EnvironmentID, access.View) || access.Allows([]access.Grant{v}, v.ProjectID, "", access.Manage) {
			visible = append(visible, v.ProjectID)
		}
		if access.Allows([]access.Grant{v}, v.ProjectID, "", access.Manage) {
			managed = append(managed, v.ProjectID)
		}
	}
	return visible, managed, admin, nil
}

const moduleColumns = `m.id::text,coalesce(m.project_id::text,''),m.repository_id::text,m.commit,m.subdirectory,m.name,m.instructions,m.example_links,m.archived,m.revision,r.url,r.enabled AND i.enabled AND NOT i.reconnect_required AND NOT m.archived AND (p.id IS NULL OR NOT p.archived)`
const moduleTables = ` FROM cosmoformer.module_entries m JOIN cosmoformer.repositories r ON r.id=m.repository_id JOIN cosmoformer.integrations i ON i.id=r.integration_id LEFT JOIN cosmoformer.projects p ON p.id=m.project_id `

func scanModule(row pgx.Row, m *ModulePublic) error {
	return row.Scan(&m.ID, &m.Project, &m.Repository, &m.Commit, &m.Directory, &m.Name, &m.Instructions, newJSON(&m.ExamplePaths), &m.Archived, &m.Revision, &m.RepositoryURL, &m.Available)
}
func moduleSource(repo, commit, dir string) string {
	suffix := ""
	if dir != "." {
		suffix = "//" + dir
	}
	return "git::" + repo + suffix + "?ref=" + commit
}
func revisionURL(repo, commit, p string) string {
	u, _ := url.Parse(strings.TrimSuffix(repo, ".git"))
	u.Path = strings.TrimSuffix(u.Path, "/") + "/src/" + commit + "/"
	if p != "." {
		u.Path += p
	}
	return u.String()
}
func decorateModule(m *ModulePublic) {
	m.Source = moduleSource(m.RepositoryURL, m.Commit, m.Directory)
	m.SourceURL = revisionURL(m.RepositoryURL, m.Commit, m.Directory)
	raw, _ := json.Marshal(m.Source)
	m.Snippet = fmt.Sprintf("module \"catalog_module\" {\n  source = %s\n}\n", raw)
	m.Examples = []ModuleExample{}
	for _, p := range m.ExamplePaths {
		m.Examples = append(m.Examples, ModuleExample{p, revisionURL(m.RepositoryURL, m.Commit, p)})
	}
}
func moduleFor(ctx context.Context, tx pgx.Tx, actor store.Actor, org, id string, manage bool) (ModulePublic, error) {
	var m ModulePublic
	if e := scanModule(tx.QueryRow(ctx, `SELECT `+moduleColumns+moduleTables+` WHERE m.id=$1`, id), &m); e != nil {
		if e == pgx.ErrNoRows {
			return m, store.ErrNotFound
		}
		return m, e
	}
	visible, managed, admin, e := moduleScopes(ctx, tx, actor, org)
	if e != nil {
		return m, e
	}
	contains := func(ids []string, p string) bool {
		for _, id := range ids {
			if id == p {
				return true
			}
		}
		return false
	}
	m.CanManage = admin || m.Project != "" && contains(managed, m.Project)
	readable := admin || m.Project == "" && len(visible) > 0 || contains(visible, m.Project)
	if manage && !m.CanManage || !manage && !readable {
		return m, store.ErrNotFound
	}
	decorateModule(&m)
	return m, nil
}
func validateModule(f ModuleFields) bool {
	if !source.Commit(f.Commit) || !catalog.RepoPath(f.Directory) || strings.TrimSpace(f.Name) == "" || !utf8.ValidString(f.Name) || strings.ContainsAny(f.Name, "\x00\r\n") || strings.TrimSpace(f.Instructions) == "" || !utf8.ValidString(f.Instructions) || strings.ContainsRune(f.Instructions, 0) || f.ExamplePaths == nil || len(f.ExamplePaths) > 16 {
		return false
	}
	seen := map[string]bool{}
	for _, p := range f.ExamplePaths {
		if !catalog.RepoPath(p) || seen[p] {
			return false
		}
		seen[p] = true
	}
	return true
}
func moduleRepository(ctx context.Context, tx pgx.Tx, actor store.Actor, org, repo, commit, dir string) error {
	var project, raw string
	e := tx.QueryRow(ctx, `SELECT r.project_id::text,r.url FROM cosmoformer.repositories r JOIN cosmoformer.integrations i ON i.id=r.integration_id WHERE r.id=$1 AND r.enabled AND i.enabled AND NOT i.reconnect_required`, repo).Scan(&project, &raw)
	if e == pgx.ErrNoRows {
		return store.ErrNotFound
	}
	if e != nil {
		return e
	}
	if e = manager(ctx, tx, actor, org, project, ""); e != nil {
		return e
	}
	// The literal must round-trip through the same grammar used by consumption.
	parsed, e := source.ParseModule(moduleSource(raw, commit, dir))
	if e != nil || parsed.Repository != raw || parsed.Commit != commit || parsed.Subdirectory != dir {
		return store.ErrConflict
	}
	return nil
}
func (a *Server) registerModules(api huma.API) {
	register(api, "GET", "/organizations/{org}/modules", "modules", func(ctx context.Context, in *ModuleListInput) (*ModulesOutput, error) {
		out := &ModulesOutput{}
		out.Body.Modules = []ModulePublic{}
		out.Body.Repositories = []ModuleRepository{}
		e := a.tx(ctx, in.Org, func(tx pgx.Tx, actor store.Actor) error {
			visible, managed, admin, e := moduleScopes(ctx, tx, actor, in.Org)
			if e != nil {
				return e
			}
			allowed := admin
			for _, p := range visible {
				if p == in.Project {
					allowed = true
				}
			}
			if in.Project != "" {
				var exists bool
				if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.projects WHERE id=$1 AND NOT archived)`, in.Project).Scan(&exists); e != nil {
					return e
				}
				if !exists || !allowed {
					return nil
				}
			}
			out.Body.CanRegisterShared = admin
			out.Body.CanRegister = admin
			for _, p := range managed {
				if p == in.Project {
					out.Body.CanRegister = true
				}
			}
			const scope = `($1::boolean OR m.project_id=ANY($2::uuid[]) OR m.project_id IS NULL AND cardinality($2::uuid[])>0) AND ($3='' OR m.project_id IS NULL OR m.project_id::text=$3) AND (NOT m.archived OR $4 AND ($1 OR m.project_id=ANY($7::uuid[]))) AND strpos(lower(m.name),lower($5))>0`
			rows, e := tx.Query(ctx, `SELECT `+moduleColumns+moduleTables+` WHERE `+scope+` AND ($6='' OR (m.name,m.id)>(SELECT m.name,m.id`+moduleTables+` WHERE `+scope+` AND m.id::text=$6)) ORDER BY m.name,m.id LIMIT $8`, admin, visible, in.Project, in.Archived, in.Query, in.Before, managed, in.Limit+1)
			if e != nil {
				return e
			}
			for rows.Next() {
				var m ModulePublic
				if e = scanModule(rows, &m); e != nil {
					rows.Close()
					return e
				}
				out.Body.Modules = append(out.Body.Modules, m)
			}
			e = rows.Err()
			rows.Close()
			if e != nil {
				return e
			}
			if len(out.Body.Modules) > in.Limit {
				out.Body.Modules = out.Body.Modules[:in.Limit]
				out.Body.Next = out.Body.Modules[in.Limit-1].ID
			}
			for i := range out.Body.Modules {
				m := &out.Body.Modules[i]
				m.CanManage = admin
				for _, p := range managed {
					m.CanManage = m.CanManage || p == m.Project
				}
				decorateModule(m)
			}
			if !out.Body.CanRegister {
				return nil
			}
			rows, e = tx.Query(ctx, `SELECT r.id::text,r.url FROM cosmoformer.repositories r JOIN cosmoformer.integrations i ON i.id=r.integration_id WHERE r.enabled AND i.enabled AND NOT i.reconnect_required AND ($1 OR r.project_id=ANY($2::uuid[])) ORDER BY r.url,r.id LIMIT 100`, admin, managed)
			if e != nil {
				return e
			}
			defer rows.Close()
			for rows.Next() {
				var r ModuleRepository
				if e = rows.Scan(&r.ID, &r.URL); e != nil {
					return e
				}
				out.Body.Repositories = append(out.Body.Repositories, r)
			}
			return rows.Err()
		})
		return out, e
	})
	read := func(ctx context.Context, in *ModuleReadInput, usage bool) (*ModuleOutput, error) {
		out := &ModuleOutput{}
		e := a.tx(ctx, in.Org, func(tx pgx.Tx, actor store.Actor) error {
			m, e := moduleFor(ctx, tx, actor, in.Org, in.ID, false)
			if e != nil {
				return e
			}
			if in.Revision != 0 && in.Revision != m.Revision {
				return store.ErrConflict
			}
			if usage && !m.Available {
				return store.ErrConflict
			}
			out.Body = m
			return nil
		})
		return out, e
	}
	register(api, "GET", "/organizations/{org}/modules/{id}", "module", func(ctx context.Context, in *ModuleReadInput) (*ModuleOutput, error) { return read(ctx, in, false) })
	register(api, "GET", "/organizations/{org}/modules/{id}/usage-instructions", "module-usage", func(ctx context.Context, in *ModuleReadInput) (*ModuleOutput, error) { return read(ctx, in, true) })
	register(api, "POST", "/organizations/{org}/modules", "register-module", func(ctx context.Context, in *ModuleCreate) (*IDResult, error) {
		if !validateModule(in.Body.ModuleFields) {
			return nil, problem(ctx, 422, "Supply a full commit, contained paths, plain instructions and unique example paths.")
		}
		return mutate(a, ctx, in.Org, false, func(tx pgx.Tx, actor store.Actor) error {
			if e := manager(ctx, tx, actor, in.Org, in.Body.Project, ""); e != nil {
				return e
			}
			return moduleRepository(ctx, tx, actor, in.Org, in.Body.Repository, in.Body.Commit, in.Body.Directory)
		}, func(tx pgx.Tx, actor store.Actor) (*IDResult, error) {
			var id string
			f := in.Body.ModuleFields
			e := tx.QueryRow(ctx, `INSERT INTO cosmoformer.module_entries(org_id,project_id,repository_id,commit,subdirectory,name,instructions,example_links,archived) VALUES($1,nullif($2,'')::uuid,$3,$4,$5,$6,$7,$8,$9) RETURNING id::text`, in.Org, in.Body.Project, f.Repository, f.Commit, f.Directory, f.Name, f.Instructions, jsonBytes(f.ExamplePaths), f.Archived).Scan(&id)
			if e == nil {
				e = store.Audit(ctx, tx, in.Org, actor.ID, "module.register", id)
			}
			return idResult(id), e
		})
	})
	register(api, "PATCH", "/organizations/{org}/modules/{id}", "edit-module", func(ctx context.Context, in *ModuleUpdate) (*IDResult, error) {
		if !validateModule(in.Body.ModuleFields) {
			return nil, problem(ctx, 422, "Supply a full commit, contained paths, plain instructions and unique example paths.")
		}
		return mutate(a, ctx, in.Org, false, func(tx pgx.Tx, actor store.Actor) error {
			m, e := moduleFor(ctx, tx, actor, in.Org, in.ID, true)
			if e != nil {
				return e
			}
			// Editing published metadata must remain possible after disconnecting
			// its source. Only a new source binding needs repository authorization.
			if m.Repository == in.Body.Repository && m.Commit == in.Body.Commit && m.Directory == in.Body.Directory {
				return nil
			}
			return moduleRepository(ctx, tx, actor, in.Org, in.Body.Repository, in.Body.Commit, in.Body.Directory)
		}, func(tx pgx.Tx, actor store.Actor) (*IDResult, error) {
			f := in.Body.ModuleFields
			tag, e := tx.Exec(ctx, `UPDATE cosmoformer.module_entries SET repository_id=$3,commit=$4,subdirectory=$5,name=$6,instructions=$7,example_links=$8,archived=$9,revision=revision+1 WHERE id=$1 AND revision=$2`, in.ID, in.Body.Revision, f.Repository, f.Commit, f.Directory, f.Name, f.Instructions, jsonBytes(f.ExamplePaths), f.Archived)
			if e != nil {
				return nil, e
			}
			if tag.RowsAffected() != 1 {
				return nil, store.ErrConflict
			}
			return idResult(in.ID), store.Audit(ctx, tx, in.Org, actor.ID, "module.update", in.ID)
		})
	})
}
