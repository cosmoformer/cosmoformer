import {test} from 'node:test';
import assert from 'node:assert/strict';
import {spawnSync} from 'node:child_process';
import {runStorageTest} from './local-env.mjs';

// Run the shipped integration suites under Node's coverage runner so their real
// CLI child processes contribute LCOV as well as the ordinary Go test result.
// This target requires Docker and the explicitly marked throwaway test database.
function run(command, args, timeout) {
    const result = spawnSync(command, args, {encoding: 'utf8', timeout, maxBuffer: 8 * 1024 * 1024});
    assert.ifError(result.error);
    assert.equal(result.status, 0, result.stdout + '\n' + result.stderr);
    console.log(result.stdout);
}

test('local storage integration: saved plans, drift, locks and owned-resource cleanup', {timeout: 600000}, () => {
    console.log(runStorageTest());
});

test('seed client: real authentication, idempotent setup and revoked-grant restoration', {timeout: 120000}, () => {
    assert.ok(process.env.COSMOFORMER_TEST_OWNER_DSN, 'Start the isolated test database first');
    assert.ok(process.env.COSMOFORMER_TEST_MARKER_FILE, 'Marked test database required');
    run(process.env.COSMOFORMER_GO ?? 'go', ['test', '-race', '-tags=integration', './tests/acceptance', '-run=TestLocalDevelopmentSeed', '-count=1', '-timeout=2m'], 110000);
});
