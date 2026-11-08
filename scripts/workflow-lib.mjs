import {spawn, spawnSync} from 'node:child_process';
import {existsSync, readFileSync, statSync} from 'node:fs';
import path from 'node:path';

export const root = path.resolve(import.meta.dirname, '..');
export const go = existsSync(path.join(root, '.build/tools/go/bin/go')) ? path.join(root, '.build/tools/go/bin/go') : 'go';
export const docker = ['/usr/local/bin/docker', '/opt/homebrew/bin/docker', '/usr/bin/docker'].find(existsSync);
export const dev = path.join(root, '.build/dev');
export function privateFile(file) {
    const st = statSync(file);
    if (!st.isFile() || st.uid !== process.getuid() || (st.mode & 0o077)) throw Error('Expected an owner-only regular file.');
    return readFileSync(file, 'utf8');
}
export function child(command, args, {env = process.env, label, quiet = false} = {}) {
    const proc = spawn(command, args, {cwd: root, env, detached: true, stdio: ['ignore', 'pipe', 'pipe']});
    let output = '';
    for (const stream of [proc.stdout, proc.stderr]) stream.on('data', chunk => {
        if (quiet) { if (output.length < 65536) output += chunk; }
        else process.stdout.write(label ? `[${label}] ${chunk}` : chunk);
    });
    const done = new Promise(resolve => {
        proc.once('error', () => resolve({code: 1, output: 'Required command could not start.'}));
        proc.once('exit', (code, signal) => resolve({code: code ?? (signal ? 1 : 0), output}));
    });
    let stopping;
    function members() {
        const result = spawnSync('/bin/ps', ['-axo', 'pid=,pgid=,uid=,state='], {encoding: 'utf8'});
        if (result.status !== 0) throw Error('Cannot inspect development child processes.');
        return result.stdout.trim().split('\n').map(line => line.trim().split(/\s+/))
            .filter(([, group, uid, state]) => Number(group) === proc.pid && Number(uid) === process.getuid() && !state.startsWith('Z'))
            .map(([pid]) => Number(pid));
    }
    function send(signal) {
        try { process.kill(-proc.pid, signal); }
        catch (e) {
            if (e.code === 'ESRCH') return;
            if (e.code !== 'EPERM') throw e;
            // Some macOS process policies reject group signaling. Restrict the
            // fallback to this owned process group, never a name/port search.
            for (const pid of members()) {
                try { process.kill(pid, signal); } catch (error) { if (error.code !== 'ESRCH') throw error; }
            }
        }
    }
    return {proc, done, stop({graceMs = 12000} = {}) {
        if (!stopping) stopping = (async () => {
            if (!proc.pid) return;
            send('SIGTERM');
            const deadline = performance.now() + graceMs;
            while (members().length) {
                if (performance.now() >= deadline) { send('SIGKILL'); break; }
                await new Promise(resolve => setTimeout(resolve, 50));
            }
            await done;
            for (let i = 0; i < 40 && members().length; i++) await new Promise(resolve => setTimeout(resolve, 50));
            if (members().length) throw Error('Development child processes could not be stopped; keep the session receipt for recovery.');
        })();
        return stopping;
    }};
}

export async function available(port) {
    const {createServer} = await import('node:net');
    return new Promise(resolve => {
        const server = createServer();
        server.once('error', () => resolve(false));
        server.listen(port, '127.0.0.1', () => server.close(() => resolve(true)));
    });
}
export async function healthy(url) {
    try { return (await fetch(url, {signal: AbortSignal.timeout(1500), redirect: 'error'})).status === 200; }
    catch { return false; }
}
