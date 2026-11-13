import {test} from 'node:test';
import assert from 'node:assert/strict';
import {mkdtempSync, mkdirSync, readFileSync, writeFileSync, chmodSync, rmSync, existsSync, readdirSync, copyFileSync, realpathSync} from 'node:fs';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {spawn, spawnSync} from 'node:child_process';

const root = path.resolve(import.meta.dirname, '..');
const pause = () => new Promise(resolve => setTimeout(resolve, 20));
function processGroups(groups) {
    const result = spawnSync('/bin/ps', ['-axo', 'pid=,pgid=,uid=,state='], {encoding: 'utf8', timeout: 1000});
    assert.equal(result.status, 0, 'must be able to inspect fixture processes');
    return result.stdout.trim().split('\n').map(line => line.trim().split(/\s+/))
        .filter(([, group, uid, state]) => groups.has(Number(group)) && Number(uid) === process.getuid() && !state.startsWith('Z'))
        .map(([pid, group]) => ({pid: Number(pid), group: Number(group)}));
}

// Exercise the shipped runner, process supervision and Make recipe. Only the
// database provisioner and Go executable are fixtures; neither can touch Docker.
function fixture(t) {
    const dir = realpathSync(mkdtempSync(path.join(tmpdir(), 'cf-workflows-')));
    for (const sub of ['scripts', '.build/testdb', 'resources', 'calls', 'processes', '.build/tools/go/bin']) mkdirSync(path.join(dir, sub), {recursive: true});
    for (const file of ['scripts/workflows.mjs', 'scripts/workflow-lib.mjs', 'Makefile']) copyFileSync(path.join(root, file), path.join(dir, file));
    const unrelated = { 'owner.dsn': 'unrelated-default-dsn', marker: 'unrelated-default-marker', container: 'unrelated-default-container' };
    for (const [file, value] of Object.entries(unrelated)) writeFileSync(path.join(dir, '.build/testdb', file), value);
    writeFileSync(path.join(dir, 'resources/unrelated'), 'keep');
    // External fixture executables register their groups before exposing ready
    // files. The real runner starts these in separate, detached process groups.
    writeFileSync(path.join(dir, 'scripts/register.cjs'), `
const fs = require('node:fs'), cp = require('node:child_process');
const result = cp.spawnSync('/bin/ps', ['-p', String(process.pid), '-o', 'pgid='], {encoding: 'utf8', timeout: 1000});
if (result.status !== 0) throw Error('Cannot register fixture process group');
const file = 'processes/' + process.pid;
fs.writeFileSync(file + '.tmp', String(Number(result.stdout.trim())));
fs.renameSync(file + '.tmp', file);
`);
    writeFileSync(path.join(dir, 'scripts/dev.mjs'), `
import './register.cjs';
import {writeFileSync, readFileSync, rmSync} from 'node:fs';
import path from 'node:path';
const dir = process.env.COSMOFORMER_TEST_DIR, id = path.basename(dir);
if (path.dirname(dir) !== path.resolve('.build/test-runs')) throw Error('unowned directory');
const resource = path.resolve('resources', id);
if (process.argv[2] === 'test-start') {
    writeFileSync(resource, dir);
    writeFileSync(path.join(dir, 'container-intent.json'), JSON.stringify({id}));
    if (['startup-failure', 'startup-wait'].includes(process.env.SCENARIO)) {
        // Model creation before the provisioner receives its container reply.
        writeFileSync('calls/' + id + '.started', dir);
        if (process.env.SCENARIO === 'startup-failure') process.exit(9);
        await new Promise(() => { setInterval(() => {}, 1000); });
    }
    writeFileSync(path.join(dir, 'owner.dsn'), 'dsn:' + id);
    writeFileSync(path.join(dir, 'marker'), 'marker:' + id);
    if (process.env.SCENARIO !== 'missing') writeFileSync(path.join(dir, 'container'), process.env.SCENARIO === 'empty' ? ' \\n\\t' : '  container:' + id + '\\n');
    writeFileSync('calls/' + id + '.started', dir);
} else if (process.argv[2] === 'test-stop') {
    if (readFileSync(resource, 'utf8') !== dir) throw Error('wrong resource owner');
    const intent = JSON.parse(readFileSync(path.join(dir, 'container-intent.json'), 'utf8'));
    if (intent.id !== id) throw Error('wrong receipt');
    writeFileSync('calls/' + id + '.stopped', dir);
    if (process.env.SCENARIO === 'cleanup-failure') process.exit(8);
    rmSync(resource);
    rmSync(path.join(dir, 'container-intent.json'));
    rmSync(path.join(dir, 'container'), {force: true});
} else throw Error('unexpected provisioner action');
`);
    const go = path.join(dir, '.build/tools/go/bin/go');
    writeFileSync(go, `#!${process.execPath}
require(process.cwd() + '/scripts/register.cjs');
const fs = require('node:fs'), path = require('node:path');
const env = process.env, dir = env.COSMOFORMER_TEST_DIR, id = path.basename(dir);
if (env.SCENARIO === 'unresponsive') {
    process.on('SIGTERM', () => {});
    const script = "require(process.cwd() + '/scripts/register.cjs'); process.on('SIGTERM', () => {}); require('node:fs').writeFileSync('calls/" + id + ".descendant', String(process.pid)); setInterval(() => {}, 1000);";
    require('node:child_process').spawn(process.execPath, ['-e', script], {stdio: 'inherit'});
    setInterval(() => {}, 1000);
}
fs.writeFileSync('calls/' + id + '.child', JSON.stringify({args: process.argv.slice(2), dsn: env.COSMOFORMER_TEST_OWNER_DSN, marker: env.COSMOFORMER_TEST_MARKER_FILE, container: env.COSMOFORMER_TEST_PG_CONTAINER, application: env.COSMOFORMER_DATABASE_DSN_FILE, migration: env.COSMOFORMER_MIGRATION_DSN_FILE}));
if (env.COSMOFORMER_TEST_OWNER_DSN !== 'dsn:' + id || env.COSMOFORMER_TEST_MARKER_FILE !== path.join(dir, 'marker') || fs.readFileSync(env.COSMOFORMER_TEST_MARKER_FILE, 'utf8') !== 'marker:' + id || env.COSMOFORMER_TEST_PG_CONTAINER !== 'container:' + id || env.COSMOFORMER_DATABASE_DSN_FILE || env.COSMOFORMER_MIGRATION_DSN_FILE) process.exit(42);
if (env.SCENARIO === 'test-failure') process.exit(7);
if (env.SCENARIO === 'test-wait') {
    let heartbeat = 0;
    const timer = setInterval(() => {
        const file = 'calls/' + id + '.heartbeat';
        fs.writeFileSync(file + '.tmp', String(++heartbeat));
        fs.renameSync(file + '.tmp', file);
        if (fs.existsSync('calls/' + id + '.release')) { clearInterval(timer); }
    }, 20);
}
`);
    chmodSync(go, 0o755);
    const running = [];
    let cleaning;
    function cleanup() {
        if (!cleaning) cleaning = (async () => {
            const groups = new Set(running.map(item => item.proc.pid).filter(Boolean));
            const collect = () => {
                for (const file of readdirSync(path.join(dir, 'processes')).filter(file => /^\d+$/.test(file))) {
                    const group = Number(readFileSync(path.join(dir, 'processes', file), 'utf8'));
                    assert.ok(Number.isSafeInteger(group) && group > 1, 'valid owned process group');
                    groups.add(group);
                }
                return processGroups(groups);
            };
            function send(members, signal) {
                for (const group of new Set(members.map(member => member.group))) {
                    try { process.kill(-group, signal); }
                    catch (error) {
                        if (error.code === 'ESRCH') continue;
                        if (error.code !== 'EPERM') throw error;
                        for (const member of members.filter(member => member.group === group)) {
                            try { process.kill(member.pid, signal); } catch (e) { if (e.code !== 'ESRCH') throw e; }
                        }
                    }
                }
            }
            // Continue collecting during shutdown: a child may register while
            // its parent is handling SIGTERM. Never await close without a bound.
            const graceful = performance.now() + 300;
            let members;
            while ((members = collect()).length && performance.now() < graceful) { send(members, 'SIGTERM'); await pause(); }
            const forced = performance.now() + 3000;
            while ((members = collect()).length && performance.now() < forced) { send(members, 'SIGKILL'); await pause(); }
            assert.deepEqual(collect(), [], 'owned fixture processes must stop; retain ' + dir);
            let timer;
            try {
                await Promise.race([
                    Promise.all(running.map(item => item.done)),
                    new Promise((_, reject) => { timer = setTimeout(() => reject(Error('Fixture pipes did not close; retain ' + dir)), 1000); })
                ]);
            } finally { clearTimeout(timer); }
            rmSync(dir, {recursive: true, force: true});
        })().catch(error => {
            // Failed cleanup retains the directory and fails the test. Open
            // pipes must not keep that failure from reaching the test runner.
            for (const {proc} of running) {
                proc.unref(); proc.stdout.destroy(); proc.stderr.destroy();
            }
            throw error;
        });
        return cleaning;
    }
    t.after(cleanup);
    return {
        dir, cleanup,
        start(scenario = '', stale = true, makeArgs) {
            const env = {...process.env, SCENARIO: scenario, TEST_ARGS: "-run 'TestOne|TestTwo' -count=1", COSMOFORMER_DATABASE_DSN_FILE: '/unrelated/app', COSMOFORMER_MIGRATION_DSN_FILE: '/unrelated/migration'};
            for (const key of ['COSMOFORMER_TEST_OWNER_DSN', 'COSMOFORMER_TEST_MARKER_FILE', 'COSMOFORMER_TEST_PG_CONTAINER']) {
                if (stale) env[key] = 'inherited-stale'; else delete env[key];
            }
            const command = makeArgs ? 'make' : process.execPath;
            const args = makeArgs ? ['test-integration-local', 'NODE=' + process.execPath, ...makeArgs] : ['scripts/workflows.mjs', 'test'];
            const proc = spawn(command, args, {cwd: dir, env, detached: true, stdio: ['ignore', 'pipe', 'pipe']});
            const item = {proc, output: ''};
            for (const stream of [proc.stdout, proc.stderr]) stream.on('data', bytes => { item.output += bytes; });
            item.done = new Promise((resolve, reject) => { proc.once('error', reject); proc.once('close', (code, signal) => resolve({code, signal})); });
            running.push(item);
            return item;
        },
        calls(suffix) { return readdirSync(path.join(dir, 'calls')).filter(file => file.endsWith('.' + suffix)); },
        async wait(suffix, count = 1) {
            const deadline = Date.now() + 10000;
            while (this.calls(suffix).length < count && Date.now() < deadline) await pause();
            assert.equal(this.calls(suffix).length, count, `waiting for ${suffix}`);
            return this.calls(suffix).map(file => file.slice(0, -suffix.length - 1));
        },
        assertClean(ids) {
            assert.deepEqual(readdirSync(path.join(dir, 'resources')), ['unrelated']);
            for (const id of ids) {
                assert.ok(existsSync(path.join(dir, 'calls', id + '.stopped')));
                assert.ok(!existsSync(path.join(dir, '.build/test-runs', id)));
            }
            assert.equal(readFileSync(path.join(dir, 'resources/unrelated'), 'utf8'), 'keep');
            for (const [file, value] of Object.entries(unrelated)) assert.equal(readFileSync(path.join(dir, '.build/testdb', file), 'utf8'), value);
        }
    };
}

for (const stale of [true, false]) test(`integration child receives one owned context despite ${stale ? 'inherited stale settings' : 'unrelated Make defaults'}`, {timeout: 15000}, async t => {
    const f = fixture(t), item = f.start('', stale);
    assert.equal((await item.done).code, 0, item.output);
    const [id] = await f.wait('child');
    const context = JSON.parse(readFileSync(path.join(f.dir, 'calls', id + '.child'), 'utf8'));
    assert.equal(context.container, 'container:' + id);
    assert.equal(context.dsn, 'dsn:' + id);
    assert.equal(context.marker, path.join(f.dir, '.build/test-runs', id, 'marker'));
    assert.deepEqual(context.args.slice(-3), ['-run', 'TestOne|TestTwo', '-count=1']);
    f.assertClean([id]);
});

for (const scenario of ['', 'test-failure']) test(`outer Make overrides cannot replace the owned context during ${scenario || 'success'}`, {timeout: 15000}, async t => {
    const f = fixture(t), item = f.start(scenario, true, [
        'COSMOFORMER_TEST_OWNER_DSN=command-line-stale-dsn',
        'COSMOFORMER_TEST_MARKER_FILE=command-line-stale-marker',
        'COSMOFORMER_TEST_PG_CONTAINER=command-line-stale-container',
        'COSMOFORMER_TEST_DIR=/unrelated/run',
        'COSMOFORMER_DATABASE_DSN_FILE=/unrelated/app',
        'COSMOFORMER_MIGRATION_DSN_FILE=/unrelated/migration'
    ]);
    const result = await item.done;
    assert.equal(result.code, scenario ? 2 : 0, item.output);
    if (scenario) assert.match(item.output, /Error 7/);
    const [id] = await f.wait('child');
    const context = JSON.parse(readFileSync(path.join(f.dir, 'calls', id + '.child'), 'utf8'));
    assert.equal(context.dsn, 'dsn:' + id);
    assert.equal(context.marker, path.join(f.dir, '.build/test-runs', id, 'marker'));
    assert.equal(context.container, 'container:' + id);
    assert.equal(context.application, undefined);
    assert.equal(context.migration, undefined);
    assert.deepEqual(context.args.slice(-3), ['-run', 'TestOne|TestTwo', '-count=1']);
    f.assertClean([id]);
});

for (const scenario of ['missing', 'empty', 'startup-failure', 'test-failure']) test(`integration ${scenario} fails and cleans only owned resources`, {timeout: 15000}, async t => {
    const f = fixture(t), item = f.start(scenario);
    assert.equal((await item.done).code, 1, item.output);
    const ids = await f.wait('started');
    assert.equal(f.calls('child').length, scenario === 'test-failure' ? 1 : 0, item.output);
    if (scenario === 'missing' || scenario === 'empty') assert.match(item.output, /container/i);
    if (scenario === 'test-failure') assert.match(item.output, /Error 7/);
    f.assertClean(ids);
});

for (const signal of ['SIGINT', 'SIGTERM']) for (const phase of ['startup', 'test']) test(`${signal} during ${phase} cleans only the interrupted run`, {timeout: 15000}, async t => {
    const f = fixture(t), item = f.start(phase + '-wait');
    const ids = await f.wait(phase === 'startup' ? 'started' : 'child');
    if (phase === 'startup') assert.ok(!existsSync(path.join(f.dir, '.build/test-runs', ids[0], 'container')));
    item.proc.kill(signal);
    assert.equal((await item.done).code, 130, item.output);
    if (phase === 'startup') assert.deepEqual(f.calls('child'), []);
    f.assertClean(ids);
});

test('overlapping runs keep distinct contexts and cleanup leaves the other run live', {timeout: 15000}, async t => {
    const f = fixture(t), first = f.start('test-wait');
    const [a] = await f.wait('child');
    const second = f.start('test-wait');
    const b = (await f.wait('child', 2)).find(id => id !== a);
    assert.ok(b);
    writeFileSync(path.join(f.dir, 'calls', a + '.release'), 'release');
    assert.equal((await first.done).code, 0, first.output);
    assert.ok(!existsSync(path.join(f.dir, 'resources', a)));
    assert.ok(existsSync(path.join(f.dir, 'resources', b)));
    assert.ok(existsSync(path.join(f.dir, '.build/test-runs', b, 'container')));
    assert.ok(!existsSync(path.join(f.dir, 'calls', b + '.stopped')));
    second.proc.kill('SIGINT');
    assert.equal((await second.done).code, 130, second.output);
    f.assertClean([a, b]);
});

test('cleanup failure is reported and retains the owned recovery receipt', {timeout: 15000}, async t => {
    const f = fixture(t), item = f.start('cleanup-failure');
    assert.equal((await item.done).code, 1, item.output);
    const [id] = await f.wait('stopped');
    assert.ok(existsSync(path.join(f.dir, '.build/test-runs', id, 'container-intent.json')));
    assert.ok(existsSync(path.join(f.dir, 'resources', id)));
    assert.equal(readFileSync(path.join(f.dir, 'resources/unrelated'), 'utf8'), 'keep');
});

for (const exitedLeader of [false, true]) test(`fixture cleanup stops signal-ignoring descendants with ${exitedLeader ? 'an exited' : 'a live'} runner`, {timeout: 15000}, async t => {
    const f = fixture(t), sibling = fixture(t);
    const file = path.join(f.dir, 'scripts/workflows.mjs');
    const original = readFileSync(file, 'utf8');
    const handlers = "process.on('SIGINT', signal); process.on('SIGTERM', signal);";
    assert.ok(original.includes(handlers));
    writeFileSync(file, original.replace(handlers, exitedLeader
        ? "process.on('SIGINT', () => process.exit(7)); process.on('SIGTERM', () => {});"
        : "process.on('SIGINT', () => {}); process.on('SIGTERM', () => {});"));
    const other = sibling.start('test-wait');
    const [otherID] = await sibling.wait('heartbeat');
    const item = f.start('unresponsive');
    const [id] = await f.wait('descendant');
    const descendant = Number(readFileSync(path.join(f.dir, 'calls', id + '.descendant'), 'utf8'));
    const groups = new Set(readdirSync(path.join(f.dir, 'processes')).filter(file => /^\d+$/.test(file)).map(file => Number(readFileSync(path.join(f.dir, 'processes', file), 'utf8'))));
    groups.add(item.proc.pid);
    assert.ok(processGroups(groups).some(member => member.pid === descendant));
    if (exitedLeader) {
        const exited = new Promise(resolve => item.proc.once('exit', resolve));
        item.proc.kill('SIGINT');
        await exited;
        assert.equal(item.proc.exitCode, 7);
        assert.ok(processGroups(groups).some(member => member.pid === descendant));
    }
    await f.cleanup();
    assert.deepEqual(processGroups(groups), [], 'no owned runner or descendant may survive');
    assert.ok(!existsSync(f.dir));
    if (!exitedLeader) assert.equal((await item.done).signal, 'SIGKILL');
    const heartbeat = path.join(sibling.dir, 'calls', otherID + '.heartbeat');
    const before = Number(readFileSync(heartbeat, 'utf8'));
    let observed = before;
    for (let i = 0; i < 100 && observed <= before; i++) {
        await pause();
        observed = Number(readFileSync(heartbeat, 'utf8'));
    }
    assert.ok(Number.isSafeInteger(before) && before > 0);
    assert.ok(Number.isSafeInteger(observed) && observed > before, 'unrelated child must keep doing work');
    writeFileSync(path.join(sibling.dir, 'calls', otherID + '.release'), 'release');
    assert.equal((await other.done).code, 0, other.output);
    sibling.assertClean([otherID]);
});
