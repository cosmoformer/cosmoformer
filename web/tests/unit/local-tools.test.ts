// @vitest-environment node
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import path from 'node:path';
import { createHash } from 'node:crypto';

const fixture = vi.hoisted(() => ({
    files: new Map<string, string | Buffer>(), modes: new Map<string, number>(),
    calls: [] as { executable: string; args: string[]; options: any }[],
    label: 'test', exists: false, ready: true, tools: true,
    failure: null as null | ((args: string[]) => boolean),
}));
vi.mock('node:fs', () => ({
    mkdirSync: vi.fn(),
    existsSync: (file: string) => fixture.files.has(file) || fixture.tools && ['/usr/local/bin/docker', '/usr/local/go/bin/go'].includes(file),
    writeFileSync: (file: string, value: string | Buffer, options?: { mode?: number }) => {
        // Node's mode option applies only when creating a file. Existing files
        // retain their permissions until chmod explicitly repairs them.
        if (!fixture.files.has(file)) fixture.modes.set(file, options?.mode ?? 0o666);
        fixture.files.set(file, value);
    },
    readFileSync: (file: string, encoding?: string) => { if (!fixture.files.has(file)) throw new Error('Missing fixture file'); const value = fixture.files.get(file)!; return encoding ? value.toString() : Buffer.from(value); },
    chmodSync: (file: string, mode: number) => fixture.modes.set(file, mode),
    rmSync: (file: string) => fixture.files.delete(file),
}));
vi.mock('node:child_process', () => ({
    spawnSync: (executable: string, args: string[], options: any) => {
        fixture.calls.push({ executable, args, options });
        if (fixture.failure?.(args)) return { status: 1, stderr: 'synthetic command failure', stdout: '' };
        if (args[0] === 'inspect') return { status: args.includes('--format') || fixture.exists ? 0 : 1, stdout: fixture.label, stderr: '' };
        if (args.includes('pg_isready')) return { status: fixture.ready ? 0 : 1, stdout: '', stderr: '' };
        if (args[0] === 'run' && args[1] === '--rm') return { status: 0, stdout: 'synthetic-test-container', stderr: '' };
        if (args[0] === 'run' && args[1] === '-d') return { status: 0, stdout: 'synthetic-dev-container', stderr: '' };
        if (args[0] === 'port') return { status: 0, stdout: '127.0.0.1:55432', stderr: '' };
        if (executable === '/usr/bin/openssl') {
            fixture.files.set(args[args.indexOf('-keyout') + 1], 'synthetic-private-key');
            fixture.files.set(args[args.indexOf('-out') + 1], 'synthetic-certificate');
            return { status: 0, stdout: '', stderr: '' };
        }
        if (executable === '/usr/local/go/bin/go' && args[0] === 'run' && args[1] === './cmd/cosmoformer' && ['migrate', 'bootstrap-admin'].includes(args[2])) return { status: 0, stdout: 'Synthetic command completed', stderr: '' };
        if (executable === '/usr/local/bin/docker' && (['start', 'stop'].includes(args[0]) || args[0] === 'exec' && args.includes('psql') && options?.input)) return { status: 0, stdout: '', stderr: '' };
        throw new Error(`Unexpected fixture command: ${executable} ${args[0]}`);
    },
    execFileSync: (executable: string, args: string[], options: any) => { fixture.calls.push({ executable, args, options }); return Buffer.alloc(0); },
}));

const root = path.resolve(import.meta.dirname, '../../..');
const testDir = path.join(root, '.build/testdb');
const devDir = path.join(root, '.build/dev');

beforeEach(() => {
    vi.resetModules(); fixture.files.clear(); fixture.modes.clear(); fixture.calls.length = 0;
    Object.assign(fixture, { label: 'test', exists: false, ready: true, tools: true, failure: null });
    vi.spyOn(process, 'chdir').mockImplementation(() => {});
    vi.spyOn(console, 'log').mockImplementation(() => {});
    vi.spyOn(console, 'error').mockImplementation(() => {});
    vi.spyOn(Atomics, 'wait').mockReturnValue('timed-out');
});
afterEach(() => vi.unstubAllEnvs());

async function dev(action: string, ...args: string[]) {
    const previous = process.argv, previousExit = process.exitCode;
    process.argv = ['node', 'dev.mjs', action, ...args]; process.exitCode = 0;
    try {
        // @ts-expect-error The executable JavaScript helper has no declaration file.
        await import('../../../scripts/dev.mjs');
        return process.exitCode;
    } finally { process.argv = previous; process.exitCode = previousExit; }
}

describe('isolated database and local setup safeguards', () => {
    it('marks a loopback-only throwaway database and protects its control files', async () => {
        expect(await dev('test-start')).toBe(0);
        const run = fixture.calls.find(c => c.args[0] === 'run')!;
        expect(run.args).toContain('cosmoformer.local=test');
        expect(run.args).toContain('127.0.0.1::5432');
        expect(run.args.at(-1)).toMatch(/^postgres:.*@sha256:/);
        expect(fixture.files.get(path.join(testDir, 'owner.dsn'))).toBe('postgres://postgres@127.0.0.1:55432/cosmoformer_test_control?sslmode=disable');
        expect(fixture.files.get(path.join(testDir, 'marker'))).toMatch(/^[a-f0-9]{64}$/);
        for (const file of fixture.files.keys()) expect(fixture.modes.get(file)).toBe(0o600);
        expect(fixture.calls.some(c => c.options?.input?.includes('NOBYPASSRLS'))).toBe(true);
    });
    it('refuses an already recorded test database without starting another', async () => {
        fixture.files.set(path.join(testDir, 'container'), 'existing'); fixture.exists = true;
        expect(await dev('test-start')).toBe(1);
        expect(fixture.calls.some(c => c.args[0] === 'run')).toBe(false);
    });
    it('re-protects existing control files when replacing their contents', async () => {
        for (const name of ['owner.dsn', 'marker']) {
            const file = path.join(testDir, name);
            fixture.files.set(file, 'stale synthetic fixture'); fixture.modes.set(file, 0o644);
        }
        expect(await dev('test-start')).toBe(0);
        for (const name of ['owner.dsn', 'marker']) expect(fixture.modes.get(path.join(testDir, name))).toBe(0o600);
    });
    it('refuses to stop a container without the test label', async () => {
        fixture.files.set(path.join(testDir, 'container'), 'foreign'); fixture.label = 'dev';
        expect(await dev('test-stop')).toBe(1);
        expect(fixture.calls.some(c => c.args[0] === 'stop')).toBe(false);
        expect(fixture.files.get(path.join(testDir, 'container'))).toBe('foreign');
    });
    it('stops only the recorded test container and removes its receipt', async () => {
        fixture.files.set(path.join(testDir, 'container'), 'synthetic-test-container');
        expect(await dev('test-stop')).toBe(0);
        expect(fixture.calls.find(c => c.args[0] === 'stop')?.args).toEqual(['stop', 'synthetic-test-container']);
        expect(fixture.files.has(path.join(testDir, 'container'))).toBe(false);
        vi.resetModules(); expect(await dev('test-stop')).toBe(0);
    });
    it('fails on unavailable tools, failed commands and an unready database', async () => {
        fixture.tools = false; expect(await dev('test-start')).toBe(1);
        vi.resetModules(); fixture.tools = true; fixture.failure = args => args[0] === 'run'; expect(await dev('test-start')).toBe(1);
        vi.resetModules(); fixture.failure = null; fixture.ready = false; expect(await dev('test-start')).toBe(1);
        expect(fixture.calls.filter(c => c.args.includes('pg_isready'))).toHaveLength(60);
    });
    it('requires CI context and a bounded service identifier before initialization', async () => {
        vi.stubEnv('CI', 'false'); expect(await dev('ci-test-init', 'a'.repeat(12))).toBe(1);
        vi.resetModules(); vi.stubEnv('CI', 'true'); expect(await dev('ci-test-init', 'foreign/service')).toBe(1);
        expect(fixture.calls).toHaveLength(0);
        vi.resetModules(); expect(await dev('ci-test-init', 'a'.repeat(12))).toBe(0);
        expect(fixture.files.get(path.join(testDir, 'marker'))).toMatch(/^[a-f0-9]{64}$/);
    });
    it('creates private development credentials and bootstraps only once', async () => {
        expect(await dev('setup')).toBe(0);
        expect(fixture.calls.some(c => c.args.includes('bootstrap-admin'))).toBe(true);
        expect(fixture.modes.get(path.join(devDir, 'key.pem'))).toBe(0o600);
        const saved = fixture.files.get(path.join(devDir, 'db.env'));
        fixture.calls.length = 0; vi.resetModules(); expect(await dev('setup')).toBe(0);
        expect(fixture.files.get(path.join(devDir, 'db.env'))).toBe(saved);
        expect(fixture.calls.some(c => c.args.includes('bootstrap-admin'))).toBe(false);
        expect(fixture.calls.find(c => c.args[0] === 'start')?.args[1]).toBe('synthetic-dev-container');
        vi.resetModules(); expect(await dev('stop')).toBe(0);
        expect(fixture.files.has(path.join(devDir, 'container'))).toBe(true);
    });
    it('protects generated certificates and rejects unknown commands', async () => {
        expect(await dev('cert')).toBe(0); expect(fixture.modes.get(path.join(devDir, 'key.pem'))).toBe(0o600);
        fixture.calls.length = 0; vi.resetModules(); expect(await dev('cert')).toBe(0); expect(fixture.calls).toHaveLength(0);
        vi.resetModules(); expect(await dev('unsupported')).toBe(1);
    });
});

describe('checksum-verified optional tool installation', () => {
    const archive = Buffer.from('synthetic archive bytes');
    const digest = createHash('sha256').update(archive).digest('hex');
    const os = process.platform, arch = process.arch === 'x64' ? 'amd64' : process.arch;
    const nodeName = `node-v26.13.0-${os}-${process.arch}`;
    function downloads(mode = 'valid') {
        return vi.fn(async (url: string) => {
            let body: Buffer = archive;
            if (url === 'https://go.dev/dl/?mode=json') body = Buffer.from(JSON.stringify(mode === 'missing release' ? [] : [{ version: 'go1.27.3', files: [{ os, arch, kind: 'archive', filename: 'synthetic-go.tar.gz', sha256: mode === 'bad checksum' ? '0'.repeat(64) : digest }] }]));
            else if (url.endsWith('SHASUMS256.txt')) body = Buffer.from(mode === 'missing Node checksum' ? '' : `${digest}  ${nodeName}.tar.gz\n`);
            else if (!['https://go.dev/dl/synthetic-go.tar.gz', `https://nodejs.org/dist/v26.13.0/${nodeName}.tar.gz`].includes(url)) throw new Error('Unexpected test download');
            return { ok: mode !== 'download failed', status: mode === 'download failed' ? 503 : 200, arrayBuffer: async () => body };
        });
    }
    async function tools() {
        // @ts-expect-error The executable JavaScript helper has no declaration file.
        await import('../../../scripts/tools.mjs');
    }
    it('verifies both downloaded archives before invoking extraction', async () => {
        vi.stubGlobal('fetch', downloads()); await tools();
        expect(fixture.calls).toHaveLength(2);
        for (const call of fixture.calls) { expect(call.executable).toBe('/usr/bin/tar'); expect(call.args[0]).toBe('-xzf'); }
        expect([...fixture.files.keys()].some(file => file.endsWith('.tar.gz'))).toBe(false);
    });
    it.each(['missing release', 'bad checksum', 'download failed', 'missing Node checksum'])('refuses %s without extracting an unverified archive', async mode => {
        vi.stubGlobal('fetch', downloads(mode)); await expect(tools()).rejects.toThrow();
        expect(fixture.calls).toHaveLength(mode === 'missing Node checksum' ? 1 : 0);
    });
    it('reuses approved cached tools without downloading', async () => {
        fixture.files.set(path.join(root, '.build/tools/go/bin/go'), 'synthetic');
        fixture.files.set(path.join(root, '.build/tools', nodeName, 'bin/node'), 'synthetic');
        const fetch = downloads(); vi.stubGlobal('fetch', fetch); await tools();
        expect(fetch).not.toHaveBeenCalled(); expect(fixture.calls).toHaveLength(0);
    });
});
