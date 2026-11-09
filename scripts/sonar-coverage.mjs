import {execFileSync} from 'node:child_process';
import {createHash, randomBytes} from 'node:crypto';
import {copyFileSync, existsSync, lstatSync, mkdirSync, openSync, closeSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync} from 'node:fs';
import path from 'node:path';
import {assessOverallCoverage} from './overall-coverage.mjs';

export const serverImage = 'sonarqube:26.11.0.134175-community@sha256:1ce197471f5f2bac347694a336862fd2c23e9f57c6edb46acebbfbe70f2c7d9c';
export const scannerImage = 'sonarsource/sonar-scanner-cli:11@sha256:46341f2c2dc560381414e90347885f5cddc716db080adcbafc1cac13e1473094';
export const reports = ['go.out', 'web/lcov.info', 'contract/lcov.info', 'workflows/lcov.info', 'runtime/lcov.info'];
const root = path.resolve(import.meta.dirname, '..');
const coverage = base => path.join(base, '.build/coverage');
const execute = (command, args) => execFileSync(command, args, {encoding: 'utf8', stdio: 'pipe', maxBuffer: 16 * 1024 * 1024});
const pause = ms => new Promise(resolve => setTimeout(resolve, ms));
const allowed = file => /^(cmd\/|internal\/|scripts\/|tests\/|web\/(app\/|scripts\/|tests\/|tsconfig\.json$|package\.json$)|go\.(mod|sum)$)/.test(file) &&
    !file.split('/').some(p => p.startsWith('.') || p === 'node_modules' || /^(credentials(\.(json|ya?ml))?|admin-password)$/.test(p)) && /\.(go|mjs|ts|tsx|css|json|sql)$|^go\.(mod|sum)$/.test(file);
export function sourceFiles(base, run = execute) {
    return run('git', ['-C', base, 'ls-files', '-z', '--cached', '--others', '--exclude-standard']).split('\0').filter(allowed).sort();
}
function sourceDigest(base, files) {
    const hash = createHash('sha256');
    for (const file of files) {
        const source = path.join(base, file);
        if (!lstatSync(source).isFile()) throw Error('Only regular source files may be scanned');
        hash.update(file); hash.update('\0'); hash.update(createHash('sha256').update(readFileSync(source)).digest());
    }
    return hash.digest('hex');
}
export function beginCoverage(base = root, run = execute) {
    const dir = coverage(base); mkdirSync(dir, {recursive: true});
    const receipt = {started: Date.now(), head: run('git', ['-C', base, 'rev-parse', 'HEAD']).trim(), digest: sourceDigest(base, sourceFiles(base, run))};
    for (const file of [...reports, 'overall.json']) rmSync(path.join(dir, file), {force: true});
    writeFileSync(path.join(dir, 'started.json'), JSON.stringify(receipt));
    return receipt;
}
export function prepareSnapshot(base, run = execute) {
    const dir = coverage(base), receipt = JSON.parse(readFileSync(path.join(dir, 'started.json'), 'utf8'));
    const files = sourceFiles(base, run), digest = sourceDigest(base, files);
    if (digest !== receipt.digest || run('git', ['-C', base, 'rev-parse', 'HEAD']).trim() !== receipt.head) throw Error('Checkout changed since coverage collection began');
    for (const file of reports) {
        const status = statSync(path.join(dir, file));
        if (!status.isFile() || !status.size || status.mtimeMs < receipt.started) throw Error('Missing, empty or stale coverage: ' + file);
    }
    const snapshot = path.join(dir, 'sonar/source'); rmSync(snapshot, {recursive: true, force: true}); mkdirSync(snapshot, {recursive: true});
    const copy = (source, destination) => {
        const st = lstatSync(source);
        if (st.isSymbolicLink()) throw Error('Symlinks are not scan inputs');
        if (st.isDirectory()) { for (const entry of readdirSync(source)) copy(path.join(source, entry), path.join(destination, entry)); }
        else if (st.isFile()) { mkdirSync(path.dirname(destination), {recursive: true}); copyFileSync(source, destination); }
        else throw Error('Only regular scan inputs are supported');
    };
    for (const file of files) copy(path.join(base, file), path.join(snapshot, file));
    for (const file of ['web/app/.generated/api.ts', 'web/app/.generated/validators.d.ts', 'web/.react-router/types']) {
        if (!existsSync(path.join(base, file))) throw Error('Generate the frontend contract and types before scanning');
        copy(path.join(base, file), path.join(snapshot, file));
    }
    for (const [index, file] of reports.entries()) copy(path.join(dir, file), path.join(snapshot, `.sonar-inputs/${index}.coverage`));
    return {snapshot, receipt, digest};
}
export function sonarAPI(origin, credentials, request = fetch) {
    const url = new URL(origin);
    if (url.protocol !== 'http:' || url.hostname !== '127.0.0.1' || url.username || url.password || url.pathname !== '/' || url.search || url.hash) throw Error('Sonar API must use an explicit loopback HTTP origin');
    return async (method, route, params) => {
        const target = new URL(route, url), options = {method, redirect: 'error', signal: AbortSignal.timeout(30000), headers: {Authorization: 'Basic ' + Buffer.from('admin:' + credentials()).toString('base64')}};
        if (params) {
            if (method === 'GET') target.search = new URLSearchParams(params).toString();
            else { options.body = new URLSearchParams(params); }
        }
        const response = await request(target, options);
        if (!response.ok) throw Error(`Sonar API ${route} returned HTTP ${response.status}`);
        try { return JSON.parse(await response.text() || '{}'); }
        catch { throw Error(`Sonar API ${route} returned invalid JSON`); }
    };
}
export async function waitFor(check, sleep = pause, attempts = 180) {
    for (let i = 0; i < attempts; i++) { const result = await check(); if (result) return result; await sleep(1000); }
    throw Error('Sonar analysis timed out');
}
export async function scanOverall({base = root, container, origin = 'http://127.0.0.1:9000', run = execute, request = fetch, sleep = pause} = {}) {
    if (!/^[a-f0-9]{12,64}$/.test(container ?? '')) throw Error('An explicit disposable Sonar container ID is required');
    const info = JSON.parse(run('docker', ['inspect', container]))[0];
    if (info.Config.Image !== serverImage || !info.State.Running) throw Error('Expected the pinned disposable Sonar server');
    const networks = Object.keys(info.NetworkSettings.Networks);
    const binding = info.NetworkSettings.Ports?.['9000/tcp']?.find(p => p.HostIp === '127.0.0.1');
    if (!binding || new URL(origin).port !== binding.HostPort) throw Error('Sonar origin does not belong to the disposable container');
    if (networks.length !== 1) throw Error('Expected a single isolated Sonar network');
    const {snapshot, receipt, digest} = prepareSnapshot(base, run), dir = path.join(coverage(base), 'sonar');
    let password = 'admin'; const api = sonarAPI(origin, () => password, request);
    await waitFor(async () => { try { return (await api('GET', '/api/system/status')).status === 'UP'; } catch { return false; } }, sleep);
    const nextPassword = 'Cf1-' + randomBytes(36).toString('base64url');
    await api('POST', '/api/users/change_password', {login: 'admin', previousPassword: password, password: nextPassword}); password = nextPassword;
    const project = 'cosmoformer-coverage';
    // Creating a project must succeed: never adopt another run's analysis.
    await api('POST', '/api/projects/create', {project, name: project});
    const {token} = await api('POST', '/api/user_tokens/generate', {name: 'coverage', type: 'PROJECT_ANALYSIS_TOKEN', projectKey: project});
    if (typeof token !== 'string' || !/^[A-Za-z0-9_-]{1,256}$/.test(token)) throw Error('Missing scanner token');
    const envFile = path.join(dir, 'scanner.env'), config = path.join(dir, 'scanner.properties'), work = path.join(dir, 'work');
    mkdirSync(work, {recursive: true});
    const properties = {
        'sonar.projectKey': project, 'sonar.projectVersion': digest, 'sonar.host.url': 'http://127.0.0.1:9000',
        'sonar.projectBaseDir': '/usr/src', 'sonar.sources': 'cmd,internal,scripts,web/app,web/scripts', 'sonar.tests': 'cmd,internal,scripts,web,tests',
        'sonar.test.inclusions': '**/*_test.go,**/*.test.*,**/*.spec.*,tests/**,**/tests/**,**/testsupport/**',
        'sonar.exclusions': '**/.generated/**,**/.react-router/**,**/.sonar-inputs/**',
        'sonar.scm.disabled': 'true', 'sonar.typescript.tsconfigPaths': 'web/tsconfig.json',
        'sonar.go.coverage.reportPaths': '/usr/src/.sonar-inputs/0.coverage',
        'sonar.javascript.lcov.reportPaths': reports.slice(1).map((_, i) => `/usr/src/.sonar-inputs/${i + 1}.coverage`).join(','),
        'sonar.working.directory': '/scan-work', 'sonar.scanner.metadataFilePath': '/scan-work/report-task.txt',
    };
    // tests/ can contain acceptance-only fixtures without being an analysis source.
    mkdirSync(path.join(snapshot, 'tests'), {recursive: true});
    writeFileSync(config, Object.entries(properties).map(([k, v]) => `${k}=${v}\n`).join(''));
    const envFD = openSync(envFile, 'wx', 0o600);
    try { writeFileSync(envFD, `SONAR_TOKEN=${token}\nSONAR_USER_HOME=/tmp/scanner-home\n`); } finally { closeSync(envFD); }
    let log = '';
    try {
        log = run('docker', ['run', '--rm', '--platform', 'linux/amd64', '--entrypoint', '/opt/sonar-scanner/bin/sonar-scanner', '--network', 'container:' + container, '--user', `${process.getuid()}:${process.getgid()}`, '--env-file', envFile,
            '--mount', `type=bind,src=${snapshot},dst=/usr/src,readonly`, '--mount', `type=bind,src=${config},dst=/scan-config.properties,readonly`, '--mount', `type=bind,src=${work},dst=/scan-work`, scannerImage, '-Dproject.settings=/scan-config.properties']);
    } catch (error) { log = String(error.stdout ?? '') + String(error.stderr ?? ''); throw Error('Sonar scanner failed'); }
    finally { rmSync(envFile, {force: true}); writeFileSync(path.join(dir, 'scanner.log'), log.replaceAll(token, '[REDACTED]')); }
    if (/Failed to create program|not part of any tsconfig|Failed to parse|ERROR:|could not resolve|unresolved path/i.test(log)) throw Error('Sonar analysis or coverage import was incomplete');
    const task = Object.fromEntries(readFileSync(path.join(work, 'report-task.txt'), 'utf8').trim().split('\n').map(line => { const i = line.indexOf('='); return [line.slice(0, i), line.slice(i + 1)]; }));
    if (!task.ceTaskId) throw Error('Missing exact analysis task');
    const result = await waitFor(async () => {
        const {task: current} = await api('GET', '/api/ce/task', {id: task.ceTaskId});
        if (['FAILED', 'CANCELED'].includes(current.status)) throw Error('Sonar task failed');
        return current.status === 'SUCCESS' && current.analysisId;
    }, sleep, 600);
    const latest = (await api('GET', '/api/project_analyses/search', {project, ps: '1'})).analyses?.[0]?.key;
    if (latest !== result) throw Error('Latest analysis does not match this scan');
    const metricKeys = 'coverage,line_coverage,lines_to_cover,uncovered_lines,conditions_to_cover,uncovered_conditions';
    const {component} = await api('GET', '/api/measures/component', {component: project, metricKeys});
    if (component.key !== project) throw Error('Coverage belongs to another project');
    const metrics = assessOverallCoverage(component.measures);
    const languages = new Set((await api('GET', '/api/components/tree', {component: project, qualifiers: 'FIL', ps: '500'})).components.map(x => x.language));
    if (!['go', 'js', 'ts'].every(x => languages.has(x))) throw Error('Required source languages were not analyzed');
    if (sourceDigest(base, sourceFiles(base, run)) !== digest || run('git', ['-C', base, 'rev-parse', 'HEAD']).trim() !== receipt.head) throw Error('Checkout changed during analysis');
    const report = {...metrics, head: receipt.head, digest, task: task.ceTaskId, analysis: result};
    writeFileSync(path.join(coverage(base), 'overall.json'), JSON.stringify(report, null, 2) + '\n');
    console.log(`Overall combined coverage: ${metrics.combined.percent}% (${metrics.combined.covered}/${metrics.combined.total}); lines: ${metrics.lines.percent}% (${metrics.lines.covered}/${metrics.lines.total}); both require >80%.`);
    if (!metrics.passed) throw Error('Overall coverage must be strictly greater than 80% for both combined and line coverage');
    return report;
}
if (import.meta.main) {
    try {
        if (process.argv[2] === 'begin') beginCoverage();
        else if (process.argv[2] === 'check') await scanOverall({container: process.env.COSMOFORMER_SONAR_CONTAINER, origin: process.env.COSMOFORMER_SONAR_ORIGIN});
        else throw Error('Use begin or check');
    } catch (error) { console.error(error.message); process.exitCode = 1; }
}
