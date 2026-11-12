//go:build integration

package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/cosmoformer/cosmoformer/internal/cryptostore"
	"github.com/cosmoformer/cosmoformer/internal/identity"
	"github.com/cosmoformer/cosmoformer/internal/operator"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/cosmoformer/cosmoformer/internal/testsupport"
	"github.com/jackc/pgx/v5"
)

var operatorCtx = context.Background()

func mustOp(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func openOperator(t *testing.T, db *testsupport.Database) *operator.Operator {
	t.Helper()
	op, err := operator.Open(operatorCtx, db.OwnerDSN)
	mustOp(t, err)
	t.Cleanup(op.Close)
	return op
}
func stopEvidence(t *testing.T, op *operator.Operator, db *testsupport.Database) operator.Evidence {
	t.Helper()
	s, err := op.Status(operatorCtx)
	mustOp(t, err)
	ev := operator.Evidence{MaintenanceID: s.MaintenanceID, Reference: "synthetic-host-process-check", WritersStopped: true, CleanupStopped: true}
	rows, err := db.Owner.Query(operatorCtx, `SELECT id::text FROM cosmoformer.organizations ORDER BY id`)
	mustOp(t, err)
	var orgs []string
	for rows.Next() {
		var id string
		mustOp(t, rows.Scan(&id))
		orgs = append(orgs, id)
	}
	rows.Close()
	mustOp(t, rows.Err())
	for _, org := range orgs {
		sqlOK(t, db.Owner, `SELECT set_config('app.org_id',$1,false)`, org)
		rows, err = db.Owner.Query(operatorCtx, `SELECT id::text,generation,coalesce(agent_instance::text,'') FROM cosmoformer.workers`)
		mustOp(t, err)
		for rows.Next() {
			var w operator.WorkerStop
			mustOp(t, rows.Scan(&w.ID, &w.Generation, &w.Agent))
			w.State = "stopped"
			w.Reference = "synthetic-process-tree-check"
			ev.Workers = append(ev.Workers, w)
		}
		rows.Close()
		mustOp(t, rows.Err())
	}
	return ev
}
func testCipher(t *testing.T, n byte) *cryptostore.Cipher {
	t.Helper()
	c, e := cryptostore.New(bytes.Repeat([]byte{n}, 32))
	mustOp(t, e)
	return c
}
func registerSyntheticKey(t *testing.T, db *testsupport.Database, c *cryptostore.Cipher) {
	t.Helper()
	f := c.Fingerprint()
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.crypto_keys(fingerprint,state) VALUES($1,'active')`, f[:])
	sqlOK(t, db.Owner, `UPDATE cosmoformer.installation SET active_key_fingerprint=$1`, f[:])
}
func addSyntheticSecret(t *testing.T, db *testsupport.Database, c *cryptostore.Cipher, org string) string {
	t.Helper()
	id := runs.ID()
	v, e := c.Seal([]byte("synthetic-secret"), cryptostore.AAD(org, id, "managed_secret", 7), func([32]byte) error { return nil })
	mustOp(t, e)
	sqlOK(t, db.Owner, `SELECT set_config('app.org_id',$1,false)`, org)
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.secrets(org_id,id,name,encrypted_value,semantic_revision) VALUES($1,$2,'synthetic',$3,7)`, org, id, v)
	return id
}
func TestOperatorMaintenanceAndRotation(t *testing.T) {
	db := testsupport.NewDatabase(t)
	ctx := operatorCtx
	if op, e := operator.Open(ctx, db.App.Pool.Config().ConnString()); e == nil {
		op.Close()
		t.Fatal("runtime accepted as operator")
	}
	old, next := testCipher(t, 31), testCipher(t, 32)
	registerSyntheticKey(t, db, old)
	var orgs, ids []string
	for i := 0; i < 2; i++ {
		var org string
		mustOp(t, db.Owner.QueryRow(ctx, `INSERT INTO cosmoformer.organizations(name) VALUES('Synthetic rotation') RETURNING id::text`).Scan(&org))
		orgs = append(orgs, org)
		ids = append(ids, addSyntheticSecret(t, db, old, org))
	}
	op := openOperator(t, db)
	var unscoped int
	mustOp(t, op.Conn.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.secrets`).Scan(&unscoped))
	if unscoped != 0 {
		t.Fatal("operator gained a tenant bypass")
	}
	if e := op.Rotate(ctx, operator.Evidence{}, runs.ID(), old, next); e == nil {
		t.Fatal("rotation outside maintenance")
	}
	mustOp(t, op.Enter(ctx))
	ev := stopEvidence(t, op, db)
	if e := op.Offline(ctx, ev); e == nil {
		t.Fatal("in-flight normal database sessions accepted")
	}
	db.App.Pool.Close()
	mustOp(t, op.Offline(ctx, ev))
	// The durable latch fences a late writer even when it has valid credentials.
	app, e := pgx.Connect(ctx, db.App.Pool.Config().ConnString())
	mustOp(t, e)
	_, e = app.Exec(ctx, `SELECT cosmoformer.create_account(NULL,'late-writer','late')`)
	if e == nil {
		t.Fatal("late writer accepted")
	}
	app.Close(ctx)
	// AAD failure after quota allocation rolls back every ciphertext and consumes
	// the target key's whole reservation, including unused slots.
	sqlOK(t, db.Owner, `SELECT set_config('app.org_id',$1,false)`, orgs[1])
	sqlOK(t, db.Owner, `UPDATE cosmoformer.secrets SET encrypted_value='broken'::bytea WHERE id=$1`, ids[1])
	rotation := runs.ID()
	if e = op.Rotate(ctx, ev, rotation, old, next); e == nil {
		t.Fatal("unauthenticated ciphertext rotated")
	}
	fp := next.Fingerprint()
	var reserved int64
	mustOp(t, db.Owner.QueryRow(ctx, `SELECT reserved_encryptions FROM cosmoformer.crypto_keys WHERE fingerprint=$1`, fp[:]).Scan(&reserved))
	if reserved != 4096 {
		t.Fatal("failed rotation refunded quota", reserved)
	}
	sqlOK(t, db.Owner, `SELECT set_config('app.org_id',$1,false)`, orgs[0])
	var before []byte
	mustOp(t, db.Owner.QueryRow(ctx, `SELECT encrypted_value FROM cosmoformer.secrets WHERE id=$1`, ids[0]).Scan(&before))
	if _, e = old.Open(before, cryptostore.AAD(orgs[0], ids[0], "managed_secret", 7)); e != nil {
		t.Fatal("partial rotation committed")
	}
	replacement, _ := old.Seal([]byte("synthetic-secret"), cryptostore.AAD(orgs[1], ids[1], "managed_secret", 7), func([32]byte) error { return nil })
	sqlOK(t, db.Owner, `SELECT set_config('app.org_id',$1,false)`, orgs[1])
	sqlOK(t, db.Owner, `UPDATE cosmoformer.secrets SET encrypted_value=$1 WHERE id=$2`, replacement, ids[1])
	mustOp(t, op.Rotate(ctx, ev, runs.ID(), old, next))
	mustOp(t, db.Owner.QueryRow(ctx, `SELECT reserved_encryptions FROM cosmoformer.crypto_keys WHERE fingerprint=$1`, fp[:]).Scan(&reserved))
	if reserved != 8192 {
		t.Fatal("retry reused burned allowance")
	}
	for i, org := range orgs {
		sqlOK(t, db.Owner, `SELECT set_config('app.org_id',$1,false)`, org)
		var value []byte
		var semantic, revision int64
		mustOp(t, db.Owner.QueryRow(ctx, `SELECT encrypted_value,semantic_revision,revision FROM cosmoformer.secrets WHERE id=$1`, ids[i]).Scan(&value, &semantic, &revision))
		plain, e := next.Open(value, cryptostore.AAD(org, ids[i], "managed_secret", 7))
		mustOp(t, e)
		if string(plain) != "synthetic-secret" || semantic != 7 || revision != 1 {
			t.Fatal("semantic identity changed")
		}
	}
	if e = op.Rotate(ctx, ev, runs.ID(), old, next); e == nil {
		t.Fatal("rotation replay accepted")
	}
	if e = op.Exit(ctx, ev, oldFP(old)); e == nil {
		t.Fatal("old runtime key resumed")
	}
	mustOp(t, op.Exit(ctx, ev, fp[:]))
	s, e := op.Status(ctx)
	mustOp(t, e)
	if s.Maintenance || s.Offline {
		t.Fatal("explicit exit did not resume")
	}
	mustOp(t, op.Enter(ctx))
	if e = op.Offline(ctx, ev); e == nil {
		t.Fatal("stale stop evidence reused in new window")
	}
}
func oldFP(c *cryptostore.Cipher) []byte { f := c.Fingerprint(); return f[:] }
func TestOperatorBackupClockAndOwnership(t *testing.T) {
	db := testsupport.NewDatabase(t)
	x := evidenceFixture(t, db)
	old := testCipher(t, 52)
	registerSyntheticKey(t, db, old)
	op := openOperator(t, db)
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.worker_reservations(org_id,worker_id,attempt_id,generation,agent_instance) SELECT org_id,worker_id,id,generation,agent_instance FROM cosmoformer.attempts WHERE id=$1`, x["attempt"])
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.backend_reservations(org_id,target_id,attempt_id,stack_id) SELECT org_id,target_id,id,stack_id FROM cosmoformer.attempts WHERE id=$1`, x["attempt"])
	var artifact string
	mustOp(t, db.Owner.QueryRow(operatorCtx, `INSERT INTO cosmoformer.artifacts(org_id,attempt_id,kind,upload_id,bucket,object_key,state) VALUES($1,$2,'manifest',gen_random_uuid(),'synthetic','exact','uploading') RETURNING id::text`, x["org"], x["attempt"]).Scan(&artifact))
	// A second tenant has an independent exact version and recovery owner.
	// A historical clock error put its timestamp in the future; it still belongs
	// to this database snapshot and must not be omitted from backup protection.
	y := evidenceFixture(t, db)
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.artifacts(org_id,attempt_id,kind,upload_id,bucket,object_key,state,version_id,sha256,byte_count,finalized_at) VALUES($1,$2,'manifest',gen_random_uuid(),'synthetic-other','exact-other','finalized','other-v2',sha256('other'::bytea),5,clock_timestamp()+interval '1 day')`, y["org"], y["attempt"])
	sqlOK(t, db.Owner, `SELECT set_config('app.org_id',$1,false)`, x["org"])
	mustOp(t, op.Enter(operatorCtx))
	db.App.Pool.Close()
	ev := stopEvidence(t, op, db)
	ev.Workers[0].State = "idle"
	if e := op.Offline(operatorCtx, ev); e == nil {
		t.Fatal("idle asserted over unresolved execution")
	}
	ev.Workers[0].State = "stopped"
	if _, e := op.BackupCreate(operatorCtx, ev, runs.ID()); e == nil {
		t.Fatal("inflight artifact writer accepted")
	}
	sqlOK(t, db.Owner, `SELECT set_config('app.org_id',$1,false)`, x["org"])
	sqlOK(t, db.Owner, `UPDATE cosmoformer.artifacts SET state='finalized',version_id='exact-v1',sha256=sha256('synthetic'::bytea),byte_count=9,finalized_at=clock_timestamp() WHERE id=$1`, artifact)
	// Force a failure after the global set and an earlier tenant's holds have
	// been inserted. The complete capture must roll back across tenant scopes.
	sqlOK(t, db.Owner, `CREATE FUNCTION cosmoformer.fixture_reject_backup() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.owner_kind='backup' AND NEW.org_id=(SELECT max(id::text)::uuid FROM cosmoformer.organizations) THEN RAISE EXCEPTION 'synthetic capture interruption'; END IF; RETURN NEW; END $$; CREATE TRIGGER zz_fixture_reject_backup BEFORE INSERT ON cosmoformer.artifact_holds FOR EACH ROW EXECUTE FUNCTION cosmoformer.fixture_reject_backup()`)
	failed := runs.ID()
	if _, err := op.BackupCreate(operatorCtx, ev, failed); err == nil {
		t.Fatal("synthetic capture failure accepted")
	}
	var partial int
	mustOp(t, db.Owner.QueryRow(operatorCtx, `SELECT count(*) FROM cosmoformer.backup_sets WHERE id=$1`, failed).Scan(&partial))
	if partial != 0 {
		t.Fatal("partial backup capture committed")
	}
	sqlOK(t, db.Owner, `DROP TRIGGER zz_fixture_reject_backup ON cosmoformer.artifact_holds; DROP FUNCTION cosmoformer.fixture_reject_backup()`)
	a, b := runs.ID(), runs.ID()
	r, e := op.BackupCreate(operatorCtx, ev, a)
	mustOp(t, e)
	if r.Count != 2 {
		t.Fatal("wrong backup capture")
	}
	recovered, inspectErr := op.BackupInspect(operatorCtx, a)
	mustOp(t, inspectErr)
	if recovered.Digest != r.Digest || recovered.Count != 2 {
		t.Fatal("manifest recovery changed capture")
	}
	_, e = op.BackupCreate(operatorCtx, ev, b)
	mustOp(t, e)
	if _, e = op.BackupCreate(operatorCtx, ev, a); e == nil {
		t.Fatal("backup replay recaptured")
	}
	if e = op.BackupRelease(operatorCtx, ev, a); e == nil {
		t.Fatal("retention check omitted")
	}
	ev.RetentionChecked = true
	mustOp(t, op.BackupRelease(operatorCtx, ev, a))
	var protections int
	mustOp(t, db.Owner.QueryRow(operatorCtx, `SELECT count(*) FROM cosmoformer.artifact_holds WHERE owner_kind='backup' AND released_at IS NULL AND owner_id=$1`, b).Scan(&protections))
	if protections != 1 {
		t.Fatal("release affected another backup")
	}
	sqlOK(t, db.Owner, `UPDATE cosmoformer.installation SET clock_unreliable=true`)
	if e = op.Exit(operatorCtx, ev, oldFP(old)); e == nil {
		t.Fatal("unreliable clock resumed")
	}
	if e = op.ClockReset(operatorCtx, ev, true); e == nil {
		t.Fatal("missing clock validation accepted")
	}
	ev.ClockValidation = "synthetic-independent-utc-validation"
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.sessions(account_id,credential_revision,verifier,csrf_verifier) VALUES($1,1,sha256('clock-session'::bytea),sha256('clock-csrf'::bytea))`, x["account"])
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.account_proofs(account_id,purpose,credential_revision,verifier,expires_at) VALUES($1,'reset',1,sha256('clock-proof'::bytea),clock_timestamp()+interval '1 hour')`, x["account"])
	mustOp(t, op.ClockReset(operatorCtx, ev, true))
	var revoked bool
	mustOp(t, db.Owner.QueryRow(operatorCtx, `SELECT NOT EXISTS(SELECT FROM cosmoformer.sessions) AND NOT EXISTS(SELECT FROM cosmoformer.account_proofs)`).Scan(&revoked))
	if !revoked {
		t.Fatal("clock reset retained sessions/proofs")
	}
	var plans, reservations, open int
	mustOp(t, db.Owner.QueryRow(operatorCtx, `SELECT (SELECT count(*) FROM cosmoformer.plans WHERE status='invalidated'),(SELECT count(*) FROM cosmoformer.worker_reservations)+(SELECT count(*) FROM cosmoformer.backend_reservations),(SELECT count(*) FROM cosmoformer.attempts WHERE closed_at IS NULL)`).Scan(&plans, &reservations, &open))
	if plans != 1 || reservations != 2 || open != 1 {
		t.Fatal("clock recovery released execution ownership", plans, reservations, open)
	}
	s, e := op.Status(operatorCtx)
	mustOp(t, e)
	if !s.Maintenance || s.ClockUnreliable {
		t.Fatal("clock reset implicitly resumed")
	}
}

// TestOperatorRestoreExercise runs pg_dump/pg_restore in the marked disposable
// PostgreSQL container. It never accepts an application database or host dump.
func TestOperatorRestoreExercise(t *testing.T) {
	container := os.Getenv("COSMOFORMER_TEST_PG_CONTAINER")
	if container == "" {
		t.Fatal("restore exercise requires COSMOFORMER_TEST_PG_CONTAINER from scripts/operator-recovery.mjs")
	}
	docker, err := exec.LookPath("docker")
	mustOp(t, err)
	check, err := exec.Command(docker, "inspect", "--format", `{{index .Config.Labels "cosmoformer.local"}}`, container).Output()
	mustOp(t, err)
	if strings.TrimSpace(string(check)) != "test" {
		t.Fatal("restore container must be disposable and marked test")
	}
	db := testsupport.NewDatabase(t)
	target := testsupport.NewDatabase(t)
	ctx := operatorCtx
	x, y := evidenceFixture(t, db), evidenceFixture(t, db)
	old, next := testCipher(t, 71), testCipher(t, 72)
	registerSyntheticKey(t, db, old)
	historical := testCipher(t, 73)
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.crypto_keys(fingerprint,state,reserved_encryptions) VALUES($1,'rotation',4096)`, oldFP(historical))
	secret := addSyntheticSecret(t, db, old, x["org"])
	// All managed ciphertext purposes must survive rotation with their original
	// authenticated semantic revisions, including disabled restored credentials.
	sqlOK(t, db.Owner, `SELECT set_config('app.org_id',$1,false)`, x["org"])
	integration, _ := old.Seal([]byte("synthetic-oauth"), cryptostore.AAD(x["org"], x["integration"], "integration", 1), func([32]byte) error { return nil })
	sqlOK(t, db.Owner, `UPDATE cosmoformer.integrations SET encrypted_credentials=$1,enabled=true WHERE id=$2`, integration, x["integration"])
	credential := runs.ID()
	source, _ := old.Seal([]byte("synthetic-source"), cryptostore.AAD(x["org"], credential, "source_credential", 1), func([32]byte) error { return nil })
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.source_credentials(id,org_id,repository_id,encrypted_value) VALUES($1,$2,$3,$4)`, credential, x["org"], x["repo"], source)
	token := identity.Token()
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.sessions(account_id,credential_revision,verifier,csrf_verifier) VALUES($1,1,$2,$2)`, x["account"], identity.Digest(token))
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.account_proofs(account_id,purpose,credential_revision,verifier,expires_at) VALUES($1,'reset',1,$2,clock_timestamp()+interval '1 hour')`, x["account"], identity.Digest("synthetic-proof"))
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.oauth_states(org_id,integration_id,actor_id,verifier,session_verifier,expires_at) VALUES($1,$2,$3,$4,$4,clock_timestamp()+interval '1 hour')`, x["org"], x["integration"], x["account"], identity.Digest(token))
	// This stack exists at the cutoff; its actual backend adoption is newer.
	var lostStack, lostRevision string
	mustOp(t, db.Owner.QueryRow(ctx, `INSERT INTO cosmoformer.stacks(org_id,project_id,environment_id,repository_id,declaration_id,display_name) VALUES($1,$2,$3,$4,gen_random_uuid(),'Post-backup binding') RETURNING id::text`, x["org"], x["project"], x["env"], x["repo"]).Scan(&lostStack))
	mustOp(t, db.Owner.QueryRow(ctx, `INSERT INTO cosmoformer.stack_revisions(org_id,stack_id,commit,parsed_config,effective_config,semantic_digest,inventory_revision) VALUES($1,$2,'synthetic','{}','{}',sha256('synthetic'::bytea),1) RETURNING id::text`, x["org"], lostStack).Scan(&lostRevision))
	sqlOK(t, db.Owner, `UPDATE cosmoformer.stacks SET effective_revision_id=$1 WHERE id=$2`, lostRevision, lostStack)
	body := []byte("synthetic-exact-artifact")
	hash := sha256.Sum256(body)
	var artifact string
	mustOp(t, db.Owner.QueryRow(ctx, `INSERT INTO cosmoformer.artifacts(org_id,attempt_id,kind,upload_id,bucket,object_key,state,version_id,sha256,byte_count,finalized_at) VALUES($1,$2,'manifest',gen_random_uuid(),'synthetic','artifact','finalized','retained-v1',$3,$4,clock_timestamp()) RETURNING id::text`, x["org"], x["attempt"], hash[:], len(body)).Scan(&artifact))
	// The isolated artifact directory represents exact immutable object versions.
	objects := t.TempDir()
	exact := objects + "/retained-v1"
	mustOp(t, os.WriteFile(exact, body, 0600))
	mustOp(t, os.WriteFile(objects+"/latest", []byte("unrelated-latest"), 0600))
	op := openOperator(t, db)
	mustOp(t, op.Enter(ctx))
	db.App.Pool.Close()
	ev := stopEvidence(t, op, db)
	capture, e := op.BackupCreate(ctx, ev, runs.ID())
	mustOp(t, e)
	if capture.Count != 1 {
		t.Fatal("wrong exact-set count")
	}
	dumpName := db.Owner.Config().Database
	restoreName := target.Owner.Config().Database
	for _, name := range []string{dumpName, restoreName} {
		if !strings.HasPrefix(name, "cf_test_") || len(name) != 32 {
			t.Fatal("refusing non-generated database")
		}
	}
	dump, err := exec.Command(docker, "exec", container, "pg_dump", "-U", "postgres", "-Fc", "-d", dumpName).Output()
	mustOp(t, err)
	// Independently recorded post-cutoff external resources, absent from the dump.
	sqlOK(t, db.Owner, `SELECT set_config('app.org_id',$1,false)`, x["org"])
	sqlOK(t, db.Owner, `SELECT cosmoformer.bind_backend($1,$2,'aws','synthetic-post-backup','new.tfstate','s3',NULL)`, x["org"], lostStack)
	postWorker := runs.ID()
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.workers(id,org_id,pool_id,name,verifier) VALUES($1,$2,$3,'Post-backup worker',sha256('new-worker'::bytea))`, postWorker, x["org"], x["pool"])
	target.App.Pool.Close()
	sqlOK(t, target.Owner, `DROP SCHEMA cosmoformer CASCADE`)
	restore := exec.Command(docker, "exec", "-i", container, "pg_restore", "-U", "postgres", "--role=cosmoformer_owner", "--exit-on-error", "-d", restoreName)
	restore.Stdin = bytes.NewReader(dump)
	if output, err := restore.CombinedOutput(); err != nil {
		t.Fatalf("isolated pg_restore failed: %v: %s", err, output)
	}
	restored := openOperator(t, target)
	restoreID := runs.ID()
	mustOp(t, restored.RestoreBegin(ctx, restoreID))
	if e = restored.RestoreBegin(ctx, restoreID); e == nil {
		t.Fatal("restore begin replay accepted")
	}
	var revoked bool
	mustOp(t, target.Owner.QueryRow(ctx, `SELECT cosmoformer.session_identity($1) IS NULL AND NOT EXISTS(SELECT FROM cosmoformer.account_proofs) AND NOT EXISTS(SELECT FROM cosmoformer.accounts WHERE password_phc IS NOT NULL)`, identity.Digest(token)).Scan(&revoked))
	if !revoked {
		t.Fatal("restored credentials still authorize")
	}
	ev = stopEvidence(t, restored, target)
	if e = restored.RestoreReconcile(ctx, ev); e == nil {
		t.Fatal("incomplete inventory accepted")
	}
	if e = restored.Exit(ctx, ev, oldFP(old)); e == nil {
		t.Fatal("unreconciled restore resumed")
	}
	ev.InventoryComplete = true
	ev.ExternalWritersStopped = true
	ev.SourceReconciled = true
	ev.InventoryReference = "synthetic-external-emergency-inventory"
	ev.Workers = append(ev.Workers, operator.WorkerStop{ID: postWorker, State: "stopped", Reference: "post-backup-host-stop"})
	for _, f := range []map[string]string{x, y} {
		ev.Backends = append(ev.Backends, operator.Backend{Org: f["org"], Stack: f["stack"], Partition: "aws", Bucket: "fictional-evidence-bucket", Key: f["org"], LockProfile: "s3", Reference: "synthetic-backend-inspection"})
	}
	ev.Backends = append(ev.Backends, operator.Backend{Org: x["org"], Stack: lostStack, Partition: "aws", Bucket: "synthetic-post-backup", Key: "new.tfstate", LockProfile: "s3", Reference: "post-backup-binding-record"})
	mustOp(t, os.Remove(exact))
	// The exact retained version is missing while a latest object exists. The
	// reconciliation must mark it unavailable, without substituting the latter.
	ev.Artifacts = []operator.ArtifactCheck{{Org: x["org"], ID: artifact, Bucket: "synthetic", Key: "artifact", Version: "retained-v1", SHA256: hex.EncodeToString(hash[:]), Size: int64(len(body)), Available: false, Reference: "exact-version-not-found"}}
	ev.Workers[len(ev.Workers)-1].State = "idle"
	if e = restored.RestoreReconcile(ctx, ev); e == nil {
		t.Fatal("post-backup idle assertion accepted as restore stop proof")
	}
	ev.Workers[len(ev.Workers)-1].State = "stopped"
	mustOp(t, restored.RestoreReconcile(ctx, ev))
	if e = restored.RestoreReconcile(ctx, ev); e == nil {
		t.Fatal("reconcile replay accepted")
	}
	if e = restored.Rotate(ctx, ev, runs.ID(), old, next); e == nil {
		t.Fatal("restore accepted key without external fresh-key history")
	}
	ev.FreshKey = true
	ev.KeyHistoryReference = "synthetic-key-generation-custody"
	mustOp(t, restored.Rotate(ctx, ev, runs.ID(), old, next))
	if err := restored.Rotate(ctx, ev, runs.ID(), next, historical); err == nil {
		t.Fatal("actual restore renewed a historical unfinished key")
	}
	var historicalState string
	var historicalReserved int64
	mustOp(t, target.Owner.QueryRow(ctx, `SELECT state,reserved_encryptions FROM cosmoformer.crypto_keys WHERE fingerprint=$1`, oldFP(historical)).Scan(&historicalState, &historicalReserved))
	if historicalState != "retired" || historicalReserved != 4096 {
		t.Fatal("actual restore lost historical key quarantine/accounting")
	}
	if e = restored.Exit(ctx, ev, oldFP(next)); e == nil {
		t.Fatal("restore resumed before independent clock validation")
	}
	ev.ClockValidation = "synthetic-utc-check"
	mustOp(t, restored.ClockReset(ctx, ev, true))
	mustOp(t, restored.Exit(ctx, ev, oldFP(next)))
	var open, unresolved int
	for _, f := range []map[string]string{x, y} {
		sqlOK(t, target.Owner, `SELECT set_config('app.org_id',$1,false)`, f["org"])
		var n, u int
		mustOp(t, target.Owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM cosmoformer.recovery_cases WHERE status='open'),(SELECT count(*) FROM cosmoformer.attempts WHERE closed_at IS NULL)`).Scan(&n, &u))
		open += n
		unresolved += u
	}
	if open != 3 || unresolved != 2 {
		t.Fatal("restore failed to preserve ownership or adopt lost backend", open, unresolved)
	}
	sqlOK(t, target.Owner, `SELECT set_config('app.org_id',$1,false)`, x["org"])
	var state string
	mustOp(t, target.Owner.QueryRow(ctx, `SELECT state FROM cosmoformer.artifacts WHERE id=$1`, artifact).Scan(&state))
	if state != "unavailable" {
		t.Fatal("missing exact artifact became available")
	}
	var value []byte
	mustOp(t, target.Owner.QueryRow(ctx, `SELECT encrypted_value FROM cosmoformer.secrets WHERE id=$1`, secret).Scan(&value))
	plain, e := next.Open(value, cryptostore.AAD(x["org"], secret, "managed_secret", 7))
	mustOp(t, e)
	if string(plain) != "synthetic-secret" {
		t.Fatal("restored secret changed")
	}
	var disabled bool
	mustOp(t, target.Owner.QueryRow(ctx, `SELECT NOT EXISTS(SELECT FROM cosmoformer.oauth_states) AND NOT EXISTS(SELECT FROM cosmoformer.source_credentials WHERE enabled) AND NOT EXISTS(SELECT FROM cosmoformer.integrations WHERE enabled) AND NOT EXISTS(SELECT FROM cosmoformer.workers WHERE NOT paused)`).Scan(&disabled))
	if !disabled {
		t.Fatal("restored operational authority survived")
	}
	for _, r := range []struct{ id, purpose, column, table string }{{credential, "source_credential", "encrypted_value", "source_credentials"}, {x["integration"], "integration", "encrypted_credentials", "integrations"}} {
		mustOp(t, target.Owner.QueryRow(ctx, `SELECT `+r.column+` FROM cosmoformer.`+r.table+` WHERE id=$1`, r.id).Scan(&value))
		if _, e = next.Open(value, cryptostore.AAD(x["org"], r.id, r.purpose, 1)); e != nil {
			t.Fatal("credential AAD changed during restore", e)
		}
	}
	// Demonstrate the persistent exact backup manifest survives PostgreSQL restore.
	var count int
	mustOp(t, target.Owner.QueryRow(ctx, `SELECT count(*) FROM cosmoformer.backup_artifacts WHERE backup_id=$1`, capture.ID).Scan(&count))
	if count != 1 {
		t.Fatal("restore lost backup manifest")
	}
	b, _ := json.Marshal(map[string]any{"backup_artifacts": capture.Count, "recovery_cases": open, "unresolved_attempts": unresolved, "exact_missing": state})
	t.Log("isolated restore exercise:", string(b))
}

func TestOperatorInterruptedRotationSettlesBeforeStatus(t *testing.T) {
	db := testsupport.NewDatabase(t)
	ctx := operatorCtx
	old, next := testCipher(t, 81), testCipher(t, 82)
	registerSyntheticKey(t, db, old)
	var org string
	mustOp(t, db.Owner.QueryRow(ctx, `INSERT INTO cosmoformer.organizations(name) VALUES('Synthetic interruption') RETURNING id::text`).Scan(&org))
	addSyntheticSecret(t, db, old, org)
	op := openOperator(t, db)
	mustOp(t, op.Enter(ctx))
	db.App.Pool.Close()
	ev := stopEvidence(t, op, db)
	sqlOK(t, db.Owner, `CREATE FUNCTION cosmoformer.fixture_pause_rotation() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_sleep(10); RETURN NEW; END $$; CREATE TRIGGER zz_fixture_pause_rotation BEFORE UPDATE OF encrypted_value ON cosmoformer.secrets FOR EACH ROW EXECUTE FUNCTION cosmoformer.fixture_pause_rotation()`)
	var pid int
	mustOp(t, op.Conn.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	cancelled, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- op.Rotate(cancelled, ev, runs.ID(), old, next) }()
	deadline := time.After(5 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	waiting := true
	for waiting {
		select {
		case <-ticker.C:
			var sleeping bool
			mustOp(t, db.Owner.QueryRow(ctx, `SELECT EXISTS(SELECT FROM pg_stat_activity WHERE pid=$1 AND wait_event='PgSleep')`, pid).Scan(&sleeping))
			if sleeping {
				waiting = false
			}
		case err := <-done:
			t.Fatalf("rotation did not reach interruption barrier: %v", err)
		case <-deadline:
			t.Fatal("rotation interruption barrier unavailable")
		}
	}
	cancel()
	if err := <-done; err == nil {
		t.Fatal("interrupted ciphertext transaction committed")
	}
	op.Close()
	// The new operator session's advisory lock establishes that the old session
	// settled before interpreting the key identity and protected receipt.
	again := openOperator(t, db)
	status, err := again.Status(ctx)
	mustOp(t, err)
	if !status.Maintenance || !status.Offline || status.ActiveKey != hex.EncodeToString(oldFP(old)) {
		t.Fatal("interruption resumed or changed committed key")
	}
	var reserved int64
	mustOp(t, db.Owner.QueryRow(ctx, `SELECT reserved_encryptions FROM cosmoformer.crypto_keys WHERE fingerprint=$1`, oldFP(next)).Scan(&reserved))
	if reserved != 4096 {
		t.Fatal("interruption refunded quota", reserved)
	}
	sqlOK(t, db.Owner, `DROP TRIGGER zz_fixture_pause_rotation ON cosmoformer.secrets; DROP FUNCTION cosmoformer.fixture_pause_rotation()`)
	mustOp(t, again.Rotate(ctx, ev, runs.ID(), old, next))
	mustOp(t, db.Owner.QueryRow(ctx, `SELECT reserved_encryptions FROM cosmoformer.crypto_keys WHERE fingerprint=$1`, oldFP(next)).Scan(&reserved))
	if reserved != 8192 {
		t.Fatal("retry after interruption reused reservation")
	}
}

func TestOperatorRestoredRotationKeysStayRetiredAfterFreshKeyActivation(t *testing.T) {
	db := testsupport.NewDatabase(t)
	ctx := operatorCtx
	old, fresh, historical := testCipher(t, 101), testCipher(t, 102), testCipher(t, 103)
	registerSyntheticKey(t, db, old)
	var org string
	mustOp(t, db.Owner.QueryRow(ctx, `INSERT INTO cosmoformer.organizations(name) VALUES('Restored key history') RETURNING id::text`).Scan(&org))
	secret := addSyntheticSecret(t, db, old, org)
	// The restored snapshot predates encryptions made by an interrupted rotation;
	// neither its zero counter nor the word "rotation" proves the key is unused.
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.crypto_keys(fingerprint,state,reserved_encryptions) VALUES($1,'rotation',0)`, oldFP(historical))
	op := openOperator(t, db)
	db.App.Pool.Close()
	mustOp(t, op.RestoreBegin(ctx, runs.ID()))
	// Old active-key counters are untrustworthy too: they become decrypt-only.
	if _, err := db.Owner.Exec(ctx, `SELECT cosmoformer.reserve_encryptions($1,1)`, oldFP(old)); err == nil {
		t.Fatal("restored old-key allowance still writable")
	}
	ev := stopEvidence(t, op, db)
	ev.InventoryComplete = true
	ev.ExternalWritersStopped = true
	ev.SourceReconciled = true
	ev.InventoryReference = "independent-complete-inventory"
	mustOp(t, op.RestoreReconcile(ctx, ev))
	ev.FreshKey = true
	ev.KeyHistoryReference = "independent-new-key-history"
	mustOp(t, op.Rotate(ctx, ev, runs.ID(), old, fresh))
	ev.ClockValidation = "independent-utc-check"
	mustOp(t, op.ClockReset(ctx, ev, true))
	mustOp(t, op.Exit(ctx, ev, oldFP(fresh)))
	// A later, ordinary maintenance window must not renew the old candidate's
	// rolled-back quota after the first restore rotation cleared the key latch.
	mustOp(t, op.Enter(ctx))
	ev = stopEvidence(t, op, db)
	if err := op.Rotate(ctx, ev, runs.ID(), fresh, historical); err == nil {
		t.Fatal("historical unfinished key reactivated after restore")
	}
	var state string
	var reserved int64
	mustOp(t, db.Owner.QueryRow(ctx, `SELECT state,reserved_encryptions FROM cosmoformer.crypto_keys WHERE fingerprint=$1`, oldFP(historical)).Scan(&state, &reserved))
	if state != "retired" || reserved != 0 {
		t.Fatal("restored quota changed or became usable", state, reserved)
	}
	status, err := op.Status(ctx)
	mustOp(t, err)
	if status.ActiveKey != hex.EncodeToString(oldFP(fresh)) || !status.Maintenance {
		t.Fatal("rejected historical key changed authority")
	}
	sqlOK(t, db.Owner, `SELECT set_config('app.org_id',$1,false)`, org)
	var encrypted []byte
	mustOp(t, db.Owner.QueryRow(ctx, `SELECT encrypted_value FROM cosmoformer.secrets WHERE id=$1`, secret).Scan(&encrypted))
	if plain, err := fresh.Open(encrypted, cryptostore.AAD(org, secret, "managed_secret", 7)); err != nil || string(plain) != "synthetic-secret" {
		t.Fatal("rejected historical key affected current ciphertext", err)
	}
}

func TestOperatorWarnsBeforeTheQuotaBlockCrossesNinetyPercent(t *testing.T) {
	db := testsupport.NewDatabase(t)
	c := testCipher(t, 104)
	registerSyntheticKey(t, db, c)
	op := openOperator(t, db)
	lastBlockBeforeNinety := int64((uint64(1) << 32) * 9 / 10 / 4096 * 4096)
	for _, tc := range []struct {
		count   int64
		warning bool
	}{{lastBlockBeforeNinety - 4096, false}, {lastBlockBeforeNinety, true}} {
		sqlOK(t, db.Owner, `UPDATE cosmoformer.crypto_keys SET reserved_encryptions=$1`, tc.count)
		status, err := op.Status(operatorCtx)
		mustOp(t, err)
		if status.RotationRecommended != tc.warning || status.ActiveReserved != tc.count {
			t.Fatal("quota warning boundary", tc.count, status.RotationRecommended)
		}
	}
}

func TestOperatorRestoreRejectsAmbiguousInventoryWithoutPartialAdoption(t *testing.T) {
	db := testsupport.NewDatabase(t)
	ctx := operatorCtx
	x := evidenceFixture(t, db)
	registerSyntheticKey(t, db, testCipher(t, 121))
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.worker_reservations(org_id,worker_id,attempt_id,generation,agent_instance) SELECT org_id,worker_id,id,generation,agent_instance FROM cosmoformer.attempts WHERE id=$1`, x["attempt"])
	sqlOK(t, db.Owner, `INSERT INTO cosmoformer.backend_reservations(org_id,target_id,attempt_id,stack_id) SELECT org_id,target_id,id,stack_id FROM cosmoformer.attempts WHERE id=$1`, x["attempt"])
	var otherOrg, newStack, newRevision, artifact string
	mustOp(t, db.Owner.QueryRow(ctx, `INSERT INTO cosmoformer.organizations(name) VALUES('Other restoration tenant') RETURNING id::text`).Scan(&otherOrg))
	mustOp(t, db.Owner.QueryRow(ctx, `INSERT INTO cosmoformer.stacks(org_id,project_id,environment_id,repository_id,declaration_id,display_name) VALUES($1,$2,$3,$4,gen_random_uuid(),'Post-backup adoption') RETURNING id::text`, x["org"], x["project"], x["env"], x["repo"]).Scan(&newStack))
	mustOp(t, db.Owner.QueryRow(ctx, `INSERT INTO cosmoformer.stack_revisions(org_id,stack_id,commit,parsed_config,effective_config,semantic_digest,inventory_revision) VALUES($1,$2,'synthetic','{}','{}',sha256('synthetic'::bytea),1) RETURNING id::text`, x["org"], newStack).Scan(&newRevision))
	sqlOK(t, db.Owner, `UPDATE cosmoformer.stacks SET effective_revision_id=$1 WHERE id=$2`, newRevision, newStack)
	hash := sha256.Sum256([]byte("x"))
	mustOp(t, db.Owner.QueryRow(ctx, `INSERT INTO cosmoformer.artifacts(org_id,attempt_id,kind,upload_id,bucket,object_key,state,version_id,sha256,byte_count,finalized_at) VALUES($1,$2,'manifest',gen_random_uuid(),'synthetic','exact','finalized','v1',$3,1,clock_timestamp()) RETURNING id::text`, x["org"], x["attempt"], hash[:]).Scan(&artifact))
	db.App.Pool.Close()
	op := openOperator(t, db)
	mustOp(t, op.RestoreBegin(ctx, runs.ID()))
	ev := stopEvidence(t, op, db)
	ev.InventoryComplete = true
	ev.ExternalWritersStopped = true
	ev.SourceReconciled = true
	ev.InventoryReference = "independent-inventory"
	binding := operator.Backend{Org: x["org"], Stack: x["stack"], Partition: "aws", Bucket: "fictional-evidence-bucket", Key: x["org"], LockProfile: "s3", Reference: "verified-existing-binding"}
	adopted := operator.Backend{Org: x["org"], Stack: newStack, Partition: "aws", Bucket: "synthetic-adoption", Key: "state.tfstate", LockProfile: "s3", Reference: "verified-post-backup-binding"}
	exact := operator.ArtifactCheck{Org: x["org"], ID: artifact, Bucket: "synthetic", Key: "exact", Version: "v1", SHA256: hex.EncodeToString(hash[:]), Size: 1, Available: true, Reference: "independently-verified-exact-version"}
	ev.Backends = []operator.Backend{binding}
	ev.Artifacts = []operator.ArtifactCheck{exact}
	sqlOK(t, db.Owner, `SELECT set_config('app.org_id',$1,false)`, x["org"])
	var epoch, revision int64
	mustOp(t, db.Owner.QueryRow(ctx, `SELECT (SELECT state_epoch FROM cosmoformer.backend_targets WHERE id=$1),(SELECT evidence_revision FROM cosmoformer.recovery_cases WHERE id=$2)`, x["target"], x["recovery"]).Scan(&epoch, &revision))
	cases := []struct {
		name, message string
		change        func(*operator.Evidence)
	}{
		{"tenant mismatch", "backend adoption failed", func(e *operator.Evidence) { b := binding; b.Org = otherOrg; e.Backends = []operator.Backend{b} }},
		{"changed binding", "backend inventory conflicts", func(e *operator.Evidence) {
			b := binding
			b.Key = "different.tfstate"
			e.Backends = []operator.Backend{b}
		}},
		{"duplicate binding", "invalid or duplicate backend inventory", func(e *operator.Evidence) { e.Backends = []operator.Backend{binding, binding} }},
		{"partial adoption with missing binding", "backend inventory omits a retained binding", func(e *operator.Evidence) { e.Backends = []operator.Backend{adopted} }},
		{"substituted object version", "artifact check must name the exact retained version", func(e *operator.Evidence) {
			a := exact
			a.Version = "latest"
			e.Artifacts = []operator.ArtifactCheck{a}
		}},
		{"unknown object", "artifact check names an unknown artifact", func(e *operator.Evidence) { a := exact; a.ID = runs.ID(); e.Artifacts = []operator.ArtifactCheck{a} }},
		{"duplicate object check", "invalid artifact verification evidence", func(e *operator.Evidence) { e.Artifacts = []operator.ArtifactCheck{exact, exact} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evidence := ev
			tc.change(&evidence)
			if err := op.RestoreReconcile(ctx, evidence); err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("inventory was not conservatively rejected: %v", err)
			}
			sqlOK(t, db.Owner, `SELECT set_config('app.org_id',$1,false)`, x["org"])
			var currentEpoch, currentRevision int64
			var state string
			var targets, reservations int
			mustOp(t, db.Owner.QueryRow(ctx, `SELECT (SELECT state_epoch FROM cosmoformer.backend_targets WHERE id=$1),(SELECT evidence_revision FROM cosmoformer.recovery_cases WHERE id=$2),(SELECT state FROM cosmoformer.artifacts WHERE id=$3),(SELECT count(*) FROM cosmoformer.backend_targets),(SELECT count(*) FROM cosmoformer.backend_reservations)+(SELECT count(*) FROM cosmoformer.worker_reservations)`, x["target"], x["recovery"], artifact).Scan(&currentEpoch, &currentRevision, &state, &targets, &reservations))
			if currentEpoch != epoch || currentRevision != revision || state != "finalized" || targets != 1 || reservations != 2 {
				t.Fatal("rejected inventory partially changed custody or evidence")
			}
			status, err := op.Status(ctx)
			mustOp(t, err)
			if !status.Maintenance || !status.Offline || status.RestoreReconciled {
				t.Fatal("rejected inventory relaxed maintenance")
			}
		})
	}
	ev.Backends = []operator.Backend{adopted, binding}
	mustOp(t, op.RestoreReconcile(ctx, ev))
	var casesOpen, reservations int
	sqlOK(t, db.Owner, `SELECT set_config('app.org_id',$1,false)`, x["org"])
	mustOp(t, db.Owner.QueryRow(ctx, `SELECT (SELECT count(*) FROM cosmoformer.recovery_cases WHERE status='open'),(SELECT count(*) FROM cosmoformer.backend_reservations)+(SELECT count(*) FROM cosmoformer.worker_reservations)`).Scan(&casesOpen, &reservations))
	if casesOpen != 2 || reservations != 2 {
		t.Fatal("successful adoption bypassed recovery or released execution")
	}
}

func TestOperatorOfflineRequiresCompleteIndependentStopEvidence(t *testing.T) {
	db := testsupport.NewDatabase(t)
	x := evidenceFixture(t, db)
	db.App.Pool.Close()
	op := openOperator(t, db)
	mustOp(t, op.Enter(operatorCtx))
	ev := stopEvidence(t, op, db)
	for _, tc := range []struct {
		name   string
		change func(*operator.Evidence)
	}{
		{"writers not proven stopped", func(e *operator.Evidence) { e.WritersStopped = false }},
		{"cleanup not proven stopped", func(e *operator.Evidence) { e.CleanupStopped = false }},
		{"missing worker", func(e *operator.Evidence) { e.Workers = nil }},
		{"stale generation", func(e *operator.Evidence) { w := e.Workers[0]; w.Generation++; e.Workers = []operator.WorkerStop{w} }},
		{"duplicate worker", func(e *operator.Evidence) { e.Workers = append([]operator.WorkerStop{}, e.Workers[0], e.Workers[0]) }},
		{"missing external reference", func(e *operator.Evidence) { w := e.Workers[0]; w.Reference = ""; e.Workers = []operator.WorkerStop{w} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			evidence := ev
			tc.change(&evidence)
			if err := op.Offline(operatorCtx, evidence); err == nil {
				t.Fatal("missing stop facts accepted")
			}
			status, err := op.Status(operatorCtx)
			mustOp(t, err)
			if status.Offline || !status.Maintenance {
				t.Fatal("unverified offline transition committed")
			}
			sqlOK(t, db.Owner, `SELECT set_config('app.org_id',$1,false)`, x["org"])
			var unresolved bool
			mustOp(t, db.Owner.QueryRow(operatorCtx, `SELECT closed_at IS NULL FROM cosmoformer.attempts WHERE id=$1`, x["attempt"]).Scan(&unresolved))
			if !unresolved {
				t.Fatal("stop assertion released unresolved execution")
			}
		})
	}
}

func TestOperatorRotationDatabaseFailuresNeverActivatePartialCiphertext(t *testing.T) {
	for _, tc := range []struct {
		name, table, event, predicate string
		reserved, atCommit            bool
	}{
		{"new-key registration", "crypto_keys", "INSERT", "true", false, false},
		{"quota receipt", "key_rotations", "INSERT", "true", false, false},
		{"ciphertext update", "secrets", "UPDATE", "true", true, false},
		{"old-key retirement", "crypto_keys", "UPDATE", "NEW.state='retired'", true, false},
		{"new-key activation", "crypto_keys", "UPDATE", "NEW.state='active'", true, false},
		{"installation handoff", "installation", "UPDATE", "NEW.active_key_fingerprint IS DISTINCT FROM OLD.active_key_fingerprint", true, false},
		{"commit receipt", "key_rotations", "UPDATE", "NEW.committed_at IS NOT NULL", true, false},
		{"deferred commit failure", "operator_events", "INSERT", "NEW.action='key.rotate'", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := testsupport.NewDatabase(t)
			ctx := operatorCtx
			old, next := testCipher(t, 131), testCipher(t, 132)
			registerSyntheticKey(t, db, old)
			var orgs, ids []string
			for i := 0; i < 2; i++ {
				var org string
				mustOp(t, db.Owner.QueryRow(ctx, `INSERT INTO cosmoformer.organizations(name) VALUES('Rotation rollback tenant') RETURNING id::text`).Scan(&org))
				orgs = append(orgs, org)
				ids = append(ids, addSyntheticSecret(t, db, old, org))
			}
			db.App.Pool.Close()
			op := openOperator(t, db)
			mustOp(t, op.Enter(ctx))
			ev := stopEvidence(t, op, db)
			// Closed fixture predicates only. Fail a real PostgreSQL write at each
			// durable accounting/handoff boundary, including a deferred commit failure.
			sqlOK(t, db.Owner, `CREATE FUNCTION cosmoformer.fixture_rotation_failure() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF `+tc.predicate+` THEN RAISE EXCEPTION 'synthetic rotation database failure'; END IF; RETURN NEW; END $$`)
			trigger := `CREATE TRIGGER fixture_rotation_failure BEFORE ` + tc.event + ` ON cosmoformer.` + tc.table + ` FOR EACH ROW EXECUTE FUNCTION cosmoformer.fixture_rotation_failure()`
			if tc.atCommit {
				trigger = `CREATE CONSTRAINT TRIGGER fixture_rotation_failure AFTER INSERT ON cosmoformer.operator_events DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION cosmoformer.fixture_rotation_failure()`
			}
			sqlOK(t, db.Owner, trigger)
			err := op.Rotate(ctx, ev, runs.ID(), old, next)
			if err == nil {
				t.Fatal("rotation succeeded across injected database failure")
			}
			if tc.atCommit && !strings.Contains(err.Error(), "operator commit uncertain") {
				t.Fatal("commit uncertainty was not reported conservatively", err)
			}
			status, err := op.Status(ctx)
			mustOp(t, err)
			if !status.Maintenance || !status.Offline || status.ActiveKey != hex.EncodeToString(oldFP(old)) {
				t.Fatal("database failure activated a key or resumed writers")
			}
			var allocated int64
			mustOp(t, db.Owner.QueryRow(ctx, `SELECT coalesce((SELECT reserved_encryptions FROM cosmoformer.crypto_keys WHERE fingerprint=$1),0)`, oldFP(next)).Scan(&allocated))
			expected := int64(0)
			if tc.reserved {
				expected = 4096
			}
			if allocated != expected {
				t.Fatal("reservation was refunded or allocated before a committed quota receipt", allocated, expected)
			}
			var activeOld, committedNew bool
			mustOp(t, db.Owner.QueryRow(ctx, `SELECT EXISTS(SELECT FROM cosmoformer.crypto_keys WHERE fingerprint=$1 AND state='active'),EXISTS(SELECT FROM cosmoformer.key_rotations WHERE new_fingerprint=$2 AND committed_at IS NOT NULL)`, oldFP(old), oldFP(next)).Scan(&activeOld, &committedNew))
			if !activeOld || committedNew {
				t.Fatal("partial cryptographic metadata committed")
			}
			for i, org := range orgs {
				sqlOK(t, db.Owner, `SELECT set_config('app.org_id',$1,false)`, org)
				var encrypted []byte
				var revision int64
				mustOp(t, db.Owner.QueryRow(ctx, `SELECT encrypted_value,semantic_revision FROM cosmoformer.secrets WHERE id=$1`, ids[i]).Scan(&encrypted, &revision))
				plain, err := old.Open(encrypted, cryptostore.AAD(org, ids[i], "managed_secret", 7))
				if err != nil || string(plain) != "synthetic-secret" || revision != 7 {
					t.Fatal("failure partially replaced tenant ciphertext", err)
				}
			}
		})
	}
}
