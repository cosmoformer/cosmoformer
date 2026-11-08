import {test} from 'node:test';
import assert from 'node:assert/strict';
import {mkdtempSync, writeFileSync, readFileSync, chmodSync, rmSync} from 'node:fs';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {createServer} from 'node:net';
import {root, child, available, privateFile} from './workflow-lib.mjs';

test('standalone mirror preserves the existing reviewed package metadata bytes', () => {
    const manifest = JSON.parse(readFileSync(path.join(root, 'deploy/worker/runtimes.json'), 'utf8'));
    const pkg = manifest.runtimes.find(r => r.engine === 'opentofu').test_provider_packages.find(p => p.source_addresses[0].endsWith('/aws'));
    assert.equal(readFileSync(path.join(root, 'deploy/local/aws-package.json'), 'utf8'), JSON.stringify(pkg, null, 2));
});

test('private input rejects readable-by-others files', t => {
    const dir = mkdtempSync(path.join(tmpdir(), 'cosmoformer-file-'));
    t.after(() => rmSync(dir, {recursive: true, force: true}));
    const file = path.join(dir, 'password'); writeFileSync(file, 'fixture', {mode: 0o600});
    assert.equal(privateFile(file), 'fixture'); chmodSync(file, 0o644);
    assert.throws(() => privateFile(file), /owner-only/);
});

test('port checks detect occupied loopback listeners', async t => {
    const server = createServer();
    await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
    const port = server.address().port;
    t.after(() => server.close());
    assert.equal(await available(port), false);
    await new Promise(resolve => server.close(resolve));
    assert.equal(await available(port), true);
});

test('supervision reports command failures and terminates a real child', async () => {
    assert.equal((await child(process.execPath, ['-e', 'process.exit(7)'], {quiet: true}).done).code, 7);
    const running = child(process.execPath, ['-e', 'setInterval(()=>{},1000)'], {quiet: true});
    await running.stop();
    assert.notEqual((await running.done).code, 0);
    assert.throws(() => process.kill(running.proc.pid, 0), {code: 'ESRCH'});
});

test('shutdown kills a descendant that outlives its group leader', async t => {
    const dir = mkdtempSync(path.join(tmpdir(), 'cosmoformer-process-'));
    t.after(() => rmSync(dir, {recursive: true, force: true}));
    const file = path.join(dir, 'ready');
    const grandchild = `process.on('SIGTERM',()=>{}); require('node:fs').writeFileSync(${JSON.stringify(file)},String(process.pid)); setInterval(()=>{},1000);`;
    const script = `require('node:child_process').spawn(process.execPath,['-e',${JSON.stringify(grandchild)}],{stdio:'ignore'}); setInterval(()=>{},1000);`;
    const running = child(process.execPath, ['-e', script], {quiet: true});
    t.after(() => running.stop({graceMs: 50}));
    for (let i = 0; i < 100; i++) {
        try { readFileSync(file); break; } catch { await new Promise(resolve => setTimeout(resolve, 20)); }
    }
    const pid = Number(readFileSync(file, 'utf8'));
    await running.stop({graceMs: 100});
    // The OS may briefly retain an orphan as a zombie; it cannot serve work.
    const {spawnSync} = await import('node:child_process');
    const state = spawnSync('/bin/ps', ['-p', String(pid), '-o', 'state='], {encoding: 'utf8'}).stdout.trim();
    assert.ok(state === '' || state.startsWith('Z'));
});
