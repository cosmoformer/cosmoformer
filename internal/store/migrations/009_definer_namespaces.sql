-- Keep trusted relations ahead of caller-owned temporary relations. Merely
-- omitting pg_temp makes PostgreSQL search it before the named schemas.
-- Alter existing functions in place to preserve their identity and grants.
SET search_path = pg_catalog, cosmoformer, pg_temp;
DO $$
DECLARE helper regprocedure;
BEGIN
 FOR helper IN
  SELECT p.oid::regprocedure
  FROM pg_catalog.pg_proc p
  JOIN pg_catalog.pg_namespace n ON n.oid=p.pronamespace
  WHERE n.nspname='cosmoformer' AND p.prosecdef
 LOOP
  EXECUTE format('ALTER FUNCTION %s SET search_path = pg_catalog, cosmoformer, pg_temp',helper);
 END LOOP;
END $$;
