package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/tern/v2/migrate"
)

//go:embed migrations/*.sql
var migrationFiles embed.FS

const SchemaVersion int32 = 5

var ErrForbidden = errors.New("insufficient permission")
var ErrNotFound = errors.New("not found")
var ErrConflict = errors.New("record changed or conflicts with an existing record")
var ErrUnauthenticated = errors.New("sign in required")

type Store struct{ Pool *pgxpool.Pool }

func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, e := pgxpool.ParseConfig(dsn)
	if e != nil {
		return nil, errors.New("invalid database configuration")
	}
	cfg.MaxConns = 12
	p, e := pgxpool.NewWithConfig(ctx, cfg)
	if e != nil {
		return nil, errors.New("database unavailable")
	}
	s := &Store{p}
	if e = s.Ready(ctx); e != nil {
		p.Close()
		return nil, e
	}
	return s, nil
}
func (s *Store) Ready(ctx context.Context) error {
	var version int32
	var safe bool
	if e := s.Pool.QueryRow(ctx, `SELECT version FROM cosmoformer.schema_version`).Scan(&version); e != nil || version != SchemaVersion {
		return errors.New("unsupported database schema; run explicit migrations")
	}
	e := s.Pool.QueryRow(ctx, `SELECT current_user='cosmoformer_app' AND NOT r.rolsuper AND NOT r.rolbypassrls AND NOT r.rolcreaterole AND NOT r.rolcreatedb AND NOT pg_has_role(current_user,'cosmoformer_owner','MEMBER') AND NOT has_schema_privilege(current_user,'cosmoformer','CREATE') AND NOT EXISTS(SELECT FROM pg_class c JOIN pg_namespace n ON n.oid=c.relnamespace WHERE n.nspname='cosmoformer' AND c.relowner=r.oid) AND NOT has_table_privilege(current_user,'cosmoformer.accounts','SELECT') AND has_function_privilege(current_user,'cosmoformer.session_identity(bytea)','EXECUTE') FROM pg_roles r WHERE rolname=current_user`).Scan(&safe)
	if e != nil || !safe {
		return errors.New("database runtime role has incompatible privileges")
	}
	return nil
}
func MigrationNames() ([]string, error) { return fs.Glob(migrationFiles, "migrations/*.sql") }
func Migrate(ctx context.Context, dsn string) error {
	c, e := pgx.Connect(ctx, dsn)
	if e != nil {
		return errors.New("migration connection unavailable")
	}
	defer c.Close(ctx)
	var safe bool
	e = c.QueryRow(ctx, `SELECT current_user='cosmoformer_owner' AND NOT r.rolsuper AND NOT r.rolbypassrls AND NOT r.rolcreaterole AND NOT r.rolcreatedb AND EXISTS(SELECT FROM pg_roles a WHERE a.rolname='cosmoformer_app' AND NOT a.rolsuper AND NOT a.rolbypassrls AND NOT a.rolcreaterole AND NOT a.rolcreatedb AND NOT pg_has_role(a.oid,r.oid,'MEMBER')) FROM pg_roles r WHERE rolname=current_user`).Scan(&safe)
	if e != nil || !safe {
		return errors.New("operator-provisioned owner/app roles are missing or unsafe")
	}
	if _, e = c.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS cosmoformer AUTHORIZATION cosmoformer_owner`); e != nil {
		return fmt.Errorf("create application schema: %w", e)
	}
	m, e := migrate.NewMigrator(ctx, c, "cosmoformer.schema_version")
	if e != nil {
		return e
	}
	sub, e := fs.Sub(migrationFiles, "migrations")
	if e != nil {
		return e
	}
	if e = m.LoadMigrations(sub); e != nil {
		return e
	}
	current, e := m.GetCurrentVersion(ctx)
	if e != nil {
		return e
	}
	if current > SchemaVersion {
		return errors.New("FORWARD_ONLY_MIGRATIONS: database is newer than this binary")
	}
	if len(m.Migrations) != int(SchemaVersion) {
		return errors.New("invalid embedded migration inventory")
	}
	for i, migration := range m.Migrations {
		if migration.Sequence != int32(i+1) || migration.DownSQL != "" {
			return errors.New("invalid forward-only migration set")
		}
	}
	if e = m.MigrateTo(ctx, SchemaVersion); e != nil {
		return fmt.Errorf("migrate: %w", e)
	}
	_, e = c.Exec(ctx, `GRANT SELECT ON cosmoformer.schema_version TO cosmoformer_app`)
	return e
}
