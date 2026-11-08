import { mkdirSync, writeFileSync, readFileSync, existsSync, chmodSync, rmSync } from 'node:fs';
import { randomBytes } from 'node:crypto';
import { spawnSync } from 'node:child_process';
import path from 'node:path';
const root = path.resolve(import.meta.dirname, '..');
process.chdir(root);
const image = 'postgres:18.6-alpine@sha256:8a334ab9502c8ccb8d7dec4e83b36b5ce8ed7b396e585f1c86b0e69c416f4fca';
// Resolve host tools only from conventional installation directories. Never search PATH.
const docker = ['/usr/local/bin/docker', '/opt/homebrew/bin/docker', '/usr/bin/docker'].find(existsSync);
function executable(command) {
    if (path.isAbsolute(command)) return command;
    const file = command === 'docker' ? docker : command === 'openssl' ? '/usr/bin/openssl' : command === 'go' ? ['/usr/local/go/bin/go', '/opt/homebrew/bin/go', '/usr/bin/go'].find(existsSync) : undefined;
    if (!file) throw new Error(`Required local tool is unavailable: ${command}`);
    return file;
}
const action = process.argv[2];
const testing = action?.startsWith('test-') || action === 'ci-test-init';
const isolated = process.env.COSMOFORMER_TEST_DIR;
if (testing && isolated && path.dirname(path.resolve(isolated)) !== path.join(root, '.build/test-runs')) throw new Error('Managed test directory must be inside .build/test-runs.');
const dir = testing && isolated ? path.resolve(isolated) : path.join(root, testing ? '.build/testdb' : '.build/dev');
mkdirSync(dir, { recursive: true, mode: 0o700 });
chmodSync(dir, 0o700);
function run(command, args, opts = {}) { const r = spawnSync(executable(command), args, { encoding: 'utf8', stdio: 'pipe', ...opts }); if (r.status !== 0)
    throw new Error(`${command} failed: ${r.stderr || 'check that the local tool is running'}`); return r.stdout.trim(); }
function secret(name, value) { const p = path.join(dir, name); writeFileSync(p, value, { mode: 0o600 }); chmodSync(p, 0o600); return p; }
function go(args, env = {}) { const local = path.join(root, '.build/tools/go/bin/go'); return run(existsSync(local) ? local : 'go', args, { env: { ...process.env, GOCACHE: path.join(root, '.build/go-cache'), GOPATH: path.join(root, '.build/gopath'), ...env } }); }
function container() { return existsSync(path.join(dir, 'container')) ? readFileSync(path.join(dir, 'container'), 'utf8').trim() : null; }
function wait(id, user, db) { for (let i = 0; i < 60; i++) {
    // The image's temporary initialization server accepts Unix sockets before
    // restarting. TCP readiness waits for the final server instead.
    const p = spawnSync(executable('docker'), ['exec', id, 'pg_isready', '-h', '127.0.0.1', '-U', user, '-d', db], { stdio: 'ignore' });
    if (p.status === 0)
        return;
    Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 500);
} throw new Error('Local database did not become ready.'); }
try {
    if (action === 'setup') {
        if (!existsSync(path.join(dir, 'db.env'))) {
            const admin = randomBytes(32).toString('hex'), owner = randomBytes(32).toString('hex'), app = randomBytes(32).toString('hex');
            secret('db.env', `POSTGRES_PASSWORD=${admin}\nPOSTGRES_DB=cosmoformer\n`);
            secret('init.sql', `CREATE ROLE cosmoformer_owner LOGIN PASSWORD '${owner}' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;\nCREATE ROLE cosmoformer_app LOGIN PASSWORD '${app}' NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;\nALTER DATABASE cosmoformer OWNER TO cosmoformer_owner;\nREVOKE CREATE ON SCHEMA public FROM PUBLIC;\n`);
            secret('owner.dsn', `postgres://cosmoformer_owner:${owner}@127.0.0.1:54329/cosmoformer?sslmode=disable`);
            secret('app.dsn', `postgres://cosmoformer_app:${app}@127.0.0.1:54329/cosmoformer?sslmode=disable`);
        }
        let id = container();
        if (!id) {
            id = run('docker', ['run', '-d', '--name', `cosmoformer-dev-${randomBytes(4).toString('hex')}`, '--label', 'cosmoformer.local=dev', '-p', '127.0.0.1:54329:5432', '--env-file', path.join(dir, 'db.env'), '--mount', `type=bind,src=${path.join(dir, 'init.sql')},dst=/docker-entrypoint-initdb.d/01-roles.sql,readonly`, '--mount', 'type=volume,dst=/var/lib/postgresql', image]);
            secret('container', id);
        }
        else
            run('docker', ['start', id]);
        wait(id, 'postgres', 'cosmoformer');
        if (!existsSync(path.join(dir, 'cert.pem')))
            run('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', path.join(dir, 'key.pem'), '-out', path.join(dir, 'cert.pem'), '-days', '365', '-subj', '/CN=localhost', '-addext', 'subjectAltName=DNS:localhost,IP:127.0.0.1']);
        chmodSync(path.join(dir, 'key.pem'), 0o600);
        console.log(go(['run', './cmd/cosmoformer', 'migrate'], { COSMOFORMER_MIGRATION_DSN_FILE: path.join(dir, 'owner.dsn') }));
        // No default password: the bootstrap command writes a one-use setup code to
        // an owner-readable file and refuses to reopen completed bootstrap.
        if (!existsSync(path.join(dir, 'bootstrap-done'))) {
            console.log(go(['run', './cmd/cosmoformer', 'bootstrap-admin', '--username', 'admin', '--display-name', 'Local administrator', '--setup-code-file', path.join(dir, 'admin-setup-code')], { COSMOFORMER_MIGRATION_DSN_FILE: path.join(dir, 'owner.dsn') }));
            secret('bootstrap-done', 'done');
        }
        console.log('Local setup ready. Run make dev-api and make dev-web in separate terminals.\nOpen https://localhost:5173/account/setup and use .build/dev/admin-setup-code once.');
    }
    else if (action === 'cert') {
        if (!existsSync(path.join(dir, 'cert.pem')))
            run('openssl', ['req', '-x509', '-newkey', 'rsa:2048', '-nodes', '-keyout', path.join(dir, 'key.pem'), '-out', path.join(dir, 'cert.pem'), '-days', '365', '-subj', '/CN=localhost', '-addext', 'subjectAltName=DNS:localhost,IP:127.0.0.1']);
        chmodSync(path.join(dir, 'key.pem'), 0o600);
        console.log('Local HTTPS certificate ready.');
    }
    else if (action === 'ci-test-init') {
        if (process.env.CI !== 'true' || !/^[a-f0-9]{12,64}$/.test(process.argv[3] ?? ''))
            throw new Error('CI test service ID is required.');
        const id = process.argv[3], marker = randomBytes(32).toString('hex');
        wait(id, 'postgres', 'cosmoformer_test_control');
        run('docker', ['exec', '-i', id, 'psql', '-U', 'postgres', '-d', 'cosmoformer_test_control', '-v', 'ON_ERROR_STOP=1'], { input: `CREATE TABLE test_marker(value text NOT NULL); INSERT INTO test_marker VALUES('${marker}'); CREATE ROLE cosmoformer_owner LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS; CREATE ROLE cosmoformer_app LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;` });
        secret('marker', marker);
        secret('owner.dsn', 'postgres://postgres@127.0.0.1:5432/cosmoformer_test_control?sslmode=disable');
        console.log('Marked ephemeral CI database ready.');
    }
    else if (action === 'stop') {
        const id = container();
        if (id)
            run('docker', ['stop', id]);
        console.log('Stopped the recorded development database; its data is retained.');
    }
    else if (action === 'test-start') {
        if (existsSync(path.join(dir, 'container-intent.json'))) throw new Error('Test database creation is already recorded; run test-stop for this directory before retrying.');
        const old = container();
        if (old) {
            run('docker', ['info', '--format', '{{.ID}}']);
            const inspect = spawnSync(executable('docker'), ['inspect', old], {encoding: 'utf8'});
            if (inspect.status === 0) throw new Error('Test database already recorded; use make test-db-stop first.');
            if (inspect.status !== 1 || !/No such (object|container)/i.test(inspect.stderr ?? '')) throw new Error('Cannot verify the previously recorded test database; retain its receipt for recovery.');
        }
        // A stale ID must not count as a received reply for this new creation.
        rmSync(path.join(dir, 'container'), {force: true});
        const marker = randomBytes(32).toString('hex');
        const intent = {name: `cosmoformer-test-${randomBytes(16).toString('hex')}`, owner: randomBytes(32).toString('hex')};
        // Persist identity before Docker can create anything, including when
        // cancellation or response loss prevents receiving its container ID.
        secret('container-intent.json', JSON.stringify(intent));
        const id = run('docker', ['run', '--rm', '-d', '--name', intent.name, '--label', 'cosmoformer.local=test', '--label', `cosmoformer.test-owner=${intent.owner}`, '-p', '127.0.0.1::5432', '-e', 'POSTGRES_HOST_AUTH_METHOD=trust', '-e', 'POSTGRES_DB=cosmoformer_test_control', image]);
        secret('container', id);
        wait(id, 'postgres', 'cosmoformer_test_control');
        run('docker', ['exec', '-i', id, 'psql', '-U', 'postgres', '-d', 'cosmoformer_test_control', '-v', 'ON_ERROR_STOP=1'], { input: `CREATE TABLE test_marker(value text NOT NULL); INSERT INTO test_marker VALUES('${marker}'); CREATE ROLE cosmoformer_owner LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS; CREATE ROLE cosmoformer_app LOGIN NOINHERIT NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;` });
        const port = run('docker', ['port', id, '5432/tcp']).split(':').at(-1);
        secret('marker', marker);
        secret('owner.dsn', `postgres://postgres@127.0.0.1:${port}/cosmoformer_test_control?sslmode=disable`);
        console.log('Isolated test database ready.');
    }
    else if (action === 'test-stop') {
        const intentFile = path.join(dir, 'container-intent.json');
        if (existsSync(intentFile)) {
            const intent = JSON.parse(readFileSync(intentFile, 'utf8'));
            if (!/^cosmoformer-test-[a-f0-9]{32}$/.test(intent.name) || !/^[a-f0-9]{64}$/.test(intent.owner)) throw new Error('Invalid test database creation identity; retain it for recovery.');
            run('docker', ['info', '--format', '{{.ID}}']);
            const inspect = spawnSync(executable('docker'), ['inspect', '--format', '{{index .Config.Labels "cosmoformer.test-owner"}}', intent.name], {encoding: 'utf8'});
            if (inspect.status !== 0) {
                if (!container()) throw new Error('Test database creation outcome is unresolved; retain this directory and retry cleanup after Docker settles.');
                // A successfully received ID plus absence means --rm already
                // removed the container; a missing creation reply proves less.
                if (inspect.status !== 1 || !/No such (object|container)/i.test(inspect.stderr ?? '')) throw new Error('Cannot verify test database cleanup.');
            } else {
                if (inspect.stdout.trim() !== intent.owner) throw new Error('Refusing to stop a test container owned by another invocation.');
                run('docker', ['stop', intent.name]);
            }
            rmSync(path.join(dir, 'container'), {force: true}); rmSync(intentFile);
            console.log('Stopped the owned throwaway test database.');
        } else {
            const id = container();
            if (id) {
                const label = run('docker', ['inspect', '--format', '{{index .Config.Labels "cosmoformer.local"}}', id]);
                if (label !== 'test')
                    throw new Error('Refusing to stop an unmarked container.');
                run('docker', ['stop', id]);
                rmSync(path.join(dir, 'container'));
            }
            console.log('Stopped the recorded throwaway test database.');
        }
    }
    else
        throw new Error('Use setup, stop, test-start or test-stop.');
}
catch (e) {
    console.error(e.message);
    process.exitCode = 1;
}
