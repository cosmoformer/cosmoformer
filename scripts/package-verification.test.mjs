import {test} from 'node:test';
import assert from 'node:assert/strict';
import {spawnSync} from 'node:child_process';
import path from 'node:path';
import {buildPackages} from './package-build.mjs';
import {verifyReproducibility} from './package-reproducibility.mjs';

// Actual build/container/browser execution under the existing Node coverage
// runner. Child Node supervisors inherit the runner's coverage collection.
test('packaged deployment: immutable images, boundaries and uncertain restart', {timeout: 1200000}, async () => {
    buildPackages(['platform','proxy','browser-fixture','worker-opentofu','worker-terraform']);
    verifyReproducibility();
    await import('./package-verify.mjs');
});

test('packaged browsers: Chromium and Firefox journeys without CSP violations', {timeout: 600000}, () => {
    const result=spawnSync(process.execPath,[path.resolve('web/node_modules/@playwright/test/cli.js'),'test'],{
        cwd:path.resolve('web'),env:{...process.env,COSMOFORMER_PACKAGED_BROWSER:'1'},encoding:'utf8',timeout:550000,maxBuffer:16*1024*1024,
    });
    console.log(result.stdout);
    assert.ifError(result.error);
    assert.equal(result.status,0,result.stderr);
});
