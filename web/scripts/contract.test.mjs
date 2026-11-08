import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';

test('emits the Go-owned contract and strict browser validators without dynamic compilation', async () => {
    await import('./contract.mjs');
    const root = new URL('../../', import.meta.url);
    const api = JSON.parse(readFileSync(new URL('.build/openapi.json', root), 'utf8'));
    assert.ok(api.paths['/worker/v1/admit'].post);
    assert.ok(api.paths['/api/v1/session/login'].post);
    const validators = await import('../app/.generated/validators.js');
    for (const name of Object.keys(api.components.schemas)) {
        assert.equal(typeof validators[name], 'function', `Missing validator for ${name}`);
    }
    assert.equal(validators.OKBody({ ok: true }), true);
    assert.equal(validators.OKBody({ ok: true, unexpected: 'fixture' }), false);
    assert.equal(validators.AdmitOutputBody({ generation: 1, idle_lease_seconds: 30, heartbeat_seconds: 5 }), true);
    assert.equal(validators.AdmitOutputBody({ generation: Number.MAX_SAFE_INTEGER + 1, idle_lease_seconds: 30, heartbeat_seconds: 5 }), false);
    assert.equal(validators.AdmitOutputBody({ generation: '1', idle_lease_seconds: 30, heartbeat_seconds: 5 }), false);
});
