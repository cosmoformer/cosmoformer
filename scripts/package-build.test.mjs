import {test} from 'node:test';
import assert from 'node:assert/strict';
import {chmodSync, copyFileSync, existsSync, mkdirSync, mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import path from 'node:path';
import {spawnSync, spawn} from 'node:child_process';

test('package archives use the selected Buildx builder before loading and recording images', t => {
    const dir=mkdtempSync(path.join(tmpdir(),'cf-package-builder-'));
    t.after(()=>rmSync(dir,{recursive:true,force:true}));
    mkdirSync(path.join(dir,'scripts'));mkdirSync(path.join(dir,'bin'));
    for(const file of ['package-build.mjs','workflow-lib.mjs'])copyFileSync(new URL(file,import.meta.url),path.join(dir,'scripts',file));
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
import {appendFileSync,existsSync,readFileSync,writeFileSync} from 'node:fs';
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
else if(a[0]==='cp'&&process.env.FAIL_COPY==='1')throw Error('synthetic checksum copy failure');
else if(a[0]==='rm'&&process.env.FAIL_REMOVE==='1')throw Error('synthetic daemon unavailable');
else if(a[0]==='cp')writeFileSync(a[2],a[1].endsWith('/source.json')?readFileSync('.build/package/source.json'):a[1].endsWith('/Cosmoformer-MIT')?'synthetic public source':'3'.repeat(64)+'  cosmoformer\\n');
else if(a[0]!=='rm')throw Error('Unexpected Docker command');
`);
    const result=spawnSync(process.execPath,['scripts/package-build.mjs','platform'],{
        cwd:dir,encoding:'utf8',timeout:10000,
        env:{...process.env,PATH:path.join(dir,'bin')+path.delimiter+process.env.PATH,BUILDX_BUILDER:'selected-fixture-builder'},
    });
    assert.ifError(result.error);assert.equal(result.status,0,result.stderr);
    const calls=readFileSync(path.join(dir,'calls.jsonl'),'utf8').trim().split('\n').map(JSON.parse);
    assert.deepEqual(calls.map(a=>a[0]),['buildx','load','image','create','cp','cp','cp','rm']);
    const receipt=JSON.parse(readFileSync(path.join(dir,'.build/package/images.json'),'utf8'));
    assert.equal(receipt.images.platform.image_id,'sha256:'+'2'.repeat(64));
    assert.match(receipt.images.platform.archive_sha256,/^[a-f0-9]{64}$/);
    assert.equal(existsSync(path.join(dir,'.build/package-build.lock')),false);
});

// These processes run the real snapshot builder and reproduction supervisor.
// Only Docker's transport is replaced, so receipt/state/cleanup behavior is
// asserted from files and commands observed outside the child process.
import {execFileSync} from 'node:child_process';
import {createHash} from 'node:crypto';
import {fixtureGit} from './package-git.mjs';
function repository(t) {
    const dir=mkdtempSync(path.join(tmpdir(),'cf-package-state-'));
    t.after(()=>rmSync(dir,{recursive:true,force:true}));
    mkdirSync(path.join(dir,'scripts'));mkdirSync(path.join(dir,'bin'));
    for(const file of ['package-build.mjs','package-reproducibility.mjs','package-inventory.mjs','package-fixture.mjs','package-cache.mjs','package-git.mjs','workflow-lib.mjs'])copyFileSync(new URL(file,import.meta.url),path.join(dir,'scripts',file));
    writeFileSync(path.join(dir,'LICENSE'),'synthetic public input');
    writeFileSync(path.join(dir,'.gitignore'),'.build/\nbin/\ncalls.jsonl\nstate.json\nchild.mjs\n');
    mkdirSync(path.join(dir,'deploy/worker'),{recursive:true});
    writeFileSync(path.join(dir,'deploy/worker/runtimes.json'),JSON.stringify({runtimes:['opentofu','terraform'].map(engine=>({engine,version:'1.0.0',arch:'linux_amd64',base_image_ref:'synthetic@sha256:'+'a'.repeat(64),engine_archive:{url:'https://example.invalid/engine',sha256:'a'.repeat(64),member:'engine'}}))}));
    const git=fixtureGit(dir);
    git(['init','--quiet']);git(['config','user.name','Synthetic']);git(['config','user.email','synthetic@example.invalid']);git(['add','.']);
    git(['commit','--quiet','-m','A']);
    const docker=path.join(dir,'bin/docker');
    writeFileSync(docker,`#!${process.execPath}
import {createHash} from 'node:crypto';
import {appendFileSync,existsSync,readFileSync,writeFileSync} from 'node:fs';
import path from 'node:path';
const a=process.argv.slice(2),hash=s=>createHash('sha256').update(s).digest('hex');
appendFileSync(process.env.SYNTHETIC_CALLS??'calls.jsonl',JSON.stringify(a)+'\\n');
const stateFile=process.env.SYNTHETIC_STATE??'state.json';
const state=existsSync(stateFile)?JSON.parse(readFileSync(stateFile)):{images:{},tags:{}};
if(a[0]===process.env.INTERRUPT_OPERATION) {
 if(process.env.STUBBORN==='descendant') {
  const {spawn}=await import('node:child_process');
  spawn(process.execPath,['--input-type=module','-e','import {writeFileSync,appendFileSync} from "node:fs";process.on("SIGTERM",()=>{});writeFileSync("operation-descendant",String(process.pid));appendFileSync("operation-descendants",String(process.pid)+"\\\\n");setInterval(()=>{},1000);'],{stdio:'inherit'});
  process.on('SIGTERM',()=>process.exit(0));
 } else process.on('SIGTERM',()=>{});
 writeFileSync('operation-ready',String(process.pid));
 appendFileSync('operation-groups',String(process.pid)+'\\n');
 setInterval(()=>{},1000);
 await new Promise(()=>{});
}
if(a[0]==='buildx') {
 if(process.env.INTERRUPT_BUILD==='1') {
  writeFileSync('build-ready',String(process.pid));
  if(process.env.STUBBORN==='descendant') {
   const {spawn}=await import('node:child_process');
   const sub=spawn(process.execPath,['--input-type=module','-e','import {writeFileSync} from "node:fs";process.on("SIGTERM",()=>{});writeFileSync("descendant-ready",String(process.pid));setInterval(()=>{},1000);' ],{stdio:'inherit'});
   process.on('SIGTERM',()=>process.exit(0));
  } else process.on('SIGTERM',()=>{writeFileSync('build-stopped','stopped');if(process.env.STUBBORN!=='child')process.exit(0);});
  setInterval(()=>{},1000);
  await new Promise(()=>{});
 }
 const source=JSON.parse(readFileSync(path.join(a.at(-1),'packaging-source.json')));
 state.source=source;state.license=readFileSync(path.join(a.at(-1),'LICENSE'),'utf8');
 const target=a[a.indexOf('--target')+1];state.worker=target==='worker';
 const bytes=JSON.stringify(source)+target+(a.includes('--no-cache')&&process.env.FAIL_REPRO==='1'?'different':'');
 const id='sha256:'+hash(bytes),tag=a[a.indexOf('-t')+1];
 state.images[id]={Id:id,Architecture:'amd64',Config:{Labels:Object.fromEntries(a.flatMap((x,i)=>x==='--label'?[a[i+1].split(/=(.*)/s).slice(0,2)]:[]))},Created:new Date(source.source_date_epoch*1000).toISOString(),RootFS:{Layers:['sha256:'+hash(bytes+'layer')]}};
 state.tags[tag]=id;
 writeFileSync(a.find(x=>x.startsWith('--output=')).match(/dest=([^,]+)/)[1],bytes);
 writeFileSync(a[a.indexOf('--metadata-file')+1],JSON.stringify({'containerimage.config.digest':id,'containerimage.digest':'sha256:'+hash(bytes+'manifest')}));
 if(a.includes('--no-cache')&&process.env.FAIL_REPRO==='build')throw Error('synthetic build failure');
} else if(a[0]==='image'&&a[1]==='inspect') {
 const image=state.images[state.tags[a[2]]??a[2]];if(!image)throw Error('Missing image');console.log(JSON.stringify([image]));
} else if(a[0]==='tag') {if(process.env.FAIL_REPRO==='cleanup'&&a[2].endsWith(':local'))throw Error('synthetic tag restore failure');state.tags[a[2]]=state.tags[a[1]]??a[1];}
else if(a[0]==='create')console.log('owned-container');
else if(a[0]==='cp'&&process.env.FAIL_COPY==='1')throw Error('synthetic checksum copy failure');
else if(a[0]==='rm'&&process.env.FAIL_REMOVE==='1')throw Error('synthetic daemon unavailable');
else if(a[0]==='cp')writeFileSync(a[2],a[1].endsWith('/source.json')?JSON.stringify(process.env.STALE_SOURCE==='1'?{...state.source,source_sha256:'f'.repeat(64)}:state.worker?{revision:state.source.revision,source_sha256:state.source.source_sha256}:state.source):a[1].endsWith('/Cosmoformer-MIT')?(process.env.STALE_LICENSE==='1'?'stale license':state.license):hash('artifact')+'  artifact\\n');
else if(a[0]==='image'&&a[1]==='rm') {if(!state.tags[a[2]])throw Error('Deleting unowned tag');delete state.tags[a[2]];}
else if(!['rm','load'].includes(a[0]))throw Error('Unexpected Docker command: '+a);
writeFileSync(stateFile,JSON.stringify(state));
`);chmodSync(docker,0o755);
    const run=(code,extra={})=>{
        writeFileSync(path.join(dir,'child.mjs'),code);
        return spawnSync(process.execPath,['child.mjs'],{cwd:dir,encoding:'utf8',timeout:60000,env:{...process.env,PATH:path.join(dir,'bin')+path.delimiter+process.env.PATH,...extra}});
    };
    return {dir,git,run,json:name=>JSON.parse(readFileSync(path.join(dir,name),'utf8'))};
}
const buildCode=`import {buildPackages} from './scripts/package-build.mjs';await buildPackages(['platform','proxy','browser-fixture']);`;

test('source receipts bind revision, dirty state, admitted bytes and commit timestamps',t=>{
    const f=repository(t);
    let result=f.run(buildCode);assert.equal(result.status,0,result.stderr);
    const first=f.json('.build/package/source.json');assert.equal(first.dirty,false);
    assert.equal(first.revision,f.git(['rev-parse','HEAD']).toString().trim());
    assert.equal(first.source_sha256,createHash('sha256').update(JSON.stringify(first.files)).digest('hex'));
    assert.equal(first.files.LICENSE,createHash('sha256').update('synthetic public input').digest('hex'));
    writeFileSync(path.join(f.dir,'LICENSE'),'changed synthetic input');
    result=f.run(buildCode);assert.equal(result.status,0,result.stderr);
    const second=f.json('.build/package/source.json');assert.equal(second.dirty,true);
    assert.equal(second.revision,first.revision);assert.notEqual(second.source_sha256,first.source_sha256);
    assert.equal(second.source_date_epoch,first.source_date_epoch);
    assert.equal(existsSync(path.join(f.dir,'.build/package-build.lock')),false);
});
for(const failure of ['0','1','build','cleanup'])test(`reproduction ${failure} preserves full inventory and diagnostic receipts`,t=>{
    const f=repository(t);
    let result=f.run(buildCode);assert.equal(result.status,0,result.stderr);
    const first=readFileSync(path.join(f.dir,'.build/package/images.json'),'utf8');
    result=f.run(`import {verifyReproducibility} from './scripts/package-reproducibility.mjs';await verifyReproducibility();`,{FAIL_REPRO:failure});
    assert.equal(result.status,failure==='0'?0:1,result.stderr);
    assert.equal(readFileSync(path.join(f.dir,'.build/package/images.json'),'utf8'),first);
    const report=f.json('.build/package/reproducibility.json');
    assert.deepEqual(f.json(report.evidence+'/first.json'),JSON.parse(first));
    assert.deepEqual(f.json(report.evidence+'/comparison.json'),report);
    if(failure!=='build')assert.deepEqual(Object.keys(f.json(report.evidence+'/second.json').images),['platform','proxy','browser-fixture']);
    else assert.match(report.error,/Docker buildx failed/);
    assert.equal(existsSync(path.join(f.dir,report.evidence,'resources.json')),failure!=='0');
    const state=f.json('state.json');
    for(const [target,image] of Object.entries(JSON.parse(first).images))assert.equal(state.tags['cosmoformer-'+target+':local'],image.image_id);
    const calls=readFileSync(path.join(f.dir,'calls.jsonl'),'utf8').trim().split('\n').map(JSON.parse);
    assert.equal(calls.some(a=>a.includes('prune')||a.includes('buildx')&&a.includes('rm')),false);
    assert.equal(existsSync(path.join(f.dir,'.build/package-build.lock')),false);
});

for(const signal of ['SIGTERM','SIGINT'])test(`interrupted reproduction ${signal} cancels its child and preserves original/unrelated images`,async t=>{
    const f=repository(t);
    const built=f.run(buildCode);assert.equal(built.status,0,built.stderr);
    const original=readFileSync(path.join(f.dir,'.build/package/images.json'),'utf8');
    const state=f.json('state.json');state.tags['unrelated:keep']='sha256:'+'f'.repeat(64);
    writeFileSync(path.join(f.dir,'state.json'),JSON.stringify(state));
    writeFileSync(path.join(f.dir,'child.mjs'),`import {verifyReproducibility} from './scripts/package-reproducibility.mjs';await verifyReproducibility();`);
    const child=spawn(process.execPath,['child.mjs'],{cwd:f.dir,env:{...process.env,PATH:path.join(f.dir,'bin')+path.delimiter+process.env.PATH,INTERRUPT_BUILD:'1'},stdio:'pipe'});
    let stderr='';child.stderr.on('data',bytes=>stderr+=bytes);
    const exited=new Promise((resolve,reject)=>{child.once('error',reject);child.once('exit',(code,signal)=>resolve({code,signal}));});
    t.after(()=>{if(child.exitCode===null)child.kill('SIGKILL');});
    const deadline=Date.now()+5000;
    while(!existsSync(path.join(f.dir,'build-ready'))&&Date.now()<deadline)await new Promise(resolve=>setTimeout(resolve,20));
    assert.equal(existsSync(path.join(f.dir,'build-ready')),true,stderr);
    child.kill(signal);const result=await exited;
    assert.notEqual(result.code,0);assert.match(stderr,/Package build interrupted/);
    assert.equal(readFileSync(path.join(f.dir,'build-stopped'),'utf8'),'stopped');
    assert.equal(existsSync(path.join(f.dir,'.build/package-build.lock')),false);
    assert.equal(readFileSync(path.join(f.dir,'.build/package/images.json'),'utf8'),original);
    assert.equal(f.json('state.json').tags['unrelated:keep'],state.tags['unrelated:keep']);
    const report=f.json('.build/package/reproducibility.json');assert.match(report.error,/interrupted/);
    assert.deepEqual(f.json(report.evidence+'/first.json'),JSON.parse(original));
    assert.equal(existsSync(path.join(f.dir,report.evidence,'resources.json')),true);
});

test('Docker cleanup failure preserves both errors and resource ID but releases lock and listeners',t=>{
    const f=repository(t);
    const result=f.run(`
import assert from 'node:assert/strict';
import {buildPackages} from './scripts/package-build.mjs';
const before=['SIGINT','SIGTERM'].map(s=>process.listenerCount(s));
await assert.rejects(buildPackages(['platform']),e=>{
 assert(e instanceof AggregateError);
 assert.match(e.errors[0].message,/synthetic checksum copy failure/);
 assert.match(e.errors[1].message,/container cleanup failed/);
 return true;
});
assert.deepEqual(['SIGINT','SIGTERM'].map(s=>process.listenerCount(s)),before);
`,{FAIL_COPY:'1',FAIL_REMOVE:'1'});
    assert.equal(result.status,0,result.stderr);
    assert.equal(existsSync(path.join(f.dir,'.build/package-build.lock')),false);
    const receipt=readdirSync(path.join(f.dir,'.build/package')).find(name=>name.startsWith('build-resources-'));
    assert.deepEqual(f.json('.build/package/'+receipt),{container:'owned-container'});
    assert.equal(existsSync(path.join(f.dir,'.build/package/images.json')),false);
    const recovered=f.run(buildCode);assert.equal(recovered.status,0,recovered.stderr);
    assert.deepEqual(readdirSync(path.join(f.dir,'.build/package')).filter(name=>name.startsWith('build-resources-')),[receipt]);
    assert.deepEqual(f.json('.build/package/'+receipt),{container:'owned-container'});
    const calls=readFileSync(path.join(f.dir,'calls.jsonl'),'utf8').trim().split('\n').map(JSON.parse);
    assert(calls.filter(a=>a[0]==='rm').every(a=>a.join(' ')==='rm -f owned-container'));
});

for(const signal of ['SIGINT','SIGTERM'])for(const mode of ['child','descendant'])test(`bounded ${signal} cancellation stops stubborn ${mode} and releases build lock`,{timeout:15000},async t=>{
    const f=repository(t);
    const sentinel=spawn(process.execPath,['-e','setInterval(()=>{},1000)'],{stdio:'ignore'});
    t.after(()=>sentinel.kill('SIGKILL'));
    writeFileSync(path.join(f.dir,'child.mjs'),buildCode);
    const child=spawn(process.execPath,['child.mjs'],{cwd:f.dir,stdio:['ignore','pipe','pipe'],env:{...process.env,PATH:path.join(f.dir,'bin')+path.delimiter+process.env.PATH,INTERRUPT_BUILD:'1',STUBBORN:mode}});
    let stderr='';child.stderr.on('data',data=>stderr+=data);
    const exited=new Promise((resolve,reject)=>{child.once('error',reject);child.once('exit',code=>resolve(code));});
    t.after(()=>{if(child.exitCode===null)child.kill('SIGKILL');});
    const ready=path.join(f.dir,mode==='child'?'build-ready':'descendant-ready');
    const deadline=Date.now()+5000;
    while(!existsSync(ready)&&Date.now()<deadline)await new Promise(resolve=>setTimeout(resolve,20));
    assert(existsSync(ready),stderr);
    const group=Number(readFileSync(path.join(f.dir,'build-ready'),'utf8'));
    t.after(()=>{try{process.kill(-group,'SIGKILL');}catch(error){if(error.code!=='ESRCH')throw error;}});
    const started=Date.now();child.kill(signal);
    const code=await Promise.race([exited,new Promise((_,reject)=>{const timer=setTimeout(()=>reject(Error('Cancellation exceeded deadline')),6000);timer.unref();})]);
    assert.notEqual(code,0);assert(Date.now()-started<6000);assert.match(stderr,/Package build interrupted/);
    assert.equal(existsSync(path.join(f.dir,'.build/package-build.lock')),false);
    if(mode==='descendant') {
        // An orphan may remain a zombie briefly; it cannot execute work. Match
        // the supervisor's liveness contract instead of requiring instant reaping.
        const pid=Number(readFileSync(ready,'utf8'));
        const state=spawnSync('/bin/ps',['-p',String(pid),'-o','stat='],{encoding:'utf8'});
        assert(state.status===1&&!state.stdout.trim() || state.status===0&&/^\s*Z/.test(state.stdout),`Descendant still running: ${state.stdout}`);
    }
    assert.doesNotThrow(()=>process.kill(sentinel.pid,0));
    const recovered=f.run(buildCode);assert.equal(recovered.status,0,recovered.stderr);
});

test('disposable Git histories ignore inherited signing, hooks, templates and repository selectors',t=>{
    const dir=mkdtempSync(path.join(tmpdir(),'cf-package-git-'));
    t.after(()=>rmSync(dir,{recursive:true,force:true}));
    const repo=path.join(dir,'repo');mkdirSync(repo);
    const hooks=path.join(dir,'hooks');mkdirSync(hooks);
    const marker=path.join(dir,'executed');
    const hostile=path.join(dir,'hostile');writeFileSync(hostile,`#!/bin/sh\ntouch '${marker}'\nexit 91\n`);chmodSync(hostile,0o755);
    copyFileSync(hostile,path.join(hooks,'pre-commit'));
    const config=path.join(dir,'gitconfig');
    writeFileSync(config,`[commit]\n gpgsign = true\n[gpg]\n program = ${hostile}\n[core]\n hooksPath = ${hooks}\n[init]\n templateDir = ${dir}/missing-template\n`);
    const git=fixtureGit(repo,{...process.env,GIT_CONFIG_GLOBAL:config,GIT_CONFIG_SYSTEM:config,GIT_DIR:path.join(dir,'missing-repository'),GIT_CONFIG_COUNT:'1',GIT_CONFIG_KEY_0:'commit.gpgsign',GIT_CONFIG_VALUE_0:'true'});
    git(['init','--quiet']);writeFileSync(path.join(repo,'LICENSE'),'fixture');git(['add','.']);
    const commit=(name,epoch)=>git(['commit','--quiet','--allow-empty','-m',name],{GIT_AUTHOR_DATE:`@${epoch} +0000`,GIT_COMMITTER_DATE:`@${epoch} +0000`});
    commit('A',1700000000);const a=git(['rev-parse','HEAD']).trim();
    commit('B',1700007200);const b=git(['rev-parse','HEAD']).trim();
    assert.notEqual(a,b);assert.equal(git(['status','--porcelain']),'');
    assert.equal(git(['show','-s','--format=%ct','HEAD']).trim(),'1700007200');
    assert.equal(git(['diff',a,b]),'');assert.equal(existsSync(marker),false);
});

for(const [flag,kind] of [['STALE_SOURCE','source identity'],['STALE_LICENSE','license']])test(`stale embedded ${kind} cannot be admitted despite matching image labels`,t=>{
    const f=repository(t);
    const result=f.run(buildCode,{[flag]:'1'});
    assert.notEqual(result.status,0);
    assert.match(result.stderr,new RegExp(`Embedded package ${kind} disagrees with admitted source`));
    assert.equal(existsSync(path.join(f.dir,'.build/package/images.json')),false);
    assert.equal(existsSync(path.join(f.dir,'.build/package-build.lock')),false);
    const calls=readFileSync(path.join(f.dir,'calls.jsonl'),'utf8').trim().split('\n').map(JSON.parse);
    assert.deepEqual(calls.at(-1),['rm','-f','owned-container']);
    assert(existsSync(path.join(f.dir,'.build/package/platform.source.json')));
});

for(const operation of ['load','cp','create','rm'])for(const signal of ['SIGINT','SIGTERM'])test(`bounded ${signal} during Docker ${operation} cleans only owned resources`,{timeout:35000},async t=>{
    const f=repository(t);
    const sentinel=spawn(process.execPath,['-e','setInterval(()=>{},1000)'],{stdio:'ignore'});
    t.after(()=>sentinel.kill('SIGKILL'));
    writeFileSync(path.join(f.dir,'child.mjs'),`
import assert from 'node:assert/strict';
import {buildPackages} from './scripts/package-build.mjs';
const before=['SIGINT','SIGTERM'].map(s=>process.listenerCount(s));
await assert.rejects(buildPackages(['platform']),e=>{
 if(e instanceof AggregateError) {assert.match(e.errors[0].message,/interrupted/);assert.match(e.errors[1].message,/cleanup failed/);}
 else assert.match(e.message,/interrupted/);
 return true;
});
assert.deepEqual(['SIGINT','SIGTERM'].map(s=>process.listenerCount(s)),before);
`);
    const child=spawn(process.execPath,['child.mjs'],{cwd:f.dir,stdio:['ignore','pipe','pipe'],env:{...process.env,PATH:path.join(f.dir,'bin')+path.delimiter+process.env.PATH,INTERRUPT_OPERATION:operation}});
    let stderr='';child.stderr.on('data',bytes=>stderr+=bytes);
    const exited=new Promise((resolve,reject)=>{child.once('error',reject);child.once('exit',code=>resolve(code));});
    t.after(()=>{if(child.exitCode===null)child.kill('SIGKILL');});
    const ready=path.join(f.dir,'operation-ready'),deadline=Date.now()+5000;
    while(!existsSync(ready)&&Date.now()<deadline)await new Promise(resolve=>setTimeout(resolve,20));
    assert(existsSync(ready),stderr);
    const group=Number(readFileSync(ready,'utf8'));
    t.after(()=>{try{process.kill(-group,'SIGKILL');}catch(error){if(error.code!=='ESRCH')throw error;}});
    const started=Date.now();child.kill(signal);
    const code=await Promise.race([exited,new Promise((_,reject)=>{const timer=setTimeout(()=>reject(Error('Docker operation cancellation exceeded deadline')),28000);timer.unref();})]);
    assert.equal(code,0,stderr);assert(Date.now()-started<28000);
    assert.equal(existsSync(path.join(f.dir,'.build/package-build.lock')),false);
    assert.equal(existsSync(path.join(f.dir,'.build/package/images.json')),false);
    const resources=readdirSync(path.join(f.dir,'.build/package')).filter(name=>name.startsWith('build-resources-'));
    assert.equal(resources.length,operation==='rm'?1:0);
    if(operation==='rm')assert.deepEqual(f.json('.build/package/'+resources[0]),{container:'owned-container'});
    const calls=readFileSync(path.join(f.dir,'calls.jsonl'),'utf8').trim().split('\n').map(JSON.parse);
    const removals=calls.filter(args=>args[0]==='rm');
    assert.equal(removals.length,operation==='load'?0:operation==='rm'?2:1);
    for(const args of removals)assert(args[1]==='-f'&&(args[2]==='owned-container'||/^cosmoformer-package-[a-f0-9-]+$/.test(args[2])));
    assert.doesNotThrow(()=>process.kill(sentinel.pid,0));
    const recovered=f.run(buildCode);assert.equal(recovered.status,0,recovered.stderr);
});

for(const mode of ['global-config','selectors'])test(`actual cross-epoch fixture builds isolate hostile Git ${mode}`,t=>{
    const f=repository(t),marker=path.join(f.dir,'executed'),hook=path.join(f.dir,'fsmonitor');
    writeFileSync(hook,`#!/bin/sh\ntouch '${marker}'\nprintf 'dummy-token\\0'\n`);chmodSync(hook,0o755);
    const config=path.join(f.dir,'hostile.gitconfig');
    writeFileSync(config,`[core]\n fsmonitor = ${hook}\n[commit]\n gpgsign = true\n[gpg]\n program = ${hook}\n`);
    const code=`
import {fixtureGit} from './scripts/package-git.mjs';
import {buildPackages} from './scripts/package-build.mjs';
import {packageTargets} from './scripts/package-inventory.mjs';
import {verifyCrossEpochCache} from './scripts/package-cache.mjs';
await buildPackages(packageTargets,{git:fixtureGit(process.cwd())});
await verifyCrossEpochCache();`;
    const result=f.run(code,{SYNTHETIC_STATE:path.join(f.dir,'state.json'),SYNTHETIC_CALLS:path.join(f.dir,'calls.jsonl'),GIT_CONFIG_GLOBAL:config,GIT_CONFIG_SYSTEM:config,...(mode==='selectors'?{GIT_DIR:path.join(f.dir,'missing.git'),GIT_WORK_TREE:path.join(f.dir,'missing-tree'),GIT_INDEX_FILE:path.join(f.dir,'missing-index')} : {}),GIT_CONFIG_COUNT:'1',GIT_CONFIG_KEY_0:'core.fsmonitor',GIT_CONFIG_VALUE_0:hook});
    assert.equal(result.status,0,result.stderr);
    assert.equal(existsSync(marker),false);
    const evidence=readdirSync(path.join(f.dir,'.build')).find(name=>name.startsWith('package-cache-'));
    const a=f.json('.build/'+evidence+'/a.json'),b=f.json('.build/'+evidence+'/b.json');
    assert.notEqual(a.revision,b.revision);assert.equal(b.source_date_epoch-a.source_date_epoch,7200);
    assert.equal(a.dirty,false);assert.equal(b.dirty,false);assert.equal(a.source_sha256,b.source_sha256);
    assert.equal(existsSync(path.join(f.dir,'.build',evidence,'repo')),false);
});

const inspectionCases=[
    {operation:'load',mode:'child',signal:'SIGINT'},
    {operation:'load',mode:'descendant',signal:'SIGTERM'},
    {operation:'cp',mode:'child',signal:'SIGTERM'},
    {operation:'cp',mode:'descendant',signal:'SIGINT'},
    {operation:'rm',mode:'descendant',signal:'SIGTERM'},
    {operation:'load',mode:'child',signal:'SIGTERM',denySignals:true},
];
for(const denied of [false,true])for(const scenario of inspectionCases)test(`${denied?'denied':'missing'} process inspection: ${scenario.operation}/${scenario.mode}/${scenario.signal}${scenario.denySignals?'/signals-denied':''} remains bounded`,{timeout:25000},async t=>{
    const f=repository(t),inspector=path.join(f.dir,'bin','unavailable-ps');
    if(denied)writeFileSync(inspector,'',{mode:0});
    const sentinel=spawn(process.execPath,['-e','setInterval(()=>{},1000)'],{stdio:'ignore'});
    t.after(()=>sentinel.kill('SIGKILL'));
    // Route only ps to a real missing/non-executable file. All Node, Docker,
    // signal delivery and cleanup paths still run in the actual builder.
    writeFileSync(path.join(f.dir,'child.mjs'),`
import assert from 'node:assert/strict';
import cp from 'node:child_process';
import {syncBuiltinESMExports} from 'node:module';
import {appendFileSync,writeFileSync} from 'node:fs';
const original=cp.spawnSync;
cp.spawnSync=(command,args,options)=>{
 if(command!=='/bin/ps')return original(command,args,options);
 const result=original(${JSON.stringify(inspector)},args,options);
 assert.equal(result.error?.code,${JSON.stringify(denied?'EACCES':'ENOENT')});
 appendFileSync('inspection-errors',result.error.code+'\\n');
 return result;
};
syncBuiltinESMExports();
if(${!!scenario.denySignals}) {
 const kill=process.kill;
 process.kill=(pid,signal)=>{if(pid<0)throw Object.assign(Error('EPERM: synthetic owned-group signal denial'),{code:'EPERM'});return kill(pid,signal);};
}
const {buildPackages}=await import('./scripts/package-build.mjs');
const before=['SIGINT','SIGTERM'].map(s=>process.listenerCount(s));
let failure;
try {await buildPackages(['platform']);}catch(error){failure=error;}
assert(failure instanceof AggregateError);
const errors=[];const visit=e=>{errors.push(e.message??String(e));if(e.errors)for(const x of e.errors)visit(x);if(e.cause)visit(e.cause);};visit(failure);
assert(errors.some(e=>/Package build interrupted: ${scenario.signal}/.test(e)));
assert(errors.some(e=>/Cannot inspect development child processes/.test(e)));
assert(errors.some(e=>/${denied?'EACCES':'ENOENT'}/.test(e)));
assert(errors.some(e=>/process group [0-9]+/.test(e)));
if(${!!scenario.denySignals})assert(errors.some(e=>/EPERM/.test(e)));
if(${scenario.operation==='rm'})assert.match(failure.errors[1].message,/container cleanup failed/);
assert.deepEqual(['SIGINT','SIGTERM'].map(s=>process.listenerCount(s)),before);
writeFileSync('reported-errors',JSON.stringify(errors));
`);
    const child=spawn(process.execPath,['child.mjs'],{cwd:f.dir,stdio:['ignore','pipe','pipe'],env:{...process.env,PATH:path.join(f.dir,'bin')+path.delimiter+process.env.PATH,INTERRUPT_OPERATION:scenario.operation,STUBBORN:scenario.mode}});
    let stderr='';child.stderr.on('data',bytes=>stderr+=bytes);
    const exited=new Promise((resolve,reject)=>{child.once('error',reject);child.once('exit',code=>resolve(code));});
    t.after(()=>{if(child.exitCode===null)child.kill('SIGKILL');});
    const ready=path.join(f.dir,scenario.mode==='descendant'?'operation-descendant':'operation-ready'),deadline=Date.now()+5000;
    while(!existsSync(ready)&&Date.now()<deadline)await new Promise(resolve=>setTimeout(resolve,20));
    assert(existsSync(ready),stderr);
    const group=Number(readFileSync(path.join(f.dir,'operation-ready'),'utf8'));
    t.after(()=>{try{process.kill(-group,'SIGKILL');}catch(error){if(error.code!=='ESRCH')throw error;}});
    const started=Date.now();child.kill(scenario.signal);
    const limit=scenario.operation==='rm'?18000:scenario.denySignals?8000:6000;
    const code=await Promise.race([exited,new Promise((_,reject)=>{const timer=setTimeout(()=>reject(Error('Inspection failure blocked shutdown')),limit);timer.unref();})]);
    assert.equal(code,0,stderr);assert(Date.now()-started<limit);
    assert.equal(existsSync(path.join(f.dir,'.build/package-build.lock')),false);
    assert.equal(existsSync(path.join(f.dir,'.build/package/images.json')),false);
    assert.match(readFileSync(path.join(f.dir,'inspection-errors'),'utf8'),new RegExp(denied?'EACCES':'ENOENT'));
    assert(f.json('reported-errors').some(e=>e.includes('interrupted')));
    // The outer controller retains real process inspection. Check every
    // operation, including the second Docker removal under its own deadline.
    const groups=readFileSync(path.join(f.dir,'operation-groups'),'utf8').trim().split('\n').map(Number);
    t.after(()=>{for(const pgid of groups)try{process.kill(-pgid,'SIGKILL');}catch(error){if(error.code!=='ESRCH')throw error;}});
    const descendants=existsSync(path.join(f.dir,'operation-descendants'))?readFileSync(path.join(f.dir,'operation-descendants'),'utf8').trim().split('\n').map(Number):[];
    if(scenario.denySignals) {
        // No code can override an OS signal denial. Return a bounded failure
        // with the owned group ID for recovery, then let this controller stop it.
        assert.doesNotThrow(()=>process.kill(group,0));process.kill(-group,'SIGKILL');
        await new Promise(resolve=>setTimeout(resolve,100));
    }
    for(const pid of [...groups,...descendants]) {
        const state=spawnSync('/bin/ps',['-p',String(pid),'-o','stat='],{encoding:'utf8'});
        assert(state.status===1&&!state.stdout.trim()||state.status===0&&/^\s*Z/.test(state.stdout),`Owned process still running: ${pid} ${state.stdout}`);
    }
    const resources=readdirSync(path.join(f.dir,'.build/package')).filter(name=>name.startsWith('build-resources-'));
    assert.equal(resources.length,scenario.operation==='rm'?1:0);
    if(resources.length)assert.deepEqual(f.json('.build/package/'+resources[0]),{container:'owned-container'});
    assert.doesNotThrow(()=>process.kill(sentinel.pid,0));
    const recovered=f.run(buildCode);assert.equal(recovered.status,0,recovered.stderr);
});
