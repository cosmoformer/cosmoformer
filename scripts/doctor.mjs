import {existsSync, readFileSync} from 'node:fs';
import {X509Certificate, createPrivateKey} from 'node:crypto';
import path from 'node:path';
import {root, go, docker, dev, child, available, privateFile} from './workflow-lib.mjs';
import {apiSocket, localRequest} from './local-api.mjs';

let failures = 0;
async function check(name, fix, fn) {
    try { if (!await fn()) throw Error(); console.log(`OK    ${name}`); }
    catch { failures++; console.log(`FIX   ${name}: ${fix}`); }
}
async function capture(command, args) {
    if (!command) throw Error();
    const result = await child(command, args, {quiet: true}).done;
    if (result.code) throw Error(); return result.output.trim();
}
await check('Node 26.13.0', 'Run make tools and use the repository Node installation.', () => process.version === 'v26.13.0');
await check('Go 1.27.3', 'Run make tools.', async () => /go1\.27\.3\b/.test(await capture(go, ['version'])));
await check('Make and OpenSSL', 'Install Make and OpenSSL.', async () => { await capture('make', ['--version']); await capture('/usr/bin/openssl', ['version']); return true; });
await check('Docker daemon', 'Start Docker Desktop.', async () => Boolean(await capture(docker, ['info', '--format', '{{.ServerVersion}}'])));
await check('Frontend dependencies', 'Run make deps.', () => existsSync(path.join(root, 'web/node_modules/.package-lock.json')));
await check('Development session lock', 'If its recorded process has exited, remove .build/dev/session.lock; otherwise use that running session.', () => {
    const file = path.join(dev, 'session.lock/pid');
    if (!existsSync(path.dirname(file))) return true;
    const pid = Number(privateFile(file)); if (!Number.isInteger(pid) || pid < 2) return false;
    try { process.kill(pid, 0); console.log(`INFO  Development session PID ${pid} is running.`); return true; } catch { return false; }
});
await check('Development certificate and key', 'Run make dev-setup; remove expired cert.pem/key.pem first if renewal is needed.', () => {
    const cert = new X509Certificate(readFileSync(path.join(dev, 'cert.pem')));
    const key = createPrivateKey(privateFile(path.join(dev, 'key.pem')));
    return cert.checkHost('localhost') && cert.checkPrivateKey(key) && Date.parse(cert.validTo) > Date.now() && Date.parse(cert.validFrom) <= Date.now();
});
await check('Database and schema', 'Run make dev-setup. Existing databases need the explicit migration step supplied by setup.', async () => {
    const id = privateFile(path.join(dev, 'container')).trim();
    if (await capture(docker, ['inspect', '--format', '{{index .Config.Labels "cosmoformer.local"}}', id]) !== 'dev') return false;
    const expected = readFileSync(path.join(root, 'internal/store/store.go'), 'utf8').match(/const SchemaVersion int32 = (\d+)/)?.[1];
    const version = await capture(docker, ['exec', id, 'psql', '-U', 'postgres', '-d', 'cosmoformer', '-tAc', 'SELECT version FROM cosmoformer.schema_version']);
    return Boolean(expected) && version === expected && (await capture(docker, ['port', id, '5432/tcp'])) === '127.0.0.1:54329';
});
await check('Local S3 readiness', 'Run make dev-storage; inspect the recorded container if it fails.', async () => {
    const result = await child(process.execPath, ['scripts/local-env.mjs', 'status'], {quiet: true}).done; return result.code === 0;
});
await check('Private API socket', 'Stop the conflicting server or verify a stale socket before removing .build/dev/api.sock.', async () => !existsSync(apiSocket) || (await localRequest(apiSocket, 'GET', '/health/ready')).status === 200);
await check('Frontend port 5173', 'Stop the conflicting server before make dev.', async () => {
    if (await available(5173)) return true;
    // A running supervised frontend is expected; independent occupied ports are reported.
    return existsSync(path.join(dev, 'session.lock/pid'));
});
console.log(failures ? `${failures} check(s) need attention.` : 'Development checks passed.');
process.exitCode = failures ? 1 : 0;
