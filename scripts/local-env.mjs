import {mkdirSync, readFileSync, writeFileSync, existsSync, chmodSync, copyFileSync, mkdtempSync, rmSync, renameSync} from 'node:fs';
import {randomBytes, createHash, createHmac} from 'node:crypto';
import {spawnSync} from 'node:child_process';
import path from 'node:path';
import assert from 'node:assert/strict';

const root = path.resolve(import.meta.dirname, '..');
const image = 'chrislusf/seaweedfs:4.47@sha256:ce9e796f1fe6f06968f4c04bdaf8f678dad9c8acdfef3d244133d71bfa6bf882';
const runner = 'cosmoformer-local-tofu:1.13.8';
const docker = ['/usr/local/bin/docker', '/opt/homebrew/bin/docker', '/usr/bin/docker'].find(existsSync);
const action = process.argv[2];
const base = path.join(root, '.build/local');
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));
function run(args, allowed = [0], interactive = false) {
    if (!docker) throw Error('Docker is required.');
    const r = spawnSync(docker, args, {encoding: 'utf8', stdio: interactive ? 'inherit' : 'pipe', timeout: 240000, maxBuffer: 8 * 1024 * 1024});
    if (r.error || !allowed.includes(r.status)) throw Error(`Local Docker command failed: ${r.error?.message ?? r.stderr}`);
    return r;
}
function save(file, value) {
    writeFileSync(file, value, {mode: 0o600});
    chmodSync(file, 0o600);
}
function load(dir) { return JSON.parse(readFileSync(path.join(dir, 'environment.json'), 'utf8')); }
function saveRecord(file, env) {
    const temporary = `${file}.${randomBytes(8).toString('hex')}.tmp`;
    try { save(temporary, JSON.stringify(env)); renameSync(temporary, file); }
    finally { rmSync(temporary, {force: true}); }
}
function owned(kind, name, id, execute = run) {
    const template = kind === 'container' ? '{{index .Config.Labels "cosmoformer.local-id"}}' : '{{index .Labels "cosmoformer.local-id"}}';
    const actual = execute([kind, 'inspect', '--format', template, name], [0, 1]);
    if (actual.status !== 0 || actual.stdout.trim() !== id) throw Error('Refusing to manage an unowned local resource.');
}
const hash = value => createHash('sha256').update(value).digest('hex');
const hmac = (key, value) => createHmac('sha256', key).update(value).digest();
// Small dependency-free SigV4 client, restricted to the recorded loopback S3 port.
async function s3(env, method, pathname, body = '', query = '', extra = {}) {
    const date = new Date().toISOString().replace(/[:-]|\.\d{3}/g, '');
    const day = date.slice(0, 8), host = `127.0.0.1:${env.port}`;
    const headers = {host, 'x-amz-content-sha256': hash(body), 'x-amz-date': date, ...extra};
    // SigV4 requires byte ordering, independent of the host's locale.
    const names = Object.keys(headers).sort((a, b) => Buffer.compare(Buffer.from(a), Buffer.from(b)));
    const signed = names.join(';');
    const canonical = [method, pathname, query, names.map(k => `${k}:${headers[k]}\n`).join(''), signed, hash(body)].join('\n');
    const scope = `${day}/us-east-1/s3/aws4_request`;
    const key = hmac(hmac(hmac(hmac(`AWS4${env.secret}`, day), 'us-east-1'), 's3'), 'aws4_request');
    const signature = hmac(key, `AWS4-HMAC-SHA256\n${date}\n${scope}\n${hash(canonical)}`).toString('hex');
    headers.authorization = `AWS4-HMAC-SHA256 Credential=${env.access}/${scope}, SignedHeaders=${signed}, Signature=${signature}`;
    const res = await fetch(`http://${host}${pathname}${query ? `?${query}` : ''}`, {
        method, headers, body: ['GET', 'HEAD'].includes(method) ? undefined : body,
        signal: AbortSignal.timeout(10000), redirect: 'error'
    });
    return {status: res.status, text: await res.text()};
}
async function expectS3(env, method, key, body = '', query = '', statuses = [200], extra = {}, request = s3) {
    const r = await request(env, method, key, body, query, extra);
    assert.ok(statuses.includes(r.status), `Local S3 ${method} ${key}: HTTP ${r.status} ${r.text.match(/<Code>([^<]+)<\/Code>/)?.[1] ?? ''}`);
    return r.text;
}
function prepare(dir) {
    mkdirSync(path.join(dir, 'work'), {recursive: true, mode: 0o700});
    chmodSync(dir, 0o700);
    copyFileSync(path.join(root, 'tests/local/main.tf'), path.join(dir, 'work/main.tf'));
    save(path.join(dir, 'tofurc'), 'disable_checkpoint = true\nprovider_installation {\n filesystem_mirror { path = "/fixtures/provider-mirror" }\n}\n');
}
export async function start(dir, test, {execute = run, request = s3, pause = sleep} = {}) {
    prepare(dir);
    const record = path.join(dir, 'environment.json');
    let env;
    if (existsSync(record)) {
        env = load(dir);
    } else {
        const id = randomBytes(8).toString('hex');
        env = {id, phase: 'initializing', network: `cosmoformer-local-${id}`, gateway: `cosmoformer-local-access-${id}`, container: `cosmoformer-seaweed-${id}`, volume: `cosmoformer-s3-${id}`, access: randomBytes(16).toString('hex'), secret: randomBytes(32).toString('hex')};
        saveRecord(record, env); // Retain ownership even if startup fails.
    }
    if (env.phase !== undefined && !['initializing', 'started', 'ready'].includes(env.phase)) throw Error('Unknown local storage initialization phase. Explicit recovery is required.');
    env.configVolume ??= `cosmoformer-config-${env.id}`;
    saveRecord(record, env);
    // Reuse recorded credentials if an earlier attempt stopped between writes.
    save(path.join(dir, 's3.json'), JSON.stringify({identities: [{name: 'local', credentials: [{accessKey: env.access, secretKey: env.secret}], actions: ['Admin', 'Read', 'Write', 'List', 'Tagging']}]}));
    save(path.join(dir, 'filer.toml'), '[leveldb2]\nenabled = true\ndir = "/data/filerldb2"\n');
    save(path.join(dir, 'runner.env'), `AWS_ACCESS_KEY_ID=${env.access}\nAWS_SECRET_ACCESS_KEY=${env.secret}\nAWS_EC2_METADATA_DISABLED=true\nAWS_CONFIG_FILE=/dev/null\nAWS_SHARED_CREDENTIALS_FILE=/dev/null\nTF_IN_AUTOMATION=1\nCHECKPOINT_DISABLE=1\nHOME=/tmp\nTF_CLI_CONFIG_FILE=/local/tofurc\n`);
    save(path.join(dir, 'master.toml'), '[master.volume_growth]\ncopy_1 = 1\n');
    execute(['info', '--format', '{{.ID}}']);
    // Validate the volume before recreating anything: Docker otherwise silently
    // creates an empty named volume when creating a replacement container.
    for (const [kind, name] of [['volume', env.volume], ['volume', env.configVolume], ['network', env.network], ['network', env.gateway]]) {
        if (execute([kind, 'inspect', name], [0, 1]).status === 0) owned(kind, name, env.id, execute);
        else {
            if (kind === 'volume' && name === env.volume && env.phase !== 'initializing') throw Error('Recorded local data volume is missing. Explicit recovery is required; no empty replacement was created.');
            execute([kind, 'create', ...(kind === 'network' && name === env.network ? ['--internal'] : []), '--label', `cosmoformer.local-id=${env.id}`, name]);
            owned(kind, name, env.id, execute);
        }
    }
    // The image drops to its seaweed UID. Linux bind mounts preserve the host
    // owner, so keep host files private and stage private container-owned copies.
    execute(['run', '--rm', '--network', 'none', '--user', '0:0', '--label', `cosmoformer.local-id=${env.id}`, '--label', `cosmoformer.local-run=config-${randomBytes(16).toString('hex')}`, '--mount', `type=volume,src=${env.configVolume},dst=/config`, ...['s3.json', 'filer.toml', 'master.toml'].flatMap(file => ['--mount', `type=bind,src=${path.join(dir, file)},dst=/inputs/${file},readonly`]), '--entrypoint', '/bin/sh', image, '-ec', 'cp /inputs/s3.json /inputs/filer.toml /inputs/master.toml /config/; chown -R seaweed:seaweed /config; chmod 700 /config; chmod 600 /config/*']);
    let present = execute(['container', 'inspect', env.container], [0, 1]).status === 0;
    if (present) {
        owned('container', env.container, env.id, execute);
        const mounts = JSON.parse(execute(['container', 'inspect', '--format', '{{json .Mounts}}', env.container]).stdout);
        if (!mounts.some(mount => mount.Type === 'volume' && mount.Name === env.configVolume && mount.Destination === '/etc/seaweedfs')) {
            // Upgrade old bind-mounted configurations, retaining the data volume.
            env.phase = 'started'; saveRecord(record, env);
            execute(['stop', env.container]); execute(['rm', env.container]); present = false;
        }
    }
    if (!present) execute(['create', '--name', env.container, '--label', `cosmoformer.local-id=${env.id}`, '--network', env.gateway, '--network-alias', 'seaweed', '-p', test ? '127.0.0.1::8333' : '127.0.0.1:18333:8333', '--mount', `type=volume,src=${env.volume},dst=/data`, '--mount', `type=volume,src=${env.configVolume},dst=/etc/seaweedfs,readonly`, image, 'server', '-ip=seaweed', '-ip.bind=0.0.0.0', '-dir=/data', '-s3', '-s3.config=/etc/seaweedfs/s3.json', '-s3.autoCreateBucket=false', '-s3.allowDeleteBucketNotEmpty=false', '-s3.port.iceberg=0', '-s3.port.lance=0', '-master.telemetry=false', '-volume.preStopSeconds=1', '-volume.max=32', '-master.volumeSizeLimitMB=100']);
    const attached = JSON.parse(execute(['container', 'inspect', '--format', '{{json .NetworkSettings.Networks}}', env.container]).stdout);
    for (const network of [env.gateway, env.network]) if (!Object.hasOwn(attached, network)) execute(['network', 'connect', '--alias', 'seaweed', network, env.container]);
    // Commit start intent before the server can write data. Even response loss
    // must never authorize replacing a potentially used volume on the next run.
    if (env.phase !== 'ready') env.phase = 'started';
    saveRecord(record, env);
    execute(['start', env.container]);
    env.port = Number(execute(['port', env.container, '8333/tcp']).stdout.trim().split(':').at(-1));
    saveRecord(record, env);
    let ready = false;
    for (let i = 0; i < 60; i++) {
        try { if ((await request(env, 'GET', '/')).status === 200) { ready = true; break; } } catch {}
        await pause(500);
    }
    if (!ready) throw Error('Local SeaweedFS did not become ready. Inspect its recorded container.');
    for (const bucket of ['state', 'artifacts', 'resources']) {
        await expectS3(env, 'PUT', `/cosmoformer-${bucket}`, '', '', [200, 409], {}, request);
    }
    await expectS3(env, 'PUT', '/cosmoformer-state', '<VersioningConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Status>Enabled</Status></VersioningConfiguration>', 'versioning=', [200], {}, request);
    // Listing buckets can succeed before volume registration after a restart.
    // Verify the data path as well as the gateway before starting native work.
    const probe = '/cosmoformer-resources/.ready';
    let writable = false;
    for (let i = 0; i < 60; i++) {
        try {
            if ((await request(env, 'PUT', probe, env.id)).status === 200 && (await request(env, 'GET', probe)).text === env.id) { writable = true; break; }
        } catch {}
        await pause(500);
    }
    if (!writable) throw Error('Local S3 data path did not become ready.');
    await expectS3(env, 'DELETE', probe, '', '', [204], {}, request);
    env.phase = 'ready'; saveRecord(record, env);
    return env;
}
export function tofu(dir, env, args, allowed = [0], interactive = false, execute = run) {
    const invocation = randomBytes(16).toString('hex');
    const name = `${env.container}-tofu-${invocation}`;
    let result;
    try { result = execute(['run', '--rm', '--name', name, '--label', `cosmoformer.local-id=${env.id}`, '--label', `cosmoformer.local-run=${invocation}`, '--platform', 'linux/amd64', '--network', env.network, '--read-only', '--cap-drop=ALL', '--security-opt=no-new-privileges', '--user', `${process.getuid()}:${process.getgid()}`, '--tmpfs', '/tmp:rw,exec,nosuid,nodev,size=1g', '--env-file', path.join(dir, 'runner.env'), '--mount', `type=bind,src=${path.join(dir, 'work')},dst=/work`, '--mount', `type=bind,src=${path.join(dir, 'tofurc')},dst=/local/tofurc,readonly`, ...(interactive ? ['-i'] : []), '-w', '/work', runner, ...args], allowed, interactive);
    } catch (error) {
        // A timed-out Docker client can leave its child container alive.
        const owner = execute(['container', 'inspect', '--format', '{{index .Config.Labels "cosmoformer.local-run"}}', name], [0, 1]);
        if (owner.status === 0 && owner.stdout.trim() === invocation) {
            owned('container', name, env.id, execute);
            execute(['rm', '-f', name]);
        }
        throw error;
    }
    return {code: result.status, output: (result.stdout ?? '') + (result.stderr ?? '')};
}
export function cleanup(dir, execute = run) {
    const env = load(dir);
    const runners = execute(['ps', '-aq', '--filter', `label=cosmoformer.local-id=${env.id}`, '--filter', 'label=cosmoformer.local-run']).stdout.trim().split(/\s+/).filter(Boolean);
    for (const [kind, name] of [...runners.map(name => ['container', name]), ['container', env.container], ['volume', env.volume], ...(env.configVolume ? [['volume', env.configVolume]] : []), ['network', env.network], ['network', env.gateway]]) {
        if (execute([kind, 'inspect', name], [0, 1]).status !== 0) continue;
        owned(kind, name, env.id, execute);
        execute(kind === 'container' ? ['rm', '-f', name] : [kind, 'rm', name]);
    }
    rmSync(dir, {recursive: true, force: true});
}
async function integration(dir) {
    // Exercise a real partially provisioned environment: container and volume
    // exist, but attachment failed before any storage process could start.
    let failAttachment = true;
    await assert.rejects(start(dir, true, {execute(args, ...options) {
        if (failAttachment && args[0] === 'network' && args[1] === 'connect') {
            failAttachment = false; throw Error('injected local network attachment failure');
        }
        return run(args, ...options);
    }}), /injected local network attachment failure/);
    const env = await start(dir, true);
    console.log('Local storage recovered from interrupted setup.');
    const anonymous = await fetch(`http://127.0.0.1:${env.port}/`, {signal: AbortSignal.timeout(10000)});
    assert.equal(anonymous.status, 403);
    const tf = (command, ...args) => {
        const result = tofu(dir, env, [command, '-no-color', '-input=false', ...args], command === 'plan' ? [0, 1, 2] : [0]);
        if (result.code === 1 && !args.includes('-lock-timeout=1s')) throw Error(result.output);
        return result;
    };
    tf('init');
    assert.equal(tf('plan', '-detailed-exitcode', '-out=create.plan').code, 2);
    tf('apply', 'create.plan');
    console.log('Local saved-plan apply completed.');
    assert.equal(await expectS3(env, 'GET', '/cosmoformer-resources/example.txt'), 'hello from local OpenTofu');
    assert.equal(tf('plan', '-detailed-exitcode').code, 0);
    const state = JSON.parse(await expectS3(env, 'GET', '/cosmoformer-state/local/terraform.tfstate'));
    tf('apply', '-auto-approve', '-var=content=updated locally');
    assert.equal(await expectS3(env, 'GET', '/cosmoformer-resources/example.txt'), 'updated locally');
    const newer = JSON.parse(await expectS3(env, 'GET', '/cosmoformer-state/local/terraform.tfstate'));
    assert.equal(newer.lineage, state.lineage);
    assert.ok(newer.serial > state.serial);
    run(['stop', env.container]);
    Object.assign(env, await start(dir, true));
    console.log('Local storage restarted.');
    assert.equal(await expectS3(env, 'GET', '/cosmoformer-resources/example.txt'), 'updated locally');
    assert.equal(tf('plan', '-detailed-exitcode', '-var=content=updated locally').code, 0);
    await expectS3(env, 'PUT', '/cosmoformer-resources/example.txt', 'external local drift');
    assert.equal(tf('plan', '-detailed-exitcode', '-var=content=updated locally', '-out=repair.plan').code, 2);
    tf('apply', 'repair.plan');
    console.log('Local drift repair completed.');
    assert.equal(await expectS3(env, 'GET', '/cosmoformer-resources/example.txt'), 'updated locally');
    const lockPath = '/cosmoformer-state/local/terraform.tfstate.tflock';
    const lock = JSON.stringify({ID: randomBytes(16).toString('hex'), Operation: 'OperationTypeApply', Info: 'local contention test', Who: 'fixture', Version: '1.13.8', Created: new Date().toISOString(), Path: 'cosmoformer-state/local/terraform.tfstate'});
    const claims = await Promise.all([s3(env, 'PUT', lockPath, lock, '', {'if-none-match': '*'}), s3(env, 'PUT', lockPath, lock, '', {'if-none-match': '*'})]);
    assert.deepEqual(claims.map(r => r.status).sort((a, b) => a - b), [200, 412]);
    const blocked = tf('plan', '-lock-timeout=1s');
    assert.equal(blocked.code, 1);
    assert.match(blocked.output, /Error acquiring the state lock/);
    assert.equal(await expectS3(env, 'GET', lockPath), lock);
    await expectS3(env, 'DELETE', lockPath, '', '', [204]);
    assert.equal(tf('plan', '-detailed-exitcode', '-var=content=updated locally').code, 0);
    await expectS3(env, 'PUT', '/cosmoformer-artifacts/fixture.txt', 'local artifact');
    assert.equal(await expectS3(env, 'GET', '/cosmoformer-artifacts/fixture.txt'), 'local artifact');
    tf('destroy', '-auto-approve');
    await expectS3(env, 'GET', '/cosmoformer-resources/example.txt', '', '', [404]);
    const empty = JSON.parse(await expectS3(env, 'GET', '/cosmoformer-state/local/terraform.tfstate'));
    assert.equal(empty.resources.length, 0);
    // This environment is disposable. Losing its initialized volume must not
    // silently create an empty replacement on a subsequent setup attempt.
    run(['rm', '-f', env.container]);
    run(['volume', 'rm', env.volume]);
    await assert.rejects(start(dir, true), /data volume is missing.*Explicit recovery/);
    assert.equal(run(['volume', 'inspect', env.volume], [0, 1]).status, 1);
    console.log('Local S3 integration passed: startup recovery, saved plan, no-op, update, drift repair, state serial/lineage, conditional locking, contention, artifacts, destroy and missing-volume protection.');
}
if (import.meta.main) {
mkdirSync(base, {recursive: true, mode: 0o700});
try {
    const dev = path.join(base, 'dev');
    if (action === 'up') {
        const env = await start(dev, false);
        tofu(dev, env, ['init', '-input=false', '-no-color']);
        console.log(`Local S3 ready at http://127.0.0.1:${env.port}. Credentials and work directory: .build/local/dev/.`);
    } else if (action === 'status') {
        const env = load(dev); owned('container', env.container, env.id);
        for (const bucket of ['state', 'artifacts', 'resources']) await expectS3(env, 'HEAD', `/cosmoformer-${bucket}`);
        assert.match(await expectS3(env, 'GET', '/cosmoformer-state', '', 'versioning='), /<Status>Enabled<\/Status>/);
        console.log('Local S3 buckets and state versioning are ready.');
    } else if (action === 'stop') {
        if (existsSync(path.join(dev, 'environment.json'))) { const env = load(dev); owned('container', env.container, env.id); run(['stop', env.container]); }
        console.log('Local S3 stopped; development data retained.');
    } else if (action === 'tofu') {
        const env = load(dev); owned('container', env.container, env.id);
        const args = process.argv.slice(3);
        if (!args.length) throw Error('Supply OpenTofu arguments.');
        const r = tofu(dev, env, args, [0, 2], true); process.exitCode = r.code;
    } else if (action === 'test') {
        const dir = mkdtempSync(path.join(base, 'test-'));
        try { await integration(dir); } finally { if (existsSync(path.join(dir, 'environment.json'))) cleanup(dir); else rmSync(dir, {recursive: true, force: true}); }
    } else throw Error('Use up, stop, tofu or test.');
} catch (error) { console.error(error.message); process.exitCode = 1; }
}
