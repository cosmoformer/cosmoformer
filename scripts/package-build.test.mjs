import {test} from 'node:test';
import assert from 'node:assert/strict';
import {chmodSync, copyFileSync, existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {spawnSync} from 'node:child_process';

test('package archives use the selected Buildx builder before loading and recording images', t => {
    const dir=mkdtempSync(path.join(tmpdir(),'cf-package-builder-'));
    t.after(()=>rmSync(dir,{recursive:true,force:true}));
    mkdirSync(path.join(dir,'scripts'));mkdirSync(path.join(dir,'bin'));
    copyFileSync(new URL('./package-build.mjs',import.meta.url),path.join(dir,'scripts/package-build.mjs'));
    writeFileSync(path.join(dir,'LICENSE'),'synthetic public source');
    const tool=(name,body)=>{const file=path.join(dir,'bin',name);writeFileSync(file,`#!${process.execPath}\n${body}`);chmodSync(file,0o755);};
    tool('git',`
const a=process.argv.slice(2);
if(a[0]==='ls-files')process.stdout.write(a.includes('--others')?'':'LICENSE\\0');
else if(a[0]==='rev-parse')console.log('a'.repeat(40));
else if(a[0]==='show')console.log('1700000000');
else if(a[0]!=='status')throw Error('Unexpected git command');
`);
    tool('docker',`
import assert from 'node:assert/strict';
import {appendFileSync,existsSync,writeFileSync} from 'node:fs';
const a=process.argv.slice(2);
appendFileSync('calls.jsonl',JSON.stringify(a)+'\\n');
if(a[0]==='build')throw Error('The default Docker driver cannot export this archive');
if(a[0]==='buildx'&&a[1]==='build'){
 assert.equal(process.env.BUILDX_BUILDER,'selected-fixture-builder');
 const output=a.find(x=>x.startsWith('--output='));
 assert(output.startsWith('--output=type=docker,'));assert(output.endsWith(',rewrite-timestamp=true'));
 writeFileSync(output.match(/dest=([^,]+)/)[1],'synthetic archive');
 writeFileSync(a[a.indexOf('--metadata-file')+1],JSON.stringify({'containerimage.digest':'sha256:'+'1'.repeat(64)}));
}else if(a[0]==='load')assert(existsSync(a[a.indexOf('--input')+1]));
else if(a[0]==='image'&&a[1]==='inspect')console.log(JSON.stringify([{Id:'sha256:'+'2'.repeat(64),RepoDigests:[],Architecture:'amd64'}]));
else if(a[0]==='create')console.log('synthetic-container');
else if(a[0]==='cp')writeFileSync(a[2],'3'.repeat(64)+'  cosmoformer\\n');
else if(a[0]!=='rm')throw Error('Unexpected Docker command');
`);
    const result=spawnSync(process.execPath,['scripts/package-build.mjs','platform'],{
        cwd:dir,encoding:'utf8',timeout:10000,
        env:{...process.env,PATH:path.join(dir,'bin')+path.delimiter+process.env.PATH,BUILDX_BUILDER:'selected-fixture-builder'},
    });
    assert.ifError(result.error);assert.equal(result.status,0,result.stderr);
    const calls=readFileSync(path.join(dir,'calls.jsonl'),'utf8').trim().split('\n').map(JSON.parse);
    assert.deepEqual(calls.map(a=>a[0]),['buildx','load','image','create','cp','rm']);
    const receipt=JSON.parse(readFileSync(path.join(dir,'.build/package/images.json'),'utf8'));
    assert.equal(receipt.images.platform.image_id,'sha256:'+'2'.repeat(64));
    assert.match(receipt.images.platform.archive_sha256,/^[a-f0-9]{64}$/);
    assert.equal(existsSync(path.join(dir,'.build/package-build.lock')),false);
});
