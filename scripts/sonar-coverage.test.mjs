import {test, mock} from 'node:test';
import childProcess from 'node:child_process';
import {syncBuiltinESMExports} from 'node:module';
import assert from 'node:assert/strict';
import {mkdtempSync, mkdirSync, writeFileSync, readFileSync, existsSync, rmSync, symlinkSync, utimesSync} from 'node:fs';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {beginCoverage, prepareSnapshot, sourceFiles, sonarAPI, waitFor, scanOverall, serverImage, scannerImage, reports} from './sonar-coverage.mjs';

function fixture(t) {
    const base = mkdtempSync(path.join(tmpdir(), 'cosmoformer-sonar-'));
    t.after(() => rmSync(base, {recursive: true, force: true}));
    const files = ['internal/main.go', 'scripts/main.mjs', 'web/app/main.ts', 'web/tsconfig.json', 'go.mod', 'scripts/credentials.json', 'web/node_modules/private.json'];
    const write = (file, text) => { const p = path.join(base, file); mkdirSync(path.dirname(p), {recursive: true}); writeFileSync(p, text); };
    for (const file of files) write(file, 'synthetic public source');
    const f = {base, write, calls: [], head: 'synthetic-head', status: 'UP', task: 'SUCCESS', analysis: 'analysis-1', latest: 'analysis-1', languages: ['go', 'js', 'ts'], uncovered: 10, token: 'synthetic_token', config: null, stale: false, scannerFailure: false, incomplete: false};
    f.run = (command, args) => {
        f.calls.push({command, args});
        if (command === 'git') return args.includes('ls-files') ? files.join('\0') + '\0' : f.head + '\n';
        if (args[0] === 'inspect') return JSON.stringify([{Config: {Image: serverImage}, State: {Running: true}, NetworkSettings: {Networks: {'fixture-network': {}}, Ports: {'9000/tcp': [{HostIp: '127.0.0.1', HostPort: '9000'}]}}}]);
        assert.equal(command, 'docker'); assert.ok(args.includes(scannerImage)); assert.ok(args.includes('container:' + 'a'.repeat(64)));
        assert.ok(args.includes('type=bind,src=' + path.join(base, '.build/coverage/sonar/source') + ',dst=/usr/src,readonly'));
        assert.ok(!args.join(' ').includes(f.token));
        f.config = readFileSync(path.join(base, '.build/coverage/sonar/scanner.properties'), 'utf8');
        f.write('.build/coverage/sonar/work/report-task.txt', 'ceTaskId=task-1\n');
        if (f.stale) f.write('internal/main.go', 'changed during analysis');
        if (f.scannerFailure) { const error = Error('failed'); error.stdout = 'failure ' + f.token; throw error; }
        return f.incomplete ? 'ERROR: incomplete analysis' : 'Analyzed required source languages; ' + f.token;
    };
    f.request = async (url, options) => {
        const route = url.pathname;
        const value = route === '/api/system/status' ? {status: f.status} : route === '/api/user_tokens/generate' ? {token: f.token} : route === '/api/ce/task' ? {task: {status: f.task, analysisId: f.analysis}} : route === '/api/project_analyses/search' ? {analyses: [{key: f.latest}]} : route === '/api/measures/component' ? {component: {key: 'cosmoformer-coverage', measures: Object.entries({lines_to_cover: 100, uncovered_lines: f.uncovered, conditions_to_cover: 10, uncovered_conditions: 0}).map(([metric, value]) => ({metric, value: String(value)}))}} : route === '/api/components/tree' ? {components: f.languages.map(language => ({language}))} : {};
        return {ok: true, text: async () => JSON.stringify(value)};
    };
    f.options = {base, container: 'a'.repeat(64), run: f.run, request: f.request, sleep: async () => {}};
    f.refresh = () => {
        beginCoverage(base, f.run);
        for (const report of reports) f.write('.build/coverage/' + report, 'fresh profile');
        for (const file of ['web/app/.generated/api.ts', 'web/app/.generated/validators.d.ts', 'web/.react-router/types/route.ts']) f.write(file, 'generated type context');
    };
    f.refresh(); return f;
}

test('prepares only public source, generated type context and fresh reports', t => {
    const f = fixture(t), {snapshot} = prepareSnapshot(f.base, f.run);
    assert.ok(existsSync(path.join(snapshot, 'internal/main.go')));
    assert.ok(existsSync(path.join(snapshot, 'web/app/.generated/api.ts')));
    assert.ok(!existsSync(path.join(snapshot, 'scripts/credentials.json')));
    assert.ok(!existsSync(path.join(snapshot, 'web/node_modules/private.json')));
    assert.ok(!existsSync(path.join(snapshot, '.build')));
    assert.ok(sourceFiles(f.base, f.run).includes('go.mod'));
});
test('rejects stale, empty, missing and changed checkout inputs before scanning', t => {
    const f = fixture(t), profile = path.join(f.base, '.build/coverage/go.out');
    utimesSync(profile, 0, 0); assert.throws(() => prepareSnapshot(f.base, f.run), /stale/);
    f.refresh(); writeFileSync(profile, ''); assert.throws(() => prepareSnapshot(f.base, f.run), /empty/);
    f.refresh(); rmSync(profile); assert.throws(() => prepareSnapshot(f.base, f.run));
    f.refresh(); f.write('internal/main.go', 'changed'); assert.throws(() => prepareSnapshot(f.base, f.run), /Checkout changed/);
    f.refresh(); rmSync(path.join(f.base, 'web/app/.generated/api.ts')); assert.throws(() => prepareSnapshot(f.base, f.run), /Generate/);
});
test('rejects symlink source and generated context without following it', t => {
    const f = fixture(t), file = path.join(f.base, 'internal/main.go');
    rmSync(file); symlinkSync('/etc/passwd', file); assert.throws(() => beginCoverage(f.base, f.run), /regular/);
    rmSync(file); f.write('internal/main.go', 'restored'); f.refresh();
    const generated = path.join(f.base, 'web/app/.generated/api.ts'); rmSync(generated); symlinkSync('/etc/passwd', generated);
    assert.throws(() => prepareSnapshot(f.base, f.run), /Symlink/);
});
test('uses exact completed analysis, validates language coverage and redacts scanner credentials', async t => {
    const f = fixture(t), result = await scanOverall(f.options);
    assert.equal(result.passed, true); assert.equal(result.task, 'task-1'); assert.equal(result.analysis, 'analysis-1');
    assert.ok(!existsSync(path.join(f.base, '.build/coverage/sonar/scanner.env')));
    assert.ok(!readFileSync(path.join(f.base, '.build/coverage/sonar/scanner.log'), 'utf8').includes(f.token));
    assert.match(f.config, /sonar.javascript.lcov.reportPaths=.*1.coverage.*4.coverage/);
    assert.equal(JSON.parse(readFileSync(path.join(f.base, '.build/coverage/overall.json'), 'utf8')).head, 'synthetic-head');
});
test('rejects a HEAD-only change during analysis before publishing coverage', async t => {
    const f = fixture(t), request = f.request;
    f.options.request = async (url, options) => {
        const response = await request(url, options);
        if (url.pathname === '/api/ce/task') f.head = 'different-head';
        return response;
    };
    await assert.rejects(scanOverall(f.options), /Checkout changed during analysis/);
    assert.equal(readFileSync(path.join(f.base, 'internal/main.go'), 'utf8'), 'synthetic public source');
    assert.ok(f.calls.some(({command, args}) => command === 'docker' && args[0] === 'run'));
    assert.ok(!existsSync(path.join(f.base, '.build/coverage/overall.json')));
});
for (const scenario of ['low coverage', 'wrong analysis', 'failed task', 'missing language', 'changed source', 'invalid token']) test(`fails closed for ${scenario}`, async t => {
    const f = fixture(t);
    if (scenario === 'low coverage') f.uncovered = 20;
    if (scenario === 'wrong analysis') f.latest = 'different-analysis';
    if (scenario === 'failed task') f.task = 'FAILED';
    if (scenario === 'missing language') f.languages = ['go', 'js'];
    if (scenario === 'changed source') f.stale = true;
    if (scenario === 'invalid token') f.token = 'bad\ntoken';
    await assert.rejects(scanOverall(f.options));
});
test('refuses unknown containers, wrong images and ambiguous networks', async t => {
    const f = fixture(t);
    await assert.rejects(scanOverall({...f.options, container: ''}), /explicit/);
    for (const info of [{Config: {Image: 'other'}, State: {Running: true}}, {Config: {Image: serverImage}, State: {Running: true}, NetworkSettings: {Networks: {one: {}, two: {}}}}]) {
        await assert.rejects(scanOverall({...f.options, run: (cmd, args) => cmd === 'docker' ? JSON.stringify([info]) : f.run(cmd, args)}));
    }
});
test('API stays on loopback, sends secrets only in protected requests, and rejects redirects or HTTP errors', async () => {
    for (const origin of ['https://example.com', 'http://127.0.0.1/path', 'http://admin:pass@127.0.0.1', 'http://127.0.0.1?query']) assert.throws(() => sonarAPI(origin, () => 'private'));
    let body;
    const api = sonarAPI('http://127.0.0.1:9000', () => 'private', async (url, options) => { assert.equal(options.redirect, 'error'); assert.ok(!url.href.includes('private')); body = options.body; return {ok: true, text: async () => ''}; });
    await api('POST', '/api/example', {password: 'synthetic'}); assert.equal(body.get('password'), 'synthetic');
    await assert.rejects(sonarAPI('http://127.0.0.1:9000', () => 'private', async () => ({ok: false, status: 403}))('GET', '/api/example'), /HTTP 403/);
});
test('polls only until success and fails on a bounded timeout', async () => {
    let count = 0; assert.equal(await waitFor(async () => ++count === 2 && 'finished', async () => {}, 3), 'finished');
    await assert.rejects(waitFor(async () => false, async () => {}, 2), /timed out/);
});

test('rejects an unrelated API port before making credential requests', async t => {
    const f = fixture(t); let requests = 0;
    await assert.rejects(scanOverall({...f.options, origin: 'http://127.0.0.1:9001', request: async () => { requests++; }}), /does not belong/);
    assert.equal(requests, 0);
});
test('scanner errors redact credentials and cannot publish a successful report', async t => {
    const f = fixture(t); f.scannerFailure = true;
    await assert.rejects(scanOverall(f.options), /scanner failed/);
    const log = readFileSync(path.join(f.base, '.build/coverage/sonar/scanner.log'), 'utf8');
    assert.ok(log.includes('[REDACTED]')); assert.ok(!log.includes(f.token));
    assert.ok(!existsSync(path.join(f.base, '.build/coverage/sonar/scanner.env')));
    assert.ok(!existsSync(path.join(f.base, '.build/coverage/overall.json')));
});
test('incomplete parsing or coverage import cannot pass the gate', async t => {
    const f = fixture(t); f.incomplete = true;
    await assert.rejects(scanOverall(f.options), /incomplete/);
    assert.ok(!existsSync(path.join(f.base, '.build/coverage/overall.json')));
});

test('the real subprocess adapter captures stderr before any redaction or logging', () => {
    const stub = mock.method(childProcess, 'execFileSync', (_command, _args, options) => {
        assert.equal(options.stdio, 'pipe');
        return 'scripts/main.mjs\0';
    });
    syncBuiltinESMExports();
    try { assert.deepEqual(sourceFiles('/fixture'), ['scripts/main.mjs']); }
    finally { stub.mock.restore(); syncBuiltinESMExports(); }
});
test('malformed API responses do not echo potential credentials in diagnostic errors', async () => {
    const api = sonarAPI('http://127.0.0.1:9000', () => 'synthetic-password', async () => ({ok: true, text: async () => 'synthetic_sensitive_token'}));
    await assert.rejects(api('POST', '/api/user_tokens/generate'), error => {
        assert.match(error.message, /invalid JSON/);
        assert.ok(!error.message.includes('synthetic_sensitive_token'));
        return true;
    });
});
