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

const SchemaVersion int32 = 12

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
	if e = s.Pool.QueryRow(ctx, `SELECT NOT EXISTS (
 SELECT FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
 WHERE n.nspname='cosmoformer' AND p.prosecdef
 AND (p.proowner<>'cosmoformer_owner'::regrole
 OR NOT coalesce('search_path=pg_catalog, cosmoformer, pg_temp'=ANY(p.proconfig),false)))`).Scan(&safe); e != nil || !safe {
		return errors.New("database definer helpers have incompatible namespace protections")
	}
	if e = s.Pool.QueryRow(ctx, `SELECT
 NOT has_table_privilege(current_user,'cosmoformer.backup_sets','SELECT,INSERT,UPDATE,DELETE')
 AND (SELECT count(*)=3 FROM pg_class WHERE oid IN ('cosmoformer.artifacts'::regclass,'cosmoformer.artifact_holds'::regclass,'cosmoformer.cleanup_claims'::regclass) AND relrowsecurity AND relforcerowsecurity)
 AND EXISTS(SELECT FROM pg_constraint WHERE conrelid='cosmoformer.artifacts'::regclass AND conname='artifacts_finalized_identity' AND convalidated)
 AND (SELECT count(*)=3 FROM pg_trigger WHERE tgrelid IN ('cosmoformer.artifacts'::regclass,'cosmoformer.artifact_holds'::regclass,'cosmoformer.cleanup_claims'::regclass) AND tgname IN ('artifacts_integrity','artifact_holds_integrity','cleanup_claims_integrity') AND tgenabled='O')`).Scan(&safe); e != nil || !safe {
		return errors.New("database evidence schema has incompatible protections")
	}
	if e = s.Pool.QueryRow(ctx, `SELECT
 (SELECT count(*)=2 FROM pg_trigger WHERE (tgrelid='cosmoformer.attempts'::regclass AND tgname='native_protocol_identity' OR tgrelid='cosmoformer.artifacts'::regclass AND tgname='native_artifact_stage') AND tgenabled='O')
 AND NOT has_table_privilege(current_user,'cosmoformer.backend_targets','SELECT,INSERT,UPDATE,DELETE')
 AND has_function_privilege(current_user,'cosmoformer.execution_binding_matches(uuid,text,text,text,text,text)','EXECUTE')
 AND has_function_privilege(current_user,'cosmoformer.execution_attention(uuid)','EXECUTE')`).Scan(&safe); e != nil || !safe {
		return errors.New("database native protocol has incompatible protections")
	}
	if e = s.Pool.QueryRow(ctx, `SELECT (SELECT count(*)=2 FROM pg_trigger WHERE (tgrelid='cosmoformer.operations'::regclass AND tgname='planning_intent_identity' OR tgrelid='cosmoformer.plans'::regclass AND tgname='ready_plan_identity') AND tgenabled='O')`).Scan(&safe); e != nil || !safe {
		return errors.New("database planning identities have incompatible protections")
	}
	if e = s.Pool.QueryRow(ctx, `SELECT
 (SELECT count(*)=5 FROM pg_trigger WHERE (tgrelid='cosmoformer.approvals'::regclass AND tgname='approval_event_integrity' OR tgrelid='cosmoformer.plan_votes'::regclass AND tgname='plan_vote_integrity' OR tgrelid='cosmoformer.projects'::regclass AND tgname='approval_policy_revision' OR tgrelid='cosmoformer.runtime_catalog'::regclass AND tgname='invalidate_runtime_plans' OR tgrelid='cosmoformer.backend_targets'::regclass AND tgname='invalidate_epoch_plans') AND tgenabled='O')
 AND (SELECT count(*)=14 FROM pg_trigger WHERE tgname='invalidate_reviewed_input' AND tgrelid IN ('cosmoformer.stacks'::regclass,'cosmoformer.projects'::regclass,'cosmoformer.environments'::regclass,'cosmoformer.repositories'::regclass,'cosmoformer.integrations'::regclass,'cosmoformer.worker_pools'::regclass,'cosmoformer.secrets'::regclass,'cosmoformer.source_credentials'::regclass,'cosmoformer.resource_grants'::regclass,'cosmoformer.aws_connections'::regclass,'cosmoformer.stack_bindings'::regclass,'cosmoformer.artifacts'::regclass,'cosmoformer.cleanup_claims'::regclass,'cosmoformer.recovery_cases'::regclass) AND tgenabled='O')
 AND EXISTS(SELECT FROM pg_trigger WHERE tgrelid='cosmoformer.plans'::regclass AND tgname='release_invalid_plan_holds' AND tgenabled='O')
 AND (SELECT count(*)=2 FROM pg_class WHERE oid IN ('cosmoformer.approvals'::regclass,'cosmoformer.plan_votes'::regclass) AND relrowsecurity AND relforcerowsecurity)
 AND EXISTS(SELECT FROM pg_attribute WHERE attrelid='cosmoformer.approvals'::regclass AND attname='manifest_digest' AND NOT attisdropped)
 AND EXISTS(SELECT FROM pg_attribute WHERE attrelid='cosmoformer.approvals'::regclass AND attname='source_commit' AND NOT attisdropped)`).Scan(&safe); e != nil || !safe {
		return errors.New("database approvals have incompatible protections")
	}
	if e = s.Pool.QueryRow(ctx, `SELECT
 (SELECT count(*)=7 FROM pg_trigger WHERE tgname IN ('recovery_approval_event_integrity','control_identity','recovery_identity','recovery_vote_integrity','recovery_hold_artifact','recovery_operation_identity','recovery_plan_identity') AND tgrelid IN ('cosmoformer.approvals'::regclass,'cosmoformer.control_requests'::regclass,'cosmoformer.recovery_cases'::regclass,'cosmoformer.recovery_votes'::regclass,'cosmoformer.artifacts'::regclass,'cosmoformer.operations'::regclass,'cosmoformer.plans'::regclass) AND tgenabled='O')
 AND (SELECT relrowsecurity AND relforcerowsecurity FROM pg_class WHERE oid='cosmoformer.recovery_evidence_versions'::regclass)
 AND NOT has_table_privilege(current_user,'cosmoformer.recovery_evidence_versions','UPDATE,DELETE')`).Scan(&safe); e != nil || !safe {
		return errors.New("database recovery has incompatible protections")
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
