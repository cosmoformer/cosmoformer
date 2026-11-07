import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mergeProfiles } from './merge-go-coverage.mjs';

test('unions host and Linux execution without duplicating shared blocks', () => {
    const host = 'mode: atomic\np/shared.go:1.1,3.2 2 7\np/host.go:1.1,2.2 1 1\n';
    const linux = 'mode: atomic\np/shared.go:1.1,3.2 2 0\np/linux.go:1.1,2.2 1 3\n';
    const merged = mergeProfiles([host, linux]);
    assert.equal(merged, mergeProfiles([linux, host, host]));
    assert.equal(merged.split('\n').filter(line => line.startsWith('p/')).length, 3);
    assert.match(merged, /shared.go:1.1,3.2 2 7/);
    assert.match(merged, /linux.go:1.1,2.2 1 3/);
});

test('rejects incompatible or malformed profiles before writing output', () => {
    assert.throws(() => mergeProfiles(['mode: set\np/x.go:1.1,2.2 1 1']));
    assert.throws(() => mergeProfiles(['mode: atomic\ninvalid']));
    assert.throws(() => mergeProfiles(['mode: atomic\np/x.go:1.1,2.2 1 1', 'mode: atomic\np/x.go:1.1,2.2 2 1']));
    assert.throws(() => mergeProfiles(['mode: atomic\n']));
});
