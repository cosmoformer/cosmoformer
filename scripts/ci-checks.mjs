// Parallel CI producers publish only closed, checksummed coverage bundles.
import {execFileSync} from 'node:child_process';
import {createHash} from 'node:crypto';
import {lstatSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, rmSync, writeFileSync} from 'node:fs';
import path from 'node:path';
import {beginCoverage, coverageIdentity} from './sonar-coverage.mjs';
import {mergeProfiles} from './merge-go-coverage.mjs';
import {checkGoCoverage} from './check-go-coverage.mjs';

// Code-unit ordering is intentional: artifact/test order must not depend on locale.
const compareText = (a, b) => a < b ? -1 : a > b ? 1 : 0;
const root = path.resolve(import.meta.dirname, '..');
export const shardCount = 4;
export const producerReports = {
    backend: ['go.backend.out'],
    ...Object.fromEntries(Array.from({length: shardCount}, (_, i) => [`acceptance-${i}`, [`go.acceptance-${i}.out`]])),
    linux: ['linux/source.out', 'linux/worker.out', 'linux/clock.out', 'linux/command.out'],
    web: ['web/lcov.info'], scripts: ['workflows/lcov.info', 'contract/lcov.info'], runtime: ['runtime/lcov.info'],
};
export function requireSuccessfulJobs(jobs) {
    const required = [...new Set(Object.keys(producerReports).map(id => id.replace(/-\d+$/, ''))), 'native'].sort(compareText);
    if (!jobs || !equal(Object.keys(jobs).sort(compareText), required) || Object.values(jobs).some(job => job.result !== 'success')) throw Error('Every parallel test job must succeed before coverage aggregation');
}
const contextRoots = ['web/app/.generated/api.ts', 'web/app/.generated/validators.d.ts', 'web/.react-router/types'];
const hash = bytes => createHash('sha256').update(bytes).digest('hex');
const json = file => JSON.parse(readFileSync(file, 'utf8'));
const put = (file, bytes) => { mkdirSync(path.dirname(file), {recursive: true}); writeFileSync(file, bytes); };
const equal = (a, b) => JSON.stringify(a) === JSON.stringify(b);
const execute = (command, args, options = {}) => execFileSync(command, args, {encoding: 'utf8', maxBuffer: 32 * 1024 * 1024, timeout: 15 * 60 * 1000, ...options});
function settings(options) { return {base: root, env: process.env, run: execute, ...options}; }
function identity({base, env, run}) {
    const source = coverageIdentity(base, run);
    if (env.GITHUB_SHA && env.GITHUB_SHA !== source.head) throw Error('Checkout does not match the workflow commit');
    return {...source, run: env.GITHUB_RUN_ID ?? 'local', attempt: env.GITHUB_RUN_ATTEMPT ?? 'local'};
}
function requireProducer(id) {
    if (!Object.hasOwn(producerReports, id)) throw Error('Unknown coverage producer');
}
function belongsToRun(expected, receipt) {
    if (!equal([expected.head, expected.digest, expected.run], [receipt.head, receipt.digest, receipt.run])) return false;
    if (expected.attempt === 'local') return receipt.attempt === 'local';
    const valid = value => typeof value === 'string' && /^[1-9]\d{0,8}$/.test(value);
    // A failed-job retry can reuse successful producers from an earlier attempt
    // of this exact run/checkout; future attempts or other runs cannot be used.
    return valid(expected.attempt) && valid(receipt.attempt) && BigInt(receipt.attempt) <= BigInt(expected.attempt);
}
function filesAt(base, relative) {
    const file = path.join(base, relative), status = lstatSync(file);
    if (status.isDirectory()) return readdirSync(file).sort(compareText).flatMap(name => filesAt(base, `${relative}/${name}`));
    if (!status.isFile() || status.nlink !== 1) throw Error('Coverage inputs must be regular, unlinked files');
    return [relative];
}
function checkedRead(base, relative) {
    if (path.isAbsolute(relative) || relative.split('/').some(part => !part || part === '.' || part === '..')) throw Error('Invalid artifact path');
    // Check every component, including generated-context parents.
    let current = base;
    for (const part of relative.split('/')) {
        current = path.join(current, part);
        if (lstatSync(current).isSymbolicLink()) throw Error('Artifact symlinks are not allowed');
    }
    const status = lstatSync(current);
    if (!status.isFile() || status.nlink !== 1 || !status.size || status.size > 32 * 1024 * 1024) throw Error('Missing, empty or oversized artifact');
    return readFileSync(current);
}
export function selectShard(names, index, count = shardCount) {
    if (!Number.isInteger(index) || !Number.isInteger(count) || count < 1 || index < 0 || index >= count || names.length < count || new Set(names).size !== names.length || names.some(name => !/^(Test|Example|Fuzz)[\p{L}\p{N}_]*$/u.test(name))) throw Error('Invalid acceptance test inventory or shard');
    return [...names].sort(compareText).filter((_, i) => i % count === index);
}
export function beginProducer(id, options) {
    requireProducer(id);
    const o = settings(options), started = {version: 1, id, ...identity(o), started: Date.now()};
    for (const file of producerReports[id]) rmSync(path.join(o.base, '.build/coverage', file), {force: true});
    if (id === 'web') for (const file of contextRoots) rmSync(path.join(o.base, file), {recursive: true, force: true});
    rmSync(path.join(o.base, '.build/ci/artifact'), {recursive: true, force: true});
    const marker = path.join(o.base, '.build/ci/start.json');
    put(marker, JSON.stringify(started));
    // Compare filesystem timestamps with each other, rather than with Date.now:
    // Linux mtime precision/clock granularity can lag the wall-clock sample.
    started.started = lstatSync(marker).mtimeMs;
    put(marker, JSON.stringify(started));
    return started;
}
export function sealProducer(id, options, tests) {
    requireProducer(id);
    const o = settings(options), started = json(path.join(o.base, '.build/ci/start.json'));
    if (started.id !== id || !equal(identity(o), {head: started.head, digest: started.digest, run: started.run, attempt: started.attempt})) throw Error('Producer source or run changed');
    const sources = producerReports[id].map(file => [`.build/coverage/${file}`, file]);
    if (id === 'web') for (const dir of contextRoots) for (const file of filesAt(o.base, dir)) sources.push([file, `context/${file}`]);
    const output = path.join(o.base, '.build/ci/artifact'), files = {};
    for (const [source, destination] of sources) {
        const bytes = checkedRead(o.base, source);
        if (lstatSync(path.join(o.base, source)).mtimeMs < started.started) throw Error('Stale producer output');
        files[destination] = hash(bytes); put(path.join(output, destination), bytes);
    }
    put(path.join(output, 'receipt.json'), JSON.stringify({...started, files, ...(tests ? {tests} : {})}));
}
export function verifyTestEvents(output, selected) {
    const completed = [];
    for (const line of output.trim().split('\n')) {
        const event = JSON.parse(line);
        if (event.Test && !event.Test.includes('/') && ['pass', 'skip', 'fail'].includes(event.Action)) {
            if (event.Action === 'fail') throw Error('Acceptance test failed');
            completed.push(event.Test);
        }
    }
    if (!equal([...completed].sort(compareText), [...selected].sort(compareText))) throw Error('Acceptance shard did not execute its exact test selection');
    return completed.sort(compareText);
}
export function runGoProducer(id, options) {
    if (id !== 'backend' && !/^acceptance-[0-3]$/.test(id)) throw Error('Unknown Go producer');
    const o = settings(options); beginProducer(id, o);
    const coverage = path.join(o.base, '.build/coverage'); mkdirSync(coverage, {recursive: true});
    const raw = mkdtempSync(path.join(coverage, 'go-raw.'));
    const env = {...o.env, COSMOFORMER_TEST_COVER_DIR: raw}, go = o.env.COSMOFORMER_GO || 'go';
    const flags = ['-race', '-count=1', '-timeout=15m', '-tags=integration', '-covermode=atomic', '-coverpkg=./internal/...,./cmd/...'];
    const invoke = args => o.run(go, args, {cwd: o.base, env});
    try {
        let tests, args = ['./internal/...', './cmd/...'];
        if (id !== 'backend') {
            const inventory = invoke(['test', ...flags, '-list=.', './tests/acceptance', '-args', '-test.gocoverdir=' + raw]).split('\n').filter(line => /^(Test|Example|Fuzz)/.test(line)).sort(compareText);
            const selected = selectShard(inventory, Number(id.at(-1)));
            tests = {inventory, selected};
            args = ['-run=^(' + selected.join('|') + ')$', './tests/acceptance'];
        }
        const output = invoke(['test', ...flags, '-json', ...args, '-args', '-test.gocoverdir=' + raw]);
        console.log(output);
        if (tests) tests.completed = verifyTestEvents(output, tests.selected);
        invoke(['tool', 'covdata', 'textfmt', '-i=' + raw, '-o=' + path.join(coverage, producerReports[id][0])]);
        sealProducer(id, o, tests);
    } finally { rmSync(raw, {recursive: true, force: true}); }
}
function validateSelection(receipts) {
    let inventory;
    for (let i = 0; i < shardCount; i++) {
        const tests = receipts[`acceptance-${i}`].tests;
        if (!tests || inventory && !equal(inventory, tests.inventory)) throw Error('Acceptance shard inventories differ or are missing');
        inventory = tests.inventory;
        const selected = selectShard(inventory, i);
        if (!equal(selected, tests.selected) || !equal([...selected].sort(compareText), tests.completed)) throw Error('Missing or incorrect acceptance shard execution');
    }
}
export function assembleCoverage(input, options) {
    const o = settings(options), expected = identity(o), ids = Object.keys(producerReports);
    if (!equal(readdirSync(input).sort(compareText), ids.map(id => `coverage-${id}`).sort(compareText))) throw Error('Coverage producers are missing or unexpected');
    const receipts = {}, contents = new Map();
    for (const id of ids) {
        const dir = path.join(input, `coverage-${id}`);
        if (!lstatSync(dir).isDirectory() || lstatSync(dir).isSymbolicLink()) throw Error('Invalid coverage bundle directory');
        const receipt = JSON.parse(checkedRead(dir, 'receipt.json'));
        if (receipt.version !== 1 || receipt.id !== id || !belongsToRun(expected, receipt)) throw Error('Coverage belongs to another producer, commit or workflow run');
        const names = Object.keys(receipt.files).sort(compareText), required = producerReports[id];
        if (required.some(file => !names.includes(file)) || names.some(file => !required.includes(file) && !(id === 'web' && (/^context\/web\/\.react-router\/types\/.+\.ts$/.test(file) || contextRoots.slice(0, 2).some(root => file === `context/${root}`))))) throw Error('Unexpected or missing coverage files');
        if (id === 'web' && (contextRoots.slice(0, 2).some(root => !names.includes(`context/${root}`)) || !names.some(file => file.startsWith('context/web/.react-router/types/')))) throw Error('Missing generated frontend context');
        if (!equal(filesAt(dir, '.').map(file => file.slice(2)).sort(compareText), ['receipt.json', ...names].sort(compareText))) throw Error('Unreceipted artifact files');
        for (const file of names) {
            const bytes = checkedRead(dir, file);
            if (hash(bytes) !== receipt.files[file] || contents.has(file)) throw Error('Changed or duplicate coverage output');
            contents.set(file, bytes);
        }
        receipts[id] = receipt;
    }
    validateSelection(receipts);
    const go = mergeProfiles([...contents].filter(([name]) => name.endsWith('.out')).map(([, bytes]) => bytes.toString()));
    if (!checkGoCoverage(go).passes) throw Error('Merged Go coverage must be strictly above 80%');
    // Download timestamps are not provenance: verify all receipts first, then
    // create the ordinary fresh, immutable-input receipt used by the Sonar gate.
    beginCoverage(o.base, o.run);
    put(path.join(o.base, '.build/coverage/go.out'), go);
    for (const [file, bytes] of contents) if (!file.endsWith('.out')) put(path.join(o.base, file.startsWith('context/') ? file.slice(8) : `.build/coverage/${file}`), bytes);
    console.log('Verified all coverage producers and the complete, disjoint acceptance test inventory.');
}
if (import.meta.main) {
    try {
        const [command, id] = process.argv.slice(2);
        if (command === 'begin') beginProducer(id);
        else if (command === 'seal') sealProducer(id);
        else if (command === 'go') runGoProducer(id);
        else if (command === 'assemble') assembleCoverage(path.resolve(id));
        else if (command === 'require-jobs') requireSuccessfulJobs(JSON.parse(process.env.NEEDS_JSON));
        else throw Error('Use begin, seal, go, assemble or require-jobs');
    } catch (error) { console.error(error.stdout || '', error.stderr || '', error.message); process.exitCode = 1; }
}
