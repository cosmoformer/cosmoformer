// @vitest-environment node
import { beforeEach, afterEach, expect, it, vi } from 'vitest';
const f = vi.hoisted(() => ({ files: new Map<string, string>(), calls: [] as any[], stops: [] as string[], watchers: [] as any[], signals: new Map<string, () => void>(), busy: false, socket: false, lock: false, fail: '', builds: 0, onWeb: async () => { } }));
vi.mock('node:fs', () => ({
    existsSync: (p: string) => f.files.has(p) || (f.socket && p.endsWith('api.sock')),
    readFileSync: (p: string) => { if (!f.files.has(p))
        throw Error('Missing fixture ' + p); return f.files.get(p)!; },
    mkdirSync: (p: string) => { if (f.lock && p.endsWith('session.lock'))
        throw Error('EEXIST'); },
    writeFileSync: (p: string, v: string) => f.files.set(p, v),
    rmSync: (p: string) => { for (const k of f.files.keys())
        if (k === p || k.startsWith(p + '/'))
            f.files.delete(k); },
    mkdtempSync: (p: string) => p + 'fixture',
    readdirSync: (p: string) => p.endsWith('/nested') ? [{ name: 'migration.sql', isDirectory: () => false, isFile: () => true }] : [
        { name: 'main.go', isDirectory: () => false, isFile: () => true }, { name: 'nested', isDirectory: () => true, isFile: () => false }, { name: 'README.md', isDirectory: () => false, isFile: () => true }
    ],
    watch: (p: string, _options: any, cb?: any) => { const w = { path: p, callback: cb ?? _options, close: vi.fn() }; f.watchers.push(w); return w; },
}));
vi.mock('../../../scripts/workflow-lib.mjs', () => ({ root: '/fixture', dev: '/fixture/.build/dev', go: 'fixture-go', docker: 'fixture-docker', available: async () => !f.busy,
    child: (command: string, args: string[], options: any = {}) => {
        f.calls.push({ command, args, options });
        const label = options.label ?? command;
        let finish!: (v: any) => void;
        let done: Promise<any>;
        if (label === 'api' || label === 'web')
            done = new Promise(resolve => { finish = resolve; });
        else {
            if (label === 'build')
                f.builds++;
            done = Promise.resolve({ code: f.fail === label ? 1 : 0, output: args[0] === 'inspect' ? 'true' : '' });
        }
        if (label === 'test-db' && f.fail !== label) {
            const dir = options.env.COSMOFORMER_TEST_DIR;
            f.files.set(dir + '/owner.dsn', 'synthetic-owner-dsn');
            f.files.set(dir + '/container', '  synthetic-owned-container\n');
            f.files.set(dir + '/container-intent.json', 'synthetic-record');
        }
        if (label === 'web')
            queueMicrotask(() => void f.onWeb());
        return { done, stop: async () => { f.stops.push(label); finish?.({ code: 0, output: '' }); } };
    }
}));
let previousArgs: string[], previousExit: any;
beforeEach(() => {
    vi.resetModules();
    f.files.clear();
    f.calls = [];
    f.stops = [];
    f.watchers = [];
    f.signals.clear();
    Object.assign(f, { busy: false, socket: false, lock: false, fail: '', builds: 0 });
    for (const dir of ['cmd/cosmoformer', 'internal']) {
        f.files.set('/fixture/' + dir + '/main.go', 'package main');
        f.files.set('/fixture/' + dir + '/nested/migration.sql', 'SELECT 1');
    }
    f.files.set('/fixture/go.mod', 'module fixture');
    f.files.set('/fixture/go.sum', 'fixture-checksums');
    f.onWeb = async () => { f.signals.get('SIGINT')!(); };
    previousArgs = process.argv;
    previousExit = process.exitCode;
    process.exitCode = 0;
    vi.spyOn(process, 'on').mockImplementation(((name: string, fn: () => void) => { f.signals.set(name, fn); return process; }) as any);
    vi.spyOn(console, 'log').mockImplementation(() => { });
    vi.spyOn(console, 'error').mockImplementation(() => { });
});
afterEach(() => { process.argv = previousArgs; process.exitCode = previousExit; vi.unstubAllEnvs(); });
async function invoke(action: string) {
    process.argv = ['node', 'workflows.mjs', action];
    // @ts-expect-error Executable JavaScript helper.
    await import('../../../scripts/workflows.mjs');
    return process.exitCode;
}
it('supervises reloads, removes host credential settings and stops only dependencies it started', async () => {
    vi.stubEnv('AWS_SECRET_ACCESS_KEY', 'synthetic-not-a-credential');
    vi.stubEnv('COSMOFORMER_MIGRATION_DSN_FILE', 'synthetic-owner-path');
    vi.stubEnv('COSMOFORMER_DEV_API', 'synthetic-bypass');
    f.onWeb = async () => {
        const w = f.watchers.find(w => w.path === '/fixture/internal');
        w.callback('change', 'README.md');
        w.callback('change', 'main.go');
        await new Promise(resolve => setTimeout(resolve, 230));
        expect(f.builds).toBe(1);
        f.files.set('/fixture/internal/main.go', 'package changed');
        w.callback('change', 'main.go');
        await new Promise(resolve => setTimeout(resolve, 230));
        expect(f.builds).toBe(2);
        f.signals.get('SIGTERM')!();
    };
    expect(await invoke('dev')).toBe(0);
    const api = f.calls.find(c => c.options.label === 'api');
    expect(api.options.env.AWS_SECRET_ACCESS_KEY).toBeUndefined();
    expect(api.options.env.COSMOFORMER_MIGRATION_DSN_FILE).toBeUndefined();
    expect(api.options.env.COSMOFORMER_DEV_API).toBeUndefined();
    expect(api.options.env.COSMOFORMER_LISTEN).toBe('');
    expect(api.options.env.COSMOFORMER_UNIX_SOCKET).toBe('/fixture/.build/dev/api.sock');
    expect(f.calls.filter(c => c.options.label === 'stop').map(c => c.args)).toEqual([['scripts/local-env.mjs', 'stop'], ['scripts/dev.mjs', 'stop']]);
    expect(f.stops).toContain('api');
    expect(f.stops).toContain('web');
    expect(f.watchers.every(w => w.close.mock.calls.length === 1)).toBe(true);
    expect(f.files.has('/fixture/.build/dev/session.lock/pid')).toBe(false);
});
it('preserves already-running storage and database when the frontend exits', async () => {
    f.files.set('/fixture/.build/dev/container', 'recorded-db');
    f.files.set('/fixture/.build/local/dev/environment.json', JSON.stringify({ container: 'recorded-storage' }));
    f.onWeb = async () => { f.signals.get('SIGINT')!(); };
    expect(await invoke('dev')).toBe(0);
    expect(f.calls.filter(c => c.args[0] === 'inspect').map(c => c.args.at(-1))).toEqual(['recorded-db', 'recorded-storage']);
    expect(f.calls.some(c => c.options.label === 'stop')).toBe(false);
});
it.each(['port', 'socket', 'lock', 'build', 'setup'])('reports %s failure and cleans only its own session', async (failure) => {
    f.busy = failure === 'port';
    f.socket = failure === 'socket';
    f.lock = failure === 'lock';
    f.fail = ['build', 'setup'].includes(failure) ? failure : '';
    expect(await invoke('dev')).toBe(1);
    expect(f.calls.some(c => c.options.label === 'web')).toBe(false);
    if (['port', 'socket', 'lock'].includes(failure))
        expect(f.calls).toHaveLength(0);
    if (failure !== 'lock')
        expect(f.files.has('/fixture/.build/dev/session.lock/pid')).toBe(false);
});
it.each(['', 'test-db'])('isolates integration controls and cleans startup receipts after %s', async (failure) => {
    vi.stubEnv('COSMOFORMER_DATABASE_DSN_FILE', 'must-not-use');
    vi.stubEnv('COSMOFORMER_MIGRATION_DSN_FILE', 'must-not-use');
    vi.stubEnv('COSMOFORMER_TEST_OWNER_DSN', 'stale-owner-dsn');
    vi.stubEnv('COSMOFORMER_TEST_MARKER_FILE', 'stale-marker');
    vi.stubEnv('COSMOFORMER_TEST_PG_CONTAINER', 'stale-container');
    vi.stubEnv('TEST_ARGS', "-run 'TestOne|TestTwo'");
    f.fail = failure;
    // Model response loss after the startup intent became durable.
    f.files.set('/fixture/.build/test-runs/run-fixture/container-intent.json', 'intent');
    expect(await invoke('test')).toBe(failure ? 1 : 0);
    const startup = f.calls.find(c => c.options.label === 'test-db');
    expect(startup.options.env.COSMOFORMER_DATABASE_DSN_FILE).toBeUndefined();
    expect(startup.options.env.COSMOFORMER_MIGRATION_DSN_FILE).toBeUndefined();
    const cleanup = f.calls.find(c => c.options.label === 'cleanup');
    expect(cleanup.args).toEqual(['scripts/dev.mjs', 'test-stop']);
    if (!failure) {
        const make = f.calls.find(c => c.command === 'make');
        expect(make.args).toEqual(['test-integration', "TEST_ARGS=-run 'TestOne|TestTwo'"]);
        expect(make.options.env.COSMOFORMER_TEST_OWNER_DSN).toBe('synthetic-owner-dsn');
        expect(make.options.env.COSMOFORMER_TEST_MARKER_FILE).toBe('/fixture/.build/test-runs/run-fixture/marker');
        expect(make.options.env.COSMOFORMER_TEST_PG_CONTAINER).toBe('synthetic-owned-container');
    }
    expect([...f.files.keys()].some(p => p.startsWith('/fixture/.build/test-runs/'))).toBe(false);
});
it('rejects unknown workflow actions without starting commands', async () => { expect(await invoke('unknown')).toBe(1); expect(f.calls).toHaveLength(0); });
