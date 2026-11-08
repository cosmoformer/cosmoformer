import {test} from 'node:test';
import assert from 'node:assert/strict';
import {createServer} from 'node:http';
import {chmodSync, mkdtempSync, rmSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {LocalClient} from './dev-seed.mjs';
import {localRequest} from './local-api.mjs';

test('local client authenticates through the private socket and refuses public paths', async t => {
    // Keep the path short enough for macOS Unix sockets.
    const dir = mkdtempSync(path.join(tmpdir(), 'cf-api-'));
    const socket = path.join(dir, 'api.sock');
    const calls = [];
    const server = createServer(async (req, res) => {
        const chunks = []; for await (const chunk of req) chunks.push(chunk);
        calls.push({url: req.url, headers: req.headers, body: Buffer.concat(chunks).toString()});
        if (req.url.endsWith('/login')) res.setHeader('Set-Cookie', '__Host-cosmoformer_session=fixture; Secure; Path=/; HttpOnly');
        res.end(JSON.stringify({csrf_token: 'fixture-csrf', actor: {id: 'fixture'}}));
    });
    t.after(async () => { await new Promise(resolve => server.close(resolve)); rmSync(dir, {recursive: true, force: true}); });
    await new Promise(resolve => server.listen(socket, resolve));
    chmodSync(socket, 0o600);
    const client = new LocalClient(socket);
    await client.login('fixture', 'test-only-password'); await client.logout();
    assert.equal(calls.length, 3);
    assert.equal(JSON.parse(calls[0].body).password, 'test-only-password');
    assert.equal(calls[2].headers['x-csrf-token'], 'fixture-csrf');
    assert.match(calls[2].headers.cookie, /__Host-cosmoformer_session=fixture/);
    await client.call('DELETE', '/fixture', {});
    assert.equal(calls[3].body, '{}', 'DELETE bodies must be framed before reusing the connection');
    assert.throws(() => new LocalClient('http://127.0.0.1:18080'), /absolute private socket/);
    chmodSync(socket, 0o666);
    assert.throws(() => new LocalClient(socket), /private directory/);
    chmodSync(socket, 0o600); chmodSync(dir, 0o755);
    assert.throws(() => new LocalClient(socket), /private directory/);
    chmodSync(dir, 0o700);
    const regular = path.join(dir, 'file'); writeFileSync(regular, '', {mode: 0o600});
    assert.throws(() => new LocalClient(regular), /private directory/);
    assert.equal(calls.length, 4, 'rejected endpoints receive no credentials');
});

test('local request rejects oversized responses', async t => {
    const dir = mkdtempSync(path.join(tmpdir(), 'cf-api-'));
    const socket = path.join(dir, 'api.sock');
    const server = createServer((_, res) => res.end(Buffer.alloc(1024 * 1024 + 1)));
    t.after(async () => { await new Promise(resolve => server.close(resolve)); rmSync(dir, {recursive: true, force: true}); });
    await new Promise(resolve => server.listen(socket, resolve)); chmodSync(socket, 0o600);
    await assert.rejects(localRequest(socket, 'GET', '/'), /response limit/);
});
