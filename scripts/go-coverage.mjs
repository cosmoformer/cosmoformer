// Local coverage uses the same complete test groups as CI, without sharing its
// producer receipt directory between concurrent processes.
import {spawn} from 'node:child_process';
import {closeSync, mkdirSync, openSync, readFileSync, rmSync, writeFileSync} from 'node:fs';
import path from 'node:path';
import {collectGoProfile, shardCount} from './ci-checks.mjs';
import {mergeProfiles} from './merge-go-coverage.mjs';

const root = path.resolve(import.meta.dirname, '..');
export function resetGoCoverage(base = root) {
    for (const file of ['go.host.out', 'go.out', 'go.html']) rmSync(path.join(base, '.build/coverage', file), {force: true});
}
export async function collectHostCoverage({base = root, run = runGroup} = {}) {
    const dir = path.join(base, '.build/coverage'); mkdirSync(dir, {recursive: true});
    const ids = ['backend', ...Array.from({length: shardCount}, (_, i) => `acceptance-${i}`)];
    const output = path.join(dir, 'go.host.out'); resetGoCoverage(base);
    for (const id of ids) rmSync(path.join(dir, `go.${id}.out`), {force: true});
    await run('backend', base);
    // Await every concurrent group even on failure. Never merge an incomplete run.
    const results = await Promise.allSettled(ids.slice(1).map(id => run(id, base)));
    const failures = results.filter(result => result.status === 'rejected');
    if (failures.length) throw new AggregateError(failures.map(result => result.reason), 'Go coverage groups failed; see .build/coverage/go.*.log');
    writeFileSync(output, mergeProfiles(ids.map(id => readFileSync(path.join(dir, `go.${id}.out`), 'utf8'))));
}
function runGroup(id, base) {
    return new Promise((resolve, reject) => {
        const log = openSync(path.join(base, '.build/coverage', `go.${id}.log`), 'w', 0o600);
        const child = spawn(process.execPath, [path.join(root, 'scripts/go-coverage.mjs'), 'group', id], {cwd: base, env: process.env, stdio: ['ignore', log, log]});
        closeSync(log);
        child.once('error', reject);
        child.once('exit', (code, signal) => {
            if (code === 0) { console.log(`Go coverage ${id}: passed`); resolve(); }
            else reject(Error(`${id} failed (${signal || code}); see go.${id}.log`));
        });
    });
}
if (import.meta.main) {
    try {
        if (process.argv[2] === 'group' && process.argv.length === 4) collectGoProfile(process.argv[3]);
        else if (process.argv[2] === 'reset' && process.argv.length === 3) resetGoCoverage();
        else if (process.argv.length === 2) await collectHostCoverage();
        else throw Error('Use go-coverage.mjs [reset | group producer]');
    } catch (error) { console.error(error); process.exitCode = 1; }
}
