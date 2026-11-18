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

import {assertInventory, verifyInventory, readInventory, packageTargets} from './package-inventory.mjs';
function completeInventory() {
    return {source_sha256:'1'.repeat(64),revision:'2'.repeat(40),dirty:true,source_date_epoch:1700000000,images:Object.fromEntries(packageTargets.map(target=>[target,{image_id:'sha256:'+'3'.repeat(64),architecture:'amd64',archive_sha256:'4'.repeat(64),checksums:['5'.repeat(64)+'  artifact'],build_metadata:{'containerimage.digest':'sha256:'+'6'.repeat(64),'containerimage.config.digest':'sha256:'+'3'.repeat(64)}}]))};
}
const inspected=r=>({Id:r.images.platform.image_id,Architecture:'amd64',Config:{Labels:{'io.cosmoformer.source.sha256':r.source_sha256,'org.opencontainers.image.revision':r.revision,'io.cosmoformer.source.dirty':String(r.dirty)}}});
test('runtime/browser dependencies require complete verified immutable source-bound images',()=>{
    const r=completeInventory();let count=0;
    assert.throws(()=>verifyInventory(r,packageTargets,()=>{throw Error('daemon unavailable');}),/dependency failure: image unavailable/);
    verifyInventory(r,packageTargets,()=>{count++;return inspected(r);});assert.equal(count,5);
    for(const target of packageTargets) {
        const incomplete=structuredClone(r);delete incomplete.images[target];count=0;
        assert.throws(()=>verifyInventory(incomplete,packageTargets,()=>{count++;return inspected(r);}),new RegExp('dependency failure.*'+target));
        assert.equal(count,0);
    }
    for(const change of [i=>i.Id='sha256:'+'0'.repeat(64),i=>i.Architecture='arm64',i=>i.Config.Labels['io.cosmoformer.source.sha256']='0'.repeat(64),i=>i.Config.Labels['org.opencontainers.image.revision']='0'.repeat(40),i=>i.Config.Labels['io.cosmoformer.source.dirty']='false']) {
        const actual=inspected(r);change(actual);
        assert.throws(()=>verifyInventory(r,packageTargets,()=>actual),/dependency failure.*identity mismatch/);
    }
    const browser=structuredClone(r);delete browser.images['worker-opentofu'];delete browser.images['worker-terraform'];
    assert.doesNotThrow(()=>assertInventory(browser,packageTargets.slice(0,3)));
    assert.throws(()=>assertInventory(browser,packageTargets),/worker-opentofu/);
});

import {copyFileSync, existsSync, mkdirSync} from 'node:fs';
import {spawn} from 'node:child_process';
for(const signal of ['SIGTERM','SIGINT'])test(`packaged browser supervisor ${signal} cleans owned startup resources and preserves unrelated files`,async t=>{
    const dir=mkdtempSync(path.join(tmpdir(),'cf-package-interrupt-'));
    t.after(()=>rmSync(dir,{recursive:true,force:true}));
    mkdirSync(path.join(dir,'scripts'));mkdirSync(path.join(dir,'.build/testdb'),{recursive:true});
    writeFileSync(path.join(dir,'.build/testdb/unrelated'),'unrelated fixture');
    copyFileSync(new URL('./package-browser.mjs',import.meta.url),path.join(dir,'scripts/package-browser.mjs'));
    // Hold startup in the actual supervisor's cancellable credentials loop.
    writeFileSync(path.join(dir,'scripts/package-fixture.mjs'),`
import path from 'node:path';import {writeFileSync} from 'node:fs';
export const root=path.resolve(import.meta.dirname,'..');
export const delay=ms=>new Promise(resolve=>setTimeout(resolve,ms));
export const docker=()=>{throw Error('No unrelated Docker command allowed');};
export const waitHTTPS=()=>{throw Error('Must cancel before readiness');};
export const writeBrowserCredentials=()=>{throw Error('Must not write credentials');};
export const fixture=async()=>({dir:root,socket:'owned-socket',common:[],volume:()=> 'owned-volume',put:()=>{},run:()=>writeFileSync(path.join(root,'ready'),'ready'),image:()=> 'owned-image',read:()=>{throw Error('not ready');},stop:async()=>writeFileSync(path.join(root,'stopped'),'owned resources cleaned')});
`);
    const child=spawn(process.execPath,['scripts/package-browser.mjs'],{cwd:dir,stdio:'pipe'});
    let stderr='';child.stderr.on('data',data=>stderr+=data);
    const exit=new Promise((resolve,reject)=>{child.once('error',reject);child.once('exit',(code,signal)=>resolve({code,signal}));});
    t.after(()=>{if(child.exitCode===null)child.kill('SIGKILL');});
    const deadline=Date.now()+5000;
    while(!existsSync(path.join(dir,'ready'))&&Date.now()<deadline)await new Promise(resolve=>setTimeout(resolve,20));
    assert.equal(existsSync(path.join(dir,'ready')),true,stderr);
    child.kill(signal);
    assert.deepEqual(await exit,{code:0,signal:null},stderr);
    assert.equal(readFileSync(path.join(dir,'stopped'),'utf8'),'owned resources cleaned');
    assert.equal(readFileSync(path.join(dir,'.build/testdb/unrelated'),'utf8'),'unrelated fixture');
    assert.equal(existsSync(path.join(dir,'.build/testdb/browser.json')),false);
});

test('loaded IDs retain their builder/store semantics while both exporter digests remain required',()=>{
    const r=completeInventory();
    // A classic store exposes the config ID; the containerd store can expose
    // the manifest ID. Both still require separate strict config comparisons.
    assert.doesNotThrow(()=>assertInventory(r));
    for(const image of Object.values(r.images))image.image_id=image.build_metadata['containerimage.digest'];
    assert.doesNotThrow(()=>assertInventory(r));
    for(const image of Object.values(r.images))delete image.build_metadata['containerimage.config.digest'];
    assert.throws(()=>assertInventory(r),/malformed/);
});

test('missing and malformed inventory files give an explicit dependency error',t=>{
    const dir=mkdtempSync(path.join(tmpdir(),'cf-inventory-'));
    t.after(()=>rmSync(dir,{recursive:true,force:true}));
    const file=path.join(dir,'images.json');
    assert.throws(()=>readInventory(file),/Package dependency failure/);
    writeFileSync(file,'{broken');assert.throws(()=>readInventory(file),/Package dependency failure/);
    writeFileSync(file,JSON.stringify(completeInventory()));
    assert.deepEqual(readInventory(file),completeInventory());
});
