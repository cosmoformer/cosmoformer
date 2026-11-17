import {test} from 'node:test';
import assert from 'node:assert/strict';
import {existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync} from 'node:fs';
import {spawnSync} from 'node:child_process';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {collectHostCoverage, resetGoCoverage} from './go-coverage.mjs';

const profile = count => `mode: atomic\nexample/internal/main.go:1.1,2.2 2 ${count}\n`;
function fixture(t) {
    const base = mkdtempSync(path.join(tmpdir(), 'cf-host-coverage-'));
    t.after(() => rmSync(base, {recursive: true, force: true}));
    return {base, output: path.join(base, '.build/coverage/go.host.out'), calls: []};
}
test('local coverage runs backend and every CI group and unions overlapping blocks once', async t => {
    const f = fixture(t);
    await collectHostCoverage({base: f.base, run: async id => {
        f.calls.push(id);
        writeFileSync(path.join(f.base, `.build/coverage/go.${id}.out`), profile(id === 'backend' ? 0 : 1));
    }});
    assert.deepEqual(f.calls, ['backend', 'acceptance-0', 'acceptance-1', 'acceptance-2', 'acceptance-3']);
    assert.equal(readFileSync(f.output, 'utf8'), profile(1));
});
test('a failed group waits for the others and cannot leave a stale aggregate', async t => {
    const f = fixture(t); let completed = 0;
    const run = async id => {
        if (id === 'acceptance-0') throw Error('synthetic test failure');
        await new Promise(resolve => setImmediate(resolve)); completed++;
        writeFileSync(path.join(f.base, `.build/coverage/go.${id}.out`), profile(1));
    };
    await assert.rejects(collectHostCoverage({base: f.base, run}), /groups failed/);
    assert.equal(completed, 4); assert.equal(existsSync(f.output), false);
});
test('missing profile cannot reuse output from a previous collection', async t => {
    const f = fixture(t);
    const run = async id => writeFileSync(path.join(f.base, `.build/coverage/go.${id}.out`), profile(1));
    await collectHostCoverage({base: f.base, run});
    await assert.rejects(collectHostCoverage({base: f.base, run: async id => { if (id !== 'acceptance-3') await run(id); }}), /ENOENT/);
    assert.equal(existsSync(f.output), false);
});
test('a failed rerun invalidates final Sonar/HTML output as well as the host union', async t => {
    const f = fixture(t);
    await collectHostCoverage({base: f.base, run: async id => writeFileSync(path.join(f.base, `.build/coverage/go.${id}.out`), profile(1))});
    for (const file of ['go.out', 'go.html']) writeFileSync(path.join(f.base, '.build/coverage', file), 'previous successful output');
    await assert.rejects(collectHostCoverage({base: f.base, run: async () => { throw Error('backend failed'); }}), /backend failed/);
    for (const file of ['go.host.out', 'go.out', 'go.html']) assert.equal(existsSync(path.join(f.base, '.build/coverage', file)), false);
    assert.doesNotThrow(() => resetGoCoverage(f.base));
});
test('Make invalidates final Go reports before a failing Linux prerequisite', t => {
    const f = fixture(t), scripts = path.join(f.base, 'scripts'), coverage = path.join(f.base, '.build/coverage');
    mkdirSync(scripts, {recursive: true}); mkdirSync(coverage, {recursive: true});
    writeFileSync(path.join(scripts, 'go-coverage.mjs'), `import {resetGoCoverage} from ${JSON.stringify(new URL('./go-coverage.mjs', import.meta.url).href)}; resetGoCoverage(process.cwd());`);
    for (const file of ['go.host.out', 'go.out', 'go.html']) writeFileSync(path.join(coverage, file), 'previous success');
    const result = spawnSync('make', ['-f', path.resolve(import.meta.dirname, '../Makefile'), 'test-go-coverage', 'MAKE=false', 'NODE=' + process.execPath], {cwd: f.base, encoding: 'utf8'});
    assert.ifError(result.error); assert.notEqual(result.status, 0);
    assert.match(result.stdout, /false test-linux-coverage/);
    for (const file of ['go.host.out', 'go.out', 'go.html']) assert.equal(existsSync(path.join(coverage, file)), false);
});
