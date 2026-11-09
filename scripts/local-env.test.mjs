import {test} from 'node:test';
import assert from 'node:assert/strict';
import {mkdtempSync, readFileSync, writeFileSync, rmSync, existsSync} from 'node:fs';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {spawnSync} from 'node:child_process';
import {start, tofu, cleanup, runStorageTest} from './local-env.mjs';

function directory(t) {
    const dir = mkdtempSync(path.join(tmpdir(), 'cosmoformer-local-env-'));
    t.after(() => rmSync(dir, {recursive: true, force: true})); return dir;
}
const receipt = dir => JSON.parse(readFileSync(path.join(dir, 'environment.json'), 'utf8'));
const label = (args, key) => args.find(value => value.startsWith(`${key}=`))?.slice(key.length + 1);

// Model Docker's resource/label/connectivity boundaries, not the implementation's
// provisioning phases. Tests run the shipped functions against real local files.
function dockerFixture(dir) {
    const resources = new Map(), calls = [], objects = new Map();
    const fixture = {resources, calls, failAt: null, runnerFailure: null, requestsFail: false};
    fixture.execute = (args, allowed = [0]) => {
        calls.push(args);
        const [command, operation] = args, name = args.at(-1);
        const step = command === 'info' ? 'daemon' : command === 'create' ? 'container' : command === 'start' ? 'start' : command === 'port' ? 'port' : command === 'volume' && operation === 'create' ? 'volume' : command === 'network' && operation === 'create' ? (name.includes('-access-') ? 'gateway' : 'internal') : command === 'network' && operation === 'connect' ? 'connect' : '';
        if (step && fixture.failAt === step) { fixture.failAt = null; throw Error(`injected ${step} failure`); }
        if (command === 'info') return {status: 0, stdout: 'fixture-daemon'};
        if (['container', 'network', 'volume'].includes(command) && operation === 'inspect') {
            const item = resources.get(`${command}:${name}`);
            if (!item) { assert.ok(allowed.includes(1)); return {status: 1, stdout: ''}; }
            const format = args[args.indexOf('--format') + 1];
            return {status: 0, stdout: format?.includes('Mounts') ? JSON.stringify(item.mounts) : format?.includes('NetworkSettings') ? JSON.stringify(item.networks) : format?.includes('cosmoformer.local-run') ? (item.invocation ?? '') : item.owner};
        }
        if (['network', 'volume'].includes(command) && operation === 'create') {
            assert.ok(!resources.has(`${command}:${name}`));
            resources.set(`${command}:${name}`, {owner: label(args, 'cosmoformer.local-id')});
            return {status: 0, stdout: name};
        }
        if (command === 'create') {
            const container = args[args.indexOf('--name') + 1], gateway = args[args.indexOf('--network') + 1];
            const mount = args.find(value => value.startsWith('type=volume,'));
            const volume = mount.match(/src=([^,]+)/)[1];
            assert.ok(resources.has(`volume:${volume}`), 'must not let Docker implicitly create an empty volume');
            const config = args.find(value => value.includes('dst=/etc/seaweedfs'));
            resources.set(`container:${container}`, {owner: label(args, 'cosmoformer.local-id'), networks: {[gateway]: {}}, mounts: [{Type:'volume',Name:config.match(/src=([^,]+)/)[1],Destination:'/etc/seaweedfs'}]});
            return {status: 0, stdout: container};
        }
        if (command === 'network' && operation === 'connect') {
            resources.get(`container:${name}`).networks[args.at(-2)] = {};
            return {status: 0, stdout: ''};
        }
        if (command === 'start') {
            assert.ok(['started', 'ready'].includes(receipt(dir).phase), 'start intent must be durable before data can be written');
            resources.get(`container:${name}`).started = true;
            if (fixture.failAt === 'start-response-loss') { fixture.failAt = null; throw Error('injected response loss after start'); }
            return {status: 0, stdout: name};
        }
        if (command === 'port') return {status: 0, stdout: '127.0.0.1:38000'};
        if (command === 'run') {
            if (args.includes('/bin/sh')) return {status:0,stdout:''};
            const container = args[args.indexOf('--name') + 1];
            const owner = label(args, 'cosmoformer.local-id'), invocation = label(args, 'cosmoformer.local-run');
            if (fixture.runnerFailure) {
                resources.set(`container:${container}`, {owner, invocation: fixture.runnerFailure === 'collision' ? 'other-invocation' : invocation});
                throw Error(fixture.runnerFailure === 'collision' ? 'container name already in use' : 'Docker client timed out');
            }
            return {status: 0, stdout: 'fixture output', stderr: ''};
        }
        if (command === 'ps') {
            const owner = label(args, 'label=cosmoformer.local-id');
            return {status: 0, stdout: [...resources.entries()].filter(([key, value]) => key.startsWith('container:') && value.owner === owner && value.invocation).map(([key]) => key.slice('container:'.length)).join('\n')};
        }
        if (command === 'rm') { resources.delete(`container:${name}`); return {status: 0, stdout: name}; }
        if (command === 'stop') return {status:0,stdout:name};
        if (['volume', 'network'].includes(command) && operation === 'rm') { resources.delete(`${command}:${name}`); return {status: 0, stdout: name}; }
        throw Error(`Unexpected Docker fixture command: ${args.join(' ')}`);
    };
    fixture.request = async (_, method, key, body) => {
        if (fixture.requestsFail) throw Error('injected readiness failure');
        if (method === 'PUT') objects.set(key, body);
        if (method === 'DELETE') { objects.delete(key); return {status: 204, text: ''}; }
        return {status: 200, text: objects.get(key) ?? ''};
    };
    fixture.pause = async () => {};
    return fixture;
}

for (const step of ['daemon', 'volume', 'internal', 'gateway', 'container', 'connect', 'start', 'port', 'start-response-loss']) {
    test(`storage retries after ${step} failure without changing identity or existing resources`, async t => {
        const dir = directory(t), fixture = dockerFixture(dir); fixture.failAt = step;
        await assert.rejects(start(dir, false, fixture), /injected/);
        const original = receipt(dir), retained = new Map(fixture.resources);
        const resumed = await start(dir, false, fixture);
        assert.equal(resumed.id, original.id); assert.equal(resumed.access, original.access); assert.equal(resumed.secret, original.secret);
        assert.equal(resumed.phase, 'ready');
        for (const [key, value] of retained) assert.strictEqual(fixture.resources.get(key), value);
        assert.deepEqual(Object.keys(fixture.resources.get(`container:${resumed.container}`).networks).sort(), [resumed.network, resumed.gateway].sort());
    });
}

test('readiness failure retries against the same potentially used volume', async t => {
    const dir = directory(t), fixture = dockerFixture(dir); fixture.requestsFail = true;
    await assert.rejects(start(dir, true, fixture), /did not become ready/);
    const env = receipt(dir), volume = fixture.resources.get(`volume:${env.volume}`);
    assert.equal(env.phase, 'started'); fixture.requestsFail = false;
    await start(dir, true, fixture); assert.strictEqual(fixture.resources.get(`volume:${env.volume}`), volume);
});

for (const phase of ['started', 'ready', undefined]) {
    test(`missing data volume is not replaced for ${phase ?? 'legacy'} receipts`, async t => {
        const dir = directory(t), fixture = dockerFixture(dir);
        const env = await start(dir, true, fixture);
        if (phase === undefined) delete env.phase; else env.phase = phase;
        writeFileSync(path.join(dir, 'environment.json'), JSON.stringify(env));
        fixture.resources.delete(`container:${env.container}`); fixture.resources.delete(`volume:${env.volume}`);
        const before = fixture.calls.length;
        await assert.rejects(start(dir, true, fixture), /data volume is missing.*Explicit recovery/);
        assert.ok(!fixture.calls.slice(before).some(args => args.includes('create')));
    });
}

test('an existing legacy environment upgrades without replacing its data', async t => {
    const dir = directory(t), fixture = dockerFixture(dir), env = await start(dir, true, fixture);
    delete env.phase; writeFileSync(path.join(dir, 'environment.json'), JSON.stringify(env));
    fixture.resources.get(`container:${env.container}`).mounts = [{Type: 'bind', Destination: '/etc/seaweedfs/s3.json'}];
    const volume = fixture.resources.get(`volume:${env.volume}`);
    assert.equal((await start(dir, true, fixture)).phase, 'ready');
    assert.strictEqual(fixture.resources.get(`volume:${env.volume}`), volume);
    assert.ok(fixture.calls.some(args => args[0] === 'stop'));
});

test('a missing derived config volume is recreated without replacing stored data', async t => {
    const dir = directory(t), fixture = dockerFixture(dir), env = await start(dir, true, fixture);
    const volume = fixture.resources.get(`volume:${env.volume}`);
    fixture.resources.delete(`volume:${env.configVolume}`);
    await start(dir, true, fixture);
    assert.strictEqual(fixture.resources.get(`volume:${env.volume}`), volume);
    assert.equal(fixture.resources.get(`volume:${env.configVolume}`).owner, env.id);
});

for (const kind of ['volume', 'network']) {
    test(`retry refuses a foreign ${kind} instead of adopting or replacing it`, async t => {
        const dir = directory(t), fixture = dockerFixture(dir); fixture.failAt = 'container';
        await assert.rejects(start(dir, true, fixture));
        const env = receipt(dir), key = `${kind}:${kind === 'volume' ? env.volume : env.network}`;
        fixture.resources.get(key).owner = 'foreign-owner';
        await assert.rejects(start(dir, true, fixture), /unowned/);
        assert.equal(fixture.resources.get(key).owner, 'foreign-owner');
    });
}

test('each native invocation has a distinct runner identity', t => {
    const dir = directory(t), fixture = dockerFixture(dir), env = {id: 'fixture', container: 'fixture', network: 'fixture'};
    tofu(dir, env, ['plan'], [0], false, fixture.execute); tofu(dir, env, ['plan'], [0], false, fixture.execute);
    const calls = fixture.calls.filter(args => args[0] === 'run');
    assert.notEqual(calls[0][calls[0].indexOf('--name') + 1], calls[1][calls[1].indexOf('--name') + 1]);
    assert.notEqual(label(calls[0], 'cosmoformer.local-run'), label(calls[1], 'cosmoformer.local-run'));
});

test('name-conflict cleanup leaves the existing invocation running', t => {
    const dir = directory(t), fixture = dockerFixture(dir); fixture.runnerFailure = 'collision';
    assert.throws(() => tofu(dir, {id: 'fixture', container: 'fixture', network: 'fixture'}, ['plan'], [0], false, fixture.execute), /name already in use/);
    assert.equal(fixture.resources.size, 1); assert.ok(!fixture.calls.some(args => args[0] === 'rm'));
});

test('a timed-out invocation removes only its own runner', t => {
    const dir = directory(t), fixture = dockerFixture(dir); fixture.runnerFailure = 'timeout';
    const previous = {owner: 'fixture', invocation: 'previous'};
    fixture.resources.set('container:previous-command', previous);
    assert.throws(() => tofu(dir, {id: 'fixture', container: 'fixture', network: 'fixture'}, ['apply'], [0], false, fixture.execute), /timed out/);
    assert.equal(fixture.resources.size, 1); assert.strictEqual(fixture.resources.get('container:previous-command'), previous);
});

test('disposable cleanup finds invocation-specific runners and retains other environments', async t => {
    const dir = directory(t), fixture = dockerFixture(dir), env = await start(dir, true, fixture);
    fixture.resources.set('container:own-runner', {owner: env.id, invocation: 'interrupted'});
    const foreign = {owner: 'different-environment', invocation: 'active'};
    fixture.resources.set('container:foreign-runner', foreign);
    cleanup(dir, fixture.execute);
    assert.equal(fixture.resources.size, 1); assert.strictEqual(fixture.resources.get('container:foreign-runner'), foreign);
    assert.equal(existsSync(dir), false);
});

for (const outcome of ['timeout', 'failure', 'success']) test(`storage parent cleans its recorded resources after child ${outcome}`, t => {
    const parent = directory(t);
    let child, fixture;
    const foreign = {owner: 'other-environment', invocation: 'active'};
    const launch = (command, args, options) => {
        assert.equal(command, process.execPath);
        assert.equal(args[1], 'test');
        assert.equal(options.timeout, 25);
        child = args[2];
        fixture = dockerFixture(child);
        const env = {id: 'owned-fixture', container: 'owned-server', volume: 'owned-data', configVolume: 'owned-config', network: 'owned-internal', gateway: 'owned-gateway'};
        writeFileSync(path.join(child, 'environment.json'), JSON.stringify(env));
        for (const [kind, name] of [['container', env.container], ['volume', env.volume], ['volume', env.configVolume], ['network', env.network], ['network', env.gateway]]) fixture.resources.set(`${kind}:${name}`, {owner: env.id});
        fixture.resources.set('container:owned-runner', {owner: env.id, invocation: 'interrupted'});
        fixture.resources.set('container:foreign-runner', foreign);
        if (outcome === 'timeout') return spawnSync(process.execPath, ['-e', 'setTimeout(() => {}, 10000)'], options);
        return {status: outcome === 'success' ? 0 : null, stdout: 'synthetic output', stderr: ''};
    };
    const invoke = () => runStorageTest({parent, launch, timeout: 25, execute: (...args) => fixture.execute(...args)});
    if (outcome === 'success') assert.equal(invoke(), 'synthetic output');
    else assert.throws(invoke, outcome === 'timeout' ? /ETIMEDOUT/ : /synthetic output/);
    assert.equal(existsSync(child), false);
    assert.equal(fixture.resources.size, 1);
    assert.strictEqual(fixture.resources.get('container:foreign-runner'), foreign);
});

test('storage parent removes its empty directory when launch fails before a receipt', t => {
    const parent = directory(t);
    let child;
    assert.throws(() => runStorageTest({parent, launch(_command, args) { child = args[2]; throw Error('launch failed'); }, execute() { assert.fail('no resources were recorded'); }}), /launch failed/);
    assert.equal(existsSync(child), false);
});
