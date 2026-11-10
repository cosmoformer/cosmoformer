import {test} from 'node:test';
import assert from 'node:assert/strict';
import {spawnSync} from 'node:child_process';
import {chmodSync, cpSync, mkdtempSync, mkdirSync, readFileSync, readdirSync, realpathSync, rmSync, symlinkSync, utimesSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {assembleCoverage, beginProducer, producerReports, requireSuccessfulJobs, runGoProducer, sealProducer, selectShard, shardCount, verifyTestEvents} from './ci-checks.mjs';
import {prepareSnapshot} from './sonar-coverage.mjs';

const inventory = Array.from({length: 12}, (_, i) => `TestFixture${String(i).padStart(2, '0')}`);
const profile = 'mode: atomic\ngithub.com/cosmoformer/cosmoformer/internal/main.go:1.1,2.2 10 1\n';
test('required gate rejects failed, skipped, cancelled or missing jobs, including native tests without coverage', () => {
    const jobs = Object.fromEntries(['backend', 'acceptance', 'linux', 'native', 'runtime', 'web', 'scripts'].map(id => [id, {result: 'success'}]));
    assert.doesNotThrow(() => requireSuccessfulJobs(jobs));
    for (const result of ['failure', 'skipped', 'cancelled', null]) assert.throws(() => requireSuccessfulJobs({...jobs, native: {result}}));
    const missing = {...jobs}; delete missing.native;
    for (const value of [missing, {}, null, {...jobs, extra: {result: 'success'}}]) assert.throws(() => requireSuccessfulJobs(value));
});
function fixture(t) {
    const base = mkdtempSync(path.join(tmpdir(), 'cf-ci-'));
    t.after(() => rmSync(base, {recursive: true, force: true}));
    const write = (file, text) => { const full = path.join(base, file); mkdirSync(path.dirname(full), {recursive: true}); writeFileSync(full, text); };
    write('internal/main.go', 'synthetic source');
    const f = {base, write, calls: [], env: {GITHUB_SHA: 'a'.repeat(40), GITHUB_RUN_ID: '123', GITHUB_RUN_ATTEMPT: '2'}};
    f.run = (command, args, options) => {
        f.calls.push({command, args, options});
        if (command === 'git') return args.includes('ls-files') ? 'internal/main.go\0' : f.env.GITHUB_SHA + '\n';
        assert.equal(command, 'go');
        if (args.includes('-list=.')) return inventory.join('\n') + '\nok package\n';
        if (args.includes('covdata')) { writeFileSync(args.find(arg => arg.startsWith('-o=')).slice(3), profile); return ''; }
        assert.ok(args.includes('-race')); assert.ok(args.includes('-count=1')); assert.ok(args.includes('-covermode=atomic'));
        assert.ok(options.env.COSMOFORMER_TEST_COVER_DIR.startsWith(path.join(base, '.build/coverage/go-raw.')));
        const pattern = args.find(arg => arg.startsWith('-run='));
        return (pattern ? inventory.filter(name => new RegExp(pattern.slice(5)).test(name)) : ['TestBackend']).map(Test => JSON.stringify({Action: 'pass', Test})).join('\n');
    };
    f.options = {base, run: f.run, env: f.env};
    f.receipt = id => path.join(base, '.build/ci/incoming', `coverage-${id}`, 'receipt.json');
    f.edit = (id, change) => { const file = f.receipt(id), value = JSON.parse(readFileSync(file)); change(value); writeFileSync(file, JSON.stringify(value)); };
    f.publish = (id, text = profile) => {
        beginProducer(id, f.options);
        for (const file of producerReports[id]) write('.build/coverage/' + file, file.endsWith('.out') ? text : 'TN:\nSF:scripts/main.mjs\nDA:1,1\nend_of_record\n');
        if (id === 'web') for (const file of ['web/app/.generated/api.ts', 'web/app/.generated/validators.d.ts', 'web/.react-router/types/routes.ts']) write(file, 'synthetic generated types');
        const selected = id.startsWith('acceptance-') ? selectShard(inventory, Number(id.at(-1))) : null;
        sealProducer(id, f.options, selected ? {inventory, selected, completed: [...selected].sort()} : undefined);
        const incoming = path.join(base, '.build/ci/incoming'); mkdirSync(incoming, {recursive: true});
        cpSync(path.join(base, '.build/ci/artifact'), path.join(incoming, 'coverage-' + id), {recursive: true});
    };
    f.all = (text = profile) => { for (const id of Object.keys(producerReports)) f.publish(id, text); return path.join(base, '.build/ci/incoming'); };
    return f;
}

test('four deterministic shards cover every discovered test once, including new tests', () => {
    const names = [...inventory, 'ExampleSomething', 'FuzzParser', 'TestNewlyAdded'];
    const shards = Array.from({length: shardCount}, (_, i) => selectShard(names, i));
    assert.deepEqual(shards.flat().sort(), [...names].sort());
    assert.equal(new Set(shards.flat()).size, names.length);
    assert.ok(Math.max(...shards.map(s => s.length)) - Math.min(...shards.map(s => s.length)) <= 1);
    assert.deepEqual(selectShard([...names].reverse(), 2), shards[2]);
    for (const [list, index, count] of [[[], 0, 4], [names, 4, 4], [names, -1, 4], [names, 0, 0], [[...names, names[0]], 0, 4], [['TestInjected|All', ...names], 0, 4]]) assert.throws(() => selectShard(list, index, count));
});
test('shard assignment uses stable code-unit ordering for Unicode test names', () => {
    const names = ['TestΩmega', 'TestÄpfel', 'TestZulu', 'TestÉpsilon', 'Test_underscore', 'TestAlpha'];
    assert.deepEqual(selectShard(names, 0), ['TestAlpha', 'TestÉpsilon']);
    assert.deepEqual(selectShard(names, 1), ['TestZulu', 'TestΩmega']);
});
test('test events must prove the exact top-level selection, not merely a successful process', () => {
    const event = (Test, Action = 'pass') => JSON.stringify({Test, Action});
    assert.deepEqual(verifyTestEvents([event('TestA', 'run'), event('TestA/subtest'), event('TestA'), event('TestB', 'skip')].join('\n'), ['TestA', 'TestB']), ['TestA', 'TestB']);
    for (const output of [event('TestA'), [event('TestA'), event('TestA')].join('\n'), event('TestOther'), event('TestA', 'fail'), 'not JSON']) assert.throws(() => verifyTestEvents(output, ['TestA', 'TestB']));
});
for (const id of ['backend', 'acceptance-0', 'acceptance-1', 'acceptance-2', 'acceptance-3']) test(`${id} instruments tests and child commands, publishes proof and removes raw counters`, t => {
    const f = fixture(t); runGoProducer(id, f.options);
    const receipt = JSON.parse(readFileSync(path.join(f.base, '.build/ci/artifact/receipt.json')));
    assert.equal(receipt.id, id); assert.equal(receipt.run, '123');
    assert.ok(!readdirSync(path.join(f.base, '.build/coverage')).some(name => name.startsWith('go-raw.')));
    const run = f.calls.find(call => call.args.includes('-json'));
    assert.ok(run.args.includes('-args')); assert.ok(run.args.some(arg => arg.startsWith('-test.gocoverdir=')));
    if (id === 'backend') assert.ok(run.args.includes('./internal/...') && run.args.includes('./cmd/...'));
    else assert.deepEqual(receipt.tests.completed, selectShard(inventory, Number(id.at(-1))).sort());
});
test('Go failure cleans raw counters and cannot publish an artifact', t => {
    const f = fixture(t), run = f.run;
    assert.throws(() => runGoProducer('backend', {...f.options, run: (...args) => { if (args[0] === 'go') throw Error('synthetic failure'); return run(...args); }}), /synthetic failure/);
    assert.ok(!readdirSync(path.join(f.base, '.build/coverage')).some(name => name.startsWith('go-raw.')));
    assert.throws(() => readFileSync(path.join(f.base, '.build/ci/artifact/receipt.json')));
    assert.throws(() => runGoProducer('unknown', f.options), /Unknown/);
});
test('verified coverage is unioned once, restores only type context and passes existing fresh-snapshot checks', t => {
    const f = fixture(t), input = f.all(); assembleCoverage(input, f.options);
    const go = readFileSync(path.join(f.base, '.build/coverage/go.out'), 'utf8');
    assert.equal(go.trim().split('\n').length, 2, 'overlapping profiles must not inflate the denominator');
    const {snapshot} = prepareSnapshot(f.base, f.run);
    assert.match(readFileSync(path.join(snapshot, 'web/app/.generated/api.ts'), 'utf8'), /generated types/);
    assert.ok(!readdirSync(path.join(snapshot, 'web/app/.generated')).includes('validators.js'));
});
test('freshness boundaries use the filesystem clock even when wall-clock samples lead it', t => {
    const f = fixture(t), now = Date.now(); t.mock.method(Date, 'now', () => now + 1000);
    const input = f.all();
    assert.doesNotThrow(() => assembleCoverage(input, f.options));
    assert.doesNotThrow(() => prepareSnapshot(f.base, f.run));
});
test('the actual CI Make target passes the marked test database and producer to Node', t => {
    const f = fixture(t);
    f.write('Makefile', readFileSync(new URL('../Makefile', import.meta.url)));
    f.write('.build/testdb/owner.dsn', 'postgres://fixture@127.0.0.1:5432/cosmoformer_test_control');
    f.write('node', `#!${process.execPath}\nconsole.log(JSON.stringify({args:process.argv.slice(2),dsn:process.env.COSMOFORMER_TEST_OWNER_DSN,marker:process.env.COSMOFORMER_TEST_MARKER_FILE}));\n`);
    chmodSync(path.join(f.base, 'node'), 0o755);
    const env = {...process.env}; delete env.COSMOFORMER_TEST_OWNER_DSN; delete env.COSMOFORMER_TEST_MARKER_FILE;
    const result = spawnSync('make', ['--no-print-directory', 'test-ci-go', 'CI_GO_PRODUCER=acceptance-2', `NODE=${path.join(f.base, 'node')}`], {cwd: f.base, encoding: 'utf8', env});
    assert.equal(result.status, 0, result.stderr);
    const actual = JSON.parse(result.stdout.trim().split('\n').at(-1));
    assert.deepEqual(actual.args, ['scripts/ci-checks.mjs', 'go', 'acceptance-2']);
    assert.match(actual.dsn, /cosmoformer_test_control$/);
    assert.equal(actual.marker, path.join(realpathSync(f.base), '.build/testdb/marker'));
});
for (const field of ['head', 'digest', 'run', 'attempt', 'id', 'version']) test(`rejects mismatched producer ${field}`, t => {
    const f = fixture(t), input = f.all(); f.edit('backend', value => { value[field] = 'wrong'; });
    assert.throws(() => assembleCoverage(input, f.options), /another producer/);
});
test('failed-job retries reuse successful outputs only from earlier attempts of the same run and checkout', t => {
    const f = fixture(t), input = f.all(); f.edit('backend', value => { value.attempt = '1'; });
    assert.doesNotThrow(() => assembleCoverage(input, f.options));
    for (const attempt of ['3', '0', '-1', '1.5', 1]) {
        f.edit('backend', value => { value.attempt = attempt; });
        assert.throws(() => assembleCoverage(input, f.options), /another producer/);
    }
});
for (const failure of ['missing producer', 'extra producer', 'missing file', 'extra file', 'changed bytes', 'overlap', 'incomplete execution', 'different inventory', 'missing inventory', 'missing context', 'unsafe path', 'symlink']) test(`coverage aggregation rejects ${failure}`, t => {
    const f = fixture(t), input = f.all();
    const dir = path.join(input, 'coverage-backend');
    if (failure === 'missing producer') rmSync(dir, {recursive: true});
    if (failure === 'extra producer') mkdirSync(path.join(input, 'coverage-extra'));
    if (failure === 'missing file') rmSync(path.join(dir, 'go.backend.out'));
    if (failure === 'extra file') writeFileSync(path.join(dir, 'extra.out'), profile);
    if (failure === 'changed bytes') writeFileSync(path.join(dir, 'go.backend.out'), profile + '\n');
    if (failure === 'overlap') f.edit('acceptance-0', value => { value.tests.selected = selectShard(inventory, 1); });
    if (failure === 'incomplete execution') f.edit('acceptance-0', value => { value.tests.completed.pop(); });
    if (failure === 'different inventory') f.edit('acceptance-1', value => { value.tests.inventory.push('TestAdded'); });
    if (failure === 'missing inventory') f.edit('acceptance-1', value => { delete value.tests; });
    if (failure === 'missing context') f.edit('web', value => { delete value.files['context/web/app/.generated/api.ts']; });
    if (failure === 'unsafe path') f.edit('backend', value => { value.files['../outside'] = 'unsafe'; });
    if (failure === 'symlink') { rmSync(path.join(dir, 'go.backend.out')); symlinkSync('/etc/passwd', path.join(dir, 'go.backend.out')); }
    assert.throws(() => assembleCoverage(input, f.options));
});
test('a merged Go profile at exactly 80 percent cannot pass', t => {
    const f = fixture(t), input = f.all('mode: atomic\np/file.go:1.1,2.2 8 1\np/file.go:3.1,4.2 2 0\n');
    assert.throws(() => assembleCoverage(input, f.options), /strictly above 80/);
});
test('producers reject stale output, changed source, unexpected identity and unsafe context', t => {
    const f = fixture(t);
    assert.throws(() => beginProducer('unknown', f.options), /Unknown/);
    beginProducer('backend', f.options); f.write('.build/coverage/go.backend.out', profile);
    utimesSync(path.join(f.base, '.build/coverage/go.backend.out'), 0, 0);
    assert.throws(() => sealProducer('backend', f.options), /Stale/);
    f.write('internal/main.go', 'changed'); assert.throws(() => sealProducer('backend', f.options), /changed/);
    assert.throws(() => beginProducer('backend', {...f.options, env: {...f.env, GITHUB_SHA: 'wrong'}}), /workflow commit/);
    beginProducer('web', f.options); f.write('.build/coverage/web/lcov.info', 'synthetic');
    f.write('web/app/.generated/api.ts', 'type context'); f.write('web/app/.generated/validators.d.ts', 'type context');
    mkdirSync(path.join(f.base, 'web/.react-router'), {recursive: true}); symlinkSync('/etc', path.join(f.base, 'web/.react-router/types'));
    assert.throws(() => sealProducer('web', f.options), /regular/);
});
test('CLI refuses unknown commands without running tests', () => {
    const result = spawnSync(process.execPath, ['scripts/ci-checks.mjs', 'unknown'], {encoding: 'utf8'});
    assert.equal(result.status, 1); assert.match(result.stderr, /Use begin, seal, go, assemble or require-jobs/);
});
