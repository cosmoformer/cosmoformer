import {test} from 'node:test';
import assert from 'node:assert/strict';
import {chmodSync, mkdtempSync, readFileSync, rmSync, statSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {waitHTTPS, writeBrowserCredentials} from './package-fixture.mjs';

test('browser credentials are private and cannot replace another fixture file', t => {
    const dir=mkdtempSync(path.join(tmpdir(),'cf-browser-credentials-'));
    t.after(()=>rmSync(dir,{recursive:true,force:true}));
    const file=path.join(dir,'browser.json');
    writeBrowserCredentials(file,'synthetic owned credentials');
    assert.equal(statSync(file).mode & 0o777,0o600);
    assert.throws(()=>writeBrowserCredentials(file,'another fixture'),{code:'EEXIST'});
    assert.equal(readFileSync(file,'utf8'),'synthetic owned credentials');
    const other=path.join(dir,'existing.json');
    writeFileSync(other,'existing interrupted fixture',{mode:0o644});
    chmodSync(other,0o644);
    assert.throws(()=>writeBrowserCredentials(other,'replacement'),{code:'EEXIST'});
    assert.equal(readFileSync(other,'utf8'),'existing interrupted fixture');
    assert.equal(statSync(other).mode & 0o777,0o644);
});

test('packaged readiness requires the real 200 health response rather than redirects or SPA bodies', async () => {
    const options={attempts:1,sleep:async()=>{}};
    await waitHTTPS('https://localhost/health/ready',{...options,run:()=> 'ok\n\n200'});
    for(const output of ['<html>redirect</html>\n302','<!doctype html>SPA\n200','ok\n\n503','\n200']) {
        await assert.rejects(waitHTTPS('https://localhost/health/ready',{...options,run:()=>output}),/did not become ready/);
    }
});
test('cancelled packaged startup stops readiness polling without launching another command', async () => {
    const controller=new AbortController(); let calls=0;
    await assert.rejects(waitHTTPS('https://localhost/health/ready',{
        run:()=>{calls++;throw Error('not ready');},sleep:async()=>controller.abort(),signal:controller.signal,
    }),{name:'AbortError'});
    assert.equal(calls,1);
});
