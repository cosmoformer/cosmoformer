import {test} from 'node:test';
import assert from 'node:assert/strict';
import {mkdtempSync, mkdirSync, readFileSync, writeFileSync, chmodSync, rmSync, existsSync} from 'node:fs';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
import {spawnSync} from 'node:child_process';

const root = fileURLToPath(new URL('../', import.meta.url));
const original = JSON.parse(readFileSync(path.join(root, 'deploy/worker/runtimes.json'), 'utf8'));

function invoke(t, mutate = () => {}) {
    const dir = mkdtempSync(path.join(tmpdir(), 'cosmoformer-native-make-'));
    t.after(() => rmSync(dir, {recursive: true, force: true}));
    mkdirSync(path.join(dir, 'deploy/worker'), {recursive: true});
    mkdirSync(path.join(dir, 'bin'));
    writeFileSync(path.join(dir, 'Makefile'), readFileSync(path.join(root, 'Makefile')));
    const manifest = structuredClone(original);
    mutate(manifest);
    writeFileSync(path.join(dir, 'deploy/worker/runtimes.json'), JSON.stringify(manifest));
    const log = path.join(dir, 'calls.jsonl');
    const docker = path.join(dir, 'bin/docker');
    writeFileSync(docker, `#!${process.execPath}\nimport {appendFileSync} from 'node:fs';\nappendFileSync(process.env.NATIVE_TEST_DOCKER_LOG, JSON.stringify(process.argv.slice(2))+'\\n');\n`);
    chmodSync(docker, 0o755);
    const result = spawnSync('make', ['--no-print-directory', 'test-native'], {
        cwd: dir, encoding: 'utf8', timeout: 10000,
        env: {...process.env, PATH: `${path.join(dir, 'bin')}:${path.dirname(process.execPath)}:${process.env.PATH}`, NATIVE_TEST_DOCKER_LOG: log}
    });
    assert.ifError(result.error);
    const calls = existsSync(log) ? readFileSync(log, 'utf8').trim().split('\n').map(JSON.parse) : [];
    return {result, calls};
}

test('actual Make recipe passes valid JavaScript and runs only both pinned isolated fixtures', t => {
    const {result, calls} = invoke(t);
    assert.equal(result.status, 0, result.stderr);
    assert.equal(calls.length, 4);
    for (let i = 0; i < 2; i++) {
        const profile = original.runtimes[i];
        const build = calls[i * 2], run = calls[i * 2 + 1];
        assert.equal(build[0], 'build');
        assert.ok(build.includes(`ENGINE_VERSION=${profile.version}`));
        assert.ok(build.includes(`ENGINE_SHA256=${profile.engine_archive.sha256}`));
        assert.equal(run[0], 'run');
        for (const flag of ['--rm', '--network=none', '--read-only', '/tmp:rw,exec,nosuid,nodev,size=1g']) assert.ok(run.includes(flag));
        assert.equal(run.at(-1), `cosmoformer-native-tests:${profile.engine}-${profile.version}`);
        assert.ok(!run.some(arg => /DSN|mount|privileged/.test(arg)));
    }
});

for (const [name, mutate] of [
    ['runtime checksum', m => {m.runtimes[0].engine_archive.sha256 = '0'.repeat(64);}],
    ['architecture', m => {m.runtimes[0].arch = 'linux_arm64';}],
    ['archive member', m => {m.runtimes[0].test_provider_packages[0].member = '../unapproved';}],
    ['second runtime pin', m => {m.runtimes[1].version = '0.0.0';}]
]) test(`invalid ${name} fails before any Docker command`, t => {
    const {result, calls} = invoke(t, mutate);
    assert.notEqual(result.status, 0);
    assert.match(result.stderr, /Invalid pinned native fixture manifest/);
    assert.deepEqual(calls, []);
});
