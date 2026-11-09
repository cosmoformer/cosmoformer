// @vitest-environment node
import { beforeEach, afterEach, it, expect, vi } from 'vitest';
const f = vi.hoisted(() => ({ files: new Map<string, string>(), calls: [] as any[], busy: false, status: 200, expired: false, fail: '', pid: true }));
vi.mock('node:fs', () => ({ existsSync: (p: string) => f.files.has(p), readFileSync: (p: string) => { if (!f.files.has(p))
        throw Error('missing'); return f.files.get(p); } }));
vi.mock('node:crypto', () => ({ createPrivateKey: () => ({}), X509Certificate: class {
        validTo = f.expired ? '2000-01-01' : '2999-01-01';
        validFrom = '2000-01-01';
        checkHost(h: string) { return h === 'localhost'; }
        checkPrivateKey() { return true; }
    } }));
vi.mock('../../../scripts/workflow-lib.mjs', () => ({ root: '/fixture', dev: '/fixture/.build/dev', go: 'go', docker: 'docker', available: async () => !f.busy, privateFile: (p: string) => { if (!f.files.has(p))
        throw Error('missing private file'); return f.files.get(p); },
    child: (command: string, args: string[]) => { f.calls.push({ command, args }); const output = args[0] === 'version' ? 'go version go1.27.3 fixture' : args[0] === 'inspect' ? 'dev' : args[0] === 'exec' ? '7' : args[0] === 'port' ? '127.0.0.1:54329' : 'fixture-version'; return { done: Promise.resolve({ code: f.fail === command ? 1 : 0, output }) }; } }));
vi.mock('../../../scripts/local-api.mjs', () => ({ apiSocket: '/fixture/.build/dev/api.sock', localRequest: async () => ({ status: f.status }) }));
let previousExit: any;
beforeEach(() => {
    vi.resetModules();
    f.files.clear();
    f.calls = [];
    Object.assign(f, { busy: false, status: 200, expired: false, fail: '', pid: true });
    previousExit = process.exitCode;
    process.exitCode = 0;
    for (const [p, v] of Object.entries({ 'web/node_modules/.package-lock.json': 'fixture', 'internal/store/store.go': 'const SchemaVersion int32 = 7', '.build/dev/cert.pem': 'synthetic-certificate', '.build/dev/key.pem': 'synthetic-key', '.build/dev/container': 'recorded-container' }))
        f.files.set('/fixture/' + p, v);
    vi.spyOn(console, 'log').mockImplementation(() => { });
    vi.spyOn(process, 'kill').mockImplementation((() => { if (!f.pid)
        throw Error('ESRCH'); return true; }) as any);
});
afterEach(() => { process.exitCode = previousExit; });
async function doctor() {
    // @ts-expect-error Executable JavaScript helper.
    await import('../../../scripts/doctor.mjs');
    return process.exitCode;
}
it('verifies toolchain, certificate, schema, storage and socket readiness without mutations', async () => {
    f.files.set('/fixture/.build/dev/api.sock', 'socket');
    expect(await doctor()).toBe(0);
    expect(f.calls.some(c => ['run', 'start', 'stop', 'rm'].includes(c.args[0]))).toBe(false);
    expect(f.calls.find(c => c.args[0] === 'port').args.at(-1)).toBe('5432/tcp');
});
it('reports missing tools, expired certificate, failed readiness and conflicting ports', async () => {
    f.files.delete('/fixture/web/node_modules/.package-lock.json');
    f.files.set('/fixture/.build/dev/api.sock', 'socket');
    f.expired = true;
    f.busy = true;
    f.status = 503;
    f.fail = 'docker';
    expect(await doctor()).toBe(1);
    const output = vi.mocked(console.log).mock.calls.flat().join('\n');
    expect(output).toContain('Development certificate and key');
    expect(output).toContain('Private API socket');
    expect(output).toMatch(/\d+ check\(s\) need attention/);
});
it.each(['123', 'invalid'])('checks recorded session PID %s and recognizes supervised port ownership', async (pid) => {
    f.files.set('/fixture/.build/dev/session.lock', 'directory');
    f.files.set('/fixture/.build/dev/session.lock/pid', pid);
    f.busy = true;
    f.pid = false;
    expect(await doctor()).toBe(1);
    expect(vi.mocked(console.log).mock.calls.flat().join('\n')).toContain('FIX   Development session lock');
});
