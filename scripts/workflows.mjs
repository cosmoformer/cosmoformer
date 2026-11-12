import {existsSync, readFileSync, readdirSync, mkdirSync, writeFileSync, rmSync, mkdtempSync, watch} from 'node:fs';
import {createHash} from 'node:crypto';
import path from 'node:path';
import {root, go, docker, dev, child, available} from './workflow-lib.mjs';

const active = new Set();
let interrupted = false;
let receivedSignal = false;
function launch(command, args, options) {
    const item = child(command, args, options); active.add(item);
    // The leader can exit before its descendants. Retain group ownership until
    // stop() has verified the whole group, including an in-flight signal stop.
    item.done.then(async () => {
        try { await item.stop(); active.delete(item); }
        catch (error) { console.error(error.message); process.exitCode = 1; requestStop?.(); }
    });
    return item;
}
async function run(command, args, options, cleanup = false) {
    if (interrupted && !cleanup) throw Error('Interrupted.');
    const {code} = await launch(command, args, options).done;
    if (code) throw Error('Development command failed; see the output above.');
}
const node = process.execPath;
let requestStop;
function signal() {
    receivedSignal = true; interrupted = true;
    requestStop?.();
    for (const item of active) void item.stop().catch(error => console.error(error.message));
}
process.on('SIGINT', signal); process.on('SIGTERM', signal);
async function running(file, storage = false) {
    if (!existsSync(file)) return false;
    const record = readFileSync(file, 'utf8');
    const id = storage ? JSON.parse(record).container : record.trim();
    const result = await child(docker, ['inspect', '--format', '{{.State.Running}}', id], {quiet: true}).done;
    return result.code === 0 && result.output.trim() === 'true';
}
function sourceFingerprint() {
    const digest = createHash('sha256');
    function scan(dir) {
        for (const entry of readdirSync(path.join(root, dir), {withFileTypes: true}).sort((a, b) => a.name.localeCompare(b.name))) {
            const file = path.join(dir, entry.name);
            if (entry.isDirectory()) scan(file);
            else if (entry.isFile() && /\.(go|sql)$/.test(entry.name)) { digest.update(file); digest.update(readFileSync(path.join(root, file))); }
        }
    }
    scan('cmd/cosmoformer'); scan('internal');
    for (const file of ['go.mod', 'go.sum']) digest.update(readFileSync(path.join(root, file)));
    return digest.digest('hex');
}

async function development() {
    mkdirSync(dev, {recursive: true, mode: 0o700});
    const lock = path.join(dev, 'session.lock');
    try { mkdirSync(lock, {mode: 0o700}); } catch { throw Error('A development session is already recorded. Run make doctor before removing a stale .build/dev/session.lock.'); }
    writeFileSync(path.join(lock, 'pid'), `${process.pid}`, {mode: 0o600});
    let wasDB = true, wasS3 = true, started = false, reloading = false, api, web, watcherTimer;
    const watchers = [];
    let stop;
    const stopped = new Promise(resolve => { stop = resolve; });
    requestStop = stop;
    const env = {...process.env, GOCACHE: path.join(root, '.build/go-cache'), GOPATH: path.join(root, '.build/gopath'), COSMOFORMER_DATABASE_DSN_FILE: path.join(dev, 'app.dsn'), COSMOFORMER_ORIGIN: 'https://localhost:5173', COSMOFORMER_LISTEN: '', COSMOFORMER_UNIX_SOCKET: path.join(dev, 'api.sock')};
    delete env.COSMOFORMER_DEV_API;
    // Host AWS settings are unnecessary for the API/frontend development session.
    for (const key of Object.keys(env)) if (key.startsWith('AWS_') || key === 'COSMOFORMER_MIGRATION_DSN_FILE') delete env[key];
    try {
        if (!await available(5173)) throw Error('Port 5173 is busy. Stop its server before make dev.');
        if (existsSync(path.join(dev, 'api.sock'))) throw Error('API socket is already present; stop its server or verify it is stale before removing it.');
        wasDB = await running(path.join(dev, 'container'));
        wasS3 = await running(path.join(root, '.build/local/dev/environment.json'), true);
        started = true;
        await run('make', ['dev-setup'], {label: 'setup'});
        let dirty = true, fingerprint = '';
        let rebuilding;
        const rebuild = () => {
            dirty = true;
            if (rebuilding) return rebuilding;
            if (interrupted) return Promise.resolve();
            rebuilding = (async () => {
                while (dirty && !interrupted) {
                    dirty = false;
                    const next = sourceFingerprint();
                    if (next === fingerprint) continue;
                    fingerprint = next; reloading = true;
                    if (api) await api.stop();
                    let result = await launch(go, ['build', '-o', path.join(dev, 'api'), './cmd/cosmoformer'], {env, label: 'build'}).done;
                    if (!result.code && !interrupted) result = await launch('make', ['contract'], {env, label: 'contract'}).done;
                    if (!result.code && !interrupted) {
                        api = launch(path.join(dev, 'api'), ['serve'], {env, label: 'api'});
                        const current = api;
                        api.done.then(() => { if (api === current && !reloading && !interrupted) { process.exitCode = 1; stop(); } });
                    } else if (!interrupted) console.log('API build failed; fix the source to retry automatically.');
                    reloading = false;
                }
            })().finally(() => { rebuilding = undefined; });
            return rebuilding;
        };
        for (const dir of ['cmd/cosmoformer', 'internal']) watchers.push(watch(path.join(root, dir), {recursive: true}, (_, name) => {
            if (!name || !/\.(go|sql)$/.test(name)) return;
            clearTimeout(watcherTimer); watcherTimer = setTimeout(() => { void rebuild().catch(() => { process.exitCode = 1; stop(); }); }, 200);
        }));
        for (const file of ['go.mod', 'go.sum']) watchers.push(watch(path.join(root, file), () => { void rebuild()?.catch(() => { process.exitCode = 1; stop(); }); }));
        await rebuild();
        if (!api && !interrupted) throw Error('Initial API build failed.');
        if (interrupted) return;
        // Rebuild already generated the contract; standalone dev-web retains
        // its prerequisite for callers using separate terminals.
        web = launch('npm', ['--prefix', 'web', 'run', 'dev'], {env, label: 'web'});
        web.done.then(() => { if (!interrupted) { process.exitCode = 1; stop(); } });
        console.log('Development session: https://localhost:5173. Ctrl-C stops its servers and dependencies started by this session.');
        await stopped;
        if (rebuilding) await rebuilding;
    } finally {
        interrupted = true;
        clearTimeout(watcherTimer); watchers.forEach(w => w.close());
        await Promise.all([...active].map(item => item.stop()));
        if (started) {
            if (!wasS3) await run(node, ['scripts/local-env.mjs', 'stop'], {label: 'stop'}, true).catch(() => { process.exitCode = 1; });
            if (!wasDB) await run(node, ['scripts/dev.mjs', 'stop'], {label: 'stop'}, true).catch(() => { process.exitCode = 1; });
        }
        rmSync(lock, {recursive: true, force: true});
    }
}

async function integration() {
    const base = path.join(root, '.build/test-runs'); mkdirSync(base, {recursive: true, mode: 0o700});
    const dir = mkdtempSync(path.join(base, 'run-'));
    const env = {...process.env, COSMOFORMER_TEST_DIR: dir};
    delete env.COSMOFORMER_DATABASE_DSN_FILE; delete env.COSMOFORMER_MIGRATION_DSN_FILE;
    // Recursive Make command-line overrides outrank environment values, even
    // across the Node process. Only the explicitly forwarded TEST_ARGS belongs
    // to the nested invocation; its database context must come from this run.
    for (const key of ['MAKEFLAGS', 'MAKEOVERRIDES', 'MFLAGS', 'GNUMAKEFLAGS']) delete env[key];
    try {
        await run(node, ['scripts/dev.mjs', 'test-start'], {env, label: 'test-db'});
        env.COSMOFORMER_TEST_OWNER_DSN = readFileSync(path.join(dir, 'owner.dsn'), 'utf8');
        env.COSMOFORMER_TEST_MARKER_FILE = path.join(dir, 'marker');
        env.COSMOFORMER_TEST_PG_CONTAINER = readFileSync(path.join(dir, 'container'), 'utf8').trim();
        if (!env.COSMOFORMER_TEST_PG_CONTAINER) throw Error('Owned test database container metadata is empty.');
        await run('make', ['test-integration', `TEST_ARGS=${process.env.TEST_ARGS ?? ''}`], {env});
    } finally {
        // Startup may fail after recording the container; still attempt cleanup.
        if (existsSync(path.join(dir, 'container')) || existsSync(path.join(dir, 'container-intent.json'))) await run(node, ['scripts/dev.mjs', 'test-stop'], {env, label: 'cleanup'}, true);
        rmSync(dir, {recursive: true, force: true});
    }
}
try {
    if (process.argv[2] === 'dev') await development();
    else if (process.argv[2] === 'test') await integration();
    else throw Error('Use dev or test.');
} catch (error) { console.error(error.message); process.exitCode = receivedSignal ? 130 : 1; }
