import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { spawnSync } from 'node:child_process';
import { checkGoCoverage } from './check-go-coverage.mjs';

const profile = (covered, missed) => `mode: atomic\np/a.go:1.1,2.2 ${covered} 3\np/b.go:1.1,2.2 ${missed} 0\n`;

test('requires strictly more than 80% of statements, weighted by block size', () => {
    assert.equal(checkGoCoverage(profile(80, 20)).passes, false);
    assert.equal(checkGoCoverage(profile(79, 21)).passes, false);
    assert.deepEqual(checkGoCoverage(profile(81, 19)), { covered: 81n, total: 100n, passes: true });
    assert.equal(checkGoCoverage(profile(100, 0)).passes, true);
    assert.equal(checkGoCoverage(profile(0, 100)).passes, false);
});

test('uses exact counts even when the displayed percentage rounds to 80%', () => {
    assert.equal(checkGoCoverage(profile(800000000000000001n, 200000000000000000n)).passes, true);
    assert.equal(checkGoCoverage(profile(800000000000000000n, 200000000000000001n)).passes, false);
});

test('does not inflate coverage when a profile repeats source blocks', () => {
    const input = profile(81, 19) + 'p/a.go:1.1,2.2 81 0\np/b.go:1.1,2.2 19 0\n';
    assert.deepEqual(checkGoCoverage(input), { covered: 81n, total: 100n, passes: true });
});

test('fails closed for missing, malformed, incompatible or zero-statement data', () => {
    for (const input of ['', 'mode: atomic\n', 'mode: set\np/a.go:1.1,2.2 1 1',
        'mode: atomic\ninvalid', profile(0, 0), profile(81, 19) + 'p/a.go:1.1,2.2 82 1\n']) {
        assert.throws(() => checkGoCoverage(input));
    }
});

test('command exits unsuccessfully at the boundary and for unusable inputs', () => {
    const dir = mkdtempSync(join(tmpdir(), 'cosmoformer-coverage-'));
    const path = join(dir, 'go.out');
    const run = (...args) => spawnSync(process.execPath, ['scripts/check-go-coverage.mjs', ...args], { encoding: 'utf8' });
    try {
        writeFileSync(path, profile(81, 19));
        const passing = run(path);
        assert.equal(passing.status, 0, passing.stderr);
        assert.match(passing.stdout, /81\.00% \(81\/100\); required >80%/);
        writeFileSync(path, profile(80, 20));
        assert.equal(run(path).status, 1);
        writeFileSync(path, 'mode: atomic\n');
        assert.equal(run(path).status, 1);
        assert.equal(run(join(dir, 'missing')).status, 1);
        assert.equal(run().status, 1);
        assert.equal(run(path, path).status, 1);
    } finally {
        rmSync(dir, { recursive: true, force: true });
    }
});
