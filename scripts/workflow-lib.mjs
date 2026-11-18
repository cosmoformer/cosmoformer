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
export function child(command, args, {env = process.env, label, quiet = false, captureStdout = false} = {}) {
    const proc = spawn(command, args, {cwd: root, env, detached: true, stdio: ['ignore', 'pipe', 'pipe']});
    let output = '', stdoutBytes = 0, stdoutTruncated = false;
    const stdoutChunks = [];
    if (captureStdout) proc.stdout.on('data', chunk => {
        const remaining = 16 * 1024 * 1024 - stdoutBytes;
        if (chunk.length > remaining) stdoutTruncated = true;
        if (remaining > 0) {
            const bytes = chunk.subarray(0, remaining);
            stdoutChunks.push(bytes); stdoutBytes += bytes.length;
        }
    });
    for (const stream of [proc.stdout, proc.stderr]) stream.on('data', chunk => {
        if (quiet) { if (output.length < 65536) output += chunk; }
        else process.stdout.write(label ? `[${label}] ${chunk}` : chunk);
    });
    const done = new Promise(resolve => {
        proc.once('error', () => resolve({code: 1, output: 'Required command could not start.'}));
        proc.once(captureStdout ? 'close' : 'exit', (code, signal) => resolve({code: code ?? (signal ? 1 : 0), output, stdout: Buffer.concat(stdoutChunks).toString('utf8'), stdoutTruncated}));
    });
    let stopping;
    function members(timeout = 1000) {
        const result = spawnSync('/bin/ps', ['-axo', 'pid=,pgid=,uid=,state='], {encoding: 'utf8', timeout: Math.max(1, Math.ceil(timeout)), killSignal: 'SIGKILL'});
        if (result.status !== 0) throw Error('Cannot inspect development child processes.', {cause: result.error ?? Error(`ps exited ${result.status}`)});
        return result.stdout.trim().split('\n').map(line => line.trim().split(/\s+/))
            .filter(([, group, uid, state]) => Number(group) === proc.pid && Number(uid) === process.getuid() && !state.startsWith('Z'))
            .map(([pid]) => Number(pid));
    }
    function send(signal, timeout = 1000) {
        try { process.kill(-proc.pid, signal); }
        catch (e) {
            if (e.code === 'ESRCH') return;
            if (e.code !== 'EPERM') throw e;
            // Some macOS process policies reject group signaling. Restrict the
            // fallback to this owned process group, never a name/port search.
            let owned;
            try { owned = members(timeout); }
            catch (error) { throw new AggregateError([e, error], `Cannot signal owned process group ${proc.pid}.`); }
            for (const pid of owned) {
                try { process.kill(pid, signal); } catch (error) { if (error.code !== 'ESRCH') throw error; }
            }
        }
    }
    return {proc, done, stop({graceMs = 12000} = {}) {
        if (!stopping) stopping = (async () => {
            if (!proc.pid) return;
            const errors = [];
            let inspectionFailed = false, settled = false;
            // done only resolves; this observer records exit without awaiting it.
            void done.then(() => { settled = true; });
            const inspect = timeout => {
                if (inspectionFailed) return null;
                try { return members(Math.max(1, timeout)); }
                catch (error) { inspectionFailed = true; errors.push(error); return null; }
            };
            const signal = (name, timeout) => {
                try { send(name, Math.max(1, timeout)); }
                catch (error) { errors.push(error); }
            };
            const pause = ms => new Promise(resolve => setTimeout(resolve, ms));
            const deadline = performance.now() + graceMs;
            signal('SIGTERM', Math.min(1000, graceMs));
            let live;
            while (performance.now() < deadline) {
                const remaining = deadline - performance.now();
                // Do not turn the last few milliseconds into a false ps timeout.
                if (remaining >= 100) live = inspect(Math.min(1000, remaining));
                if (settled && live?.length === 0) break;
                await pause(Math.max(1, Math.min(50, deadline - performance.now())));
            }
            // Inspection is diagnostic, never a prerequisite for escalation.
            // Signal the known owned group even if ps is missing or denied.
            if (!settled || live?.length !== 0) signal('SIGKILL', 1000);
            const drainDeadline = performance.now() + 2000;
            while (performance.now() < drainDeadline) {
                const remaining = drainDeadline - performance.now();
                if (remaining >= 100) live = inspect(Math.min(1000, remaining));
                if (settled && (inspectionFailed || live?.length === 0)) break;
                await pause(Math.max(1, Math.min(50, drainDeadline - performance.now())));
            }
            if (!settled || live?.length) errors.push(Error('Owned child exit could not be confirmed within the shutdown deadline.'));
            if (errors.length) {
                // A denied signal or escaped pipe holder must not keep the
                // supervisor alive after its bounded failure has been reported.
                proc.unref(); proc.stdout.destroy(); proc.stderr.destroy();
                throw new AggregateError(errors, `Development child shutdown could not be verified (process group ${proc.pid}); retain this identity for recovery.`);
            }
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
