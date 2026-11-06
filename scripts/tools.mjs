// Optional project-local tool install. No global package-manager changes.
import { mkdirSync, existsSync, writeFileSync, rmSync } from 'node:fs';
import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import path from 'node:path';
const root = path.resolve(import.meta.dirname, '..'), dir = path.join(root, '.build/tools');
mkdirSync(dir, { recursive: true });
const os = { darwin: 'darwin', linux: 'linux' }[process.platform], arch = { arm64: 'arm64', x64: 'amd64' }[process.arch];
if (!os || !arch)
    throw new Error('Automatic local tool installation supports macOS/Linux arm64/amd64. Install Go 1.27.3 and Node 26.13.0 manually on this host.');
async function get(url) { const r = await fetch(url); if (!r.ok)
    throw new Error(`Tool download failed (${r.status})`); return Buffer.from(await r.arrayBuffer()); }
async function unpack(url, digest, name) { const b = await get(url); if (createHash('sha256').update(b).digest('hex') !== digest)
    throw new Error('Upstream checksum mismatch'); const file = path.join(dir, name); writeFileSync(file, b); execFileSync('/usr/bin/tar', ['-xzf', file, '-C', dir]); rmSync(file); }
if (!existsSync(path.join(dir, 'go/bin/go'))) {
    const releases = JSON.parse(await get('https://go.dev/dl/?mode=json'));
    const file = releases.find(r => r.version === 'go1.27.3')?.files.find(f => f.os === os && f.arch === arch && f.kind === 'archive');
    if (!file)
        throw new Error('Approved Go release not available');
    await unpack('https://go.dev/dl/' + file.filename, file.sha256, file.filename);
}
const nodeArch = arch === 'amd64' ? 'x64' : arch, name = `node-v26.13.0-${os}-${nodeArch}`;
if (!existsSync(path.join(dir, name, 'bin/node'))) {
    const file = name + '.tar.gz', base = 'https://nodejs.org/dist/v26.13.0/';
    const sums = (await get(base + 'SHASUMS256.txt')).toString();
    const line = sums.split('\n').find(l => l.trim().split(/\s+/).at(-1) === file);
    if (!line)
        throw new Error('Approved Node checksum not found');
    await unpack(base + file, line.split(/\s+/)[0], file);
}
console.log('Checksum-verified local tools ready in .build/tools.');
