import {test, mock} from 'node:test';
import assert from 'node:assert/strict';
import {existsSync, mkdtempSync, writeFileSync, readFileSync, chmodSync, rmSync} from 'node:fs';
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

test('captured command stdout is drained, separate from diagnostics and byte bounded', async () => {
    const result=await child(process.execPath,['-e',`process.stderr.write('diagnostic');process.stdout.write('x'.repeat(100000));process.stdout.write(Buffer.from([0xe2]));setTimeout(()=>process.stdout.write(Buffer.from([0x82,0xac])),10);`],{quiet:true,captureStdout:true}).done;
    assert.equal(result.code,0);assert.equal(result.stdout,'x'.repeat(100000)+'€');assert.equal(result.stdoutTruncated,false);
    const large=await child(process.execPath,['-e',`process.stdout.write('x'.repeat(17*1024*1024));`],{quiet:true,captureStdout:true}).done;
    assert.equal(large.code,0);assert.equal(large.stdout.length,16*1024*1024);assert.equal(large.stdoutTruncated,true);
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

test('health probes reject errors and redirects rather than treating any response as ready', async t => {
    const {createServer} = await import('node:http');
    const {healthy} = await import('./workflow-lib.mjs');
    const server = createServer((request, response) => {
        if (request.url === '/ready') response.writeHead(200);
        else if (request.url === '/redirect') response.writeHead(302, {Location: '/ready'});
        else response.writeHead(503);
        response.end();
    });
    await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
    t.after(() => server.close());
    const origin = `http://127.0.0.1:${server.address().port}`;
    assert.equal(await healthy(origin + '/ready'), true);
    assert.equal(await healthy(origin + '/error'), false);
    assert.equal(await healthy(origin + '/redirect'), false);
    await new Promise(resolve => server.close(resolve));
    assert.equal(await healthy(origin + '/ready'), false);
});

test('a missing executable reports failure and can be stopped without a process ID', async () => {
    const missing = child('/cosmoformer-fixture/executable-does-not-exist', [], {quiet: true});
    assert.deepEqual(await missing.done, {code: 1, output: 'Required command could not start.'});
    assert.equal(missing.proc.pid, undefined);
    await missing.stop();
});

test('command output is captured privately or forwarded with its session label', async () => {
    const captured = await child(process.execPath, ['-e', 'process.stdout.write("fixture stdout"); process.stderr.write("fixture stderr");'], {quiet: true}).done;
    assert.equal(captured.code, 0);
    assert.match(captured.output, /fixture stdout/);
    assert.match(captured.output, /fixture stderr/);
    let forwarded = '';
    const writer = mock.method(process.stdout, 'write', chunk => { forwarded += chunk; return true; });
    let result;
    try {
        result = await child(process.execPath, ['-e', 'process.stdout.write("fixture output");'], {label: 'fixture'}).done;
    } finally { writer.mock.restore(); }
    assert.equal(result.code, 0);
    assert.equal(forwarded, '[fixture] fixture output');
});

// Exercise the actual repository module as well as the disposable package
// fixtures, so the normal workflow coverage measures its failure paths.
for(const mode of ['missing','denied','signals-denied'])test(`repository supervisor reports ${mode} inspection and returns within its deadline`,{timeout:10000},async t=>{
    const {spawn,spawnSync}=await import('node:child_process');
    const dir=mkdtempSync(path.join(tmpdir(),'cf-supervisor-inspection-'));
    t.after(()=>rmSync(dir,{recursive:true,force:true}));
    const inspector=path.join(dir,'unavailable-ps');
    if(mode==='denied')writeFileSync(inspector,'',{mode:0});
    const ready=path.join(dir,'ready'),report=path.join(dir,'report.json');
    const driver=path.join(dir,'driver.mjs');
    writeFileSync(driver,`
import assert from 'node:assert/strict';
import cp from 'node:child_process';
import {syncBuiltinESMExports} from 'node:module';
import {existsSync,writeFileSync} from 'node:fs';
const original=cp.spawnSync;
cp.spawnSync=(command,args,options)=>command==='/bin/ps'?original(${JSON.stringify(inspector)},args,options):original(command,args,options);
syncBuiltinESMExports();
if(${mode==='signals-denied'}) {
 const kill=process.kill;
 process.kill=(pid,signal)=>{if(pid<0)throw Object.assign(Error('EPERM: synthetic group signal denial'),{code:'EPERM'});return kill(pid,signal);};
}
const {child}=await import(${JSON.stringify(new URL('./workflow-lib.mjs',import.meta.url).href)});
const running=child(process.execPath,['--input-type=module','-e',${JSON.stringify(`import {writeFileSync} from 'node:fs';process.on('SIGTERM',()=>{});writeFileSync(${JSON.stringify(ready)},String(process.pid));setInterval(()=>{},1000);`)}],{quiet:true,captureStdout:true});
const readyDeadline=performance.now()+3000;
while(!existsSync(${JSON.stringify(ready)})&&performance.now()<readyDeadline)await new Promise(resolve=>setTimeout(resolve,20));
assert(existsSync(${JSON.stringify(ready)}));
const started=performance.now();let error;
try {await running.stop({graceMs:100});}catch(e){error=e;}
assert(error instanceof AggregateError);
const messages=[];const visit=e=>{messages.push(e.message??String(e));if(e.errors)for(const x of e.errors)visit(x);if(e.cause)visit(e.cause);};visit(error);
assert(messages.some(e=>/Cannot inspect/.test(e)));
assert(messages.some(e=>/${mode==='denied'?'EACCES':'ENOENT'}/.test(e)));
if(${mode==='signals-denied'})assert(messages.some(e=>/EPERM/.test(e)));
writeFileSync(${JSON.stringify(report)},JSON.stringify({group:running.proc.pid,messages,elapsed:performance.now()-started}));
`);
    const sentinel=spawn(process.execPath,['-e','setInterval(()=>{},1000)'],{stdio:'ignore'});
    t.after(()=>sentinel.kill('SIGKILL'));
    const parent=spawn(process.execPath,[driver],{stdio:['ignore','pipe','pipe']});
    let stderr='';parent.stderr.on('data',bytes=>stderr+=bytes);
    const exit=new Promise((resolve,reject)=>{parent.once('error',reject);parent.once('exit',code=>resolve(code));});
    let ownedGroup;
    t.after(()=>{if(parent.exitCode===null)parent.kill('SIGKILL');if(ownedGroup)try{process.kill(-ownedGroup,'SIGKILL');}catch(e){if(e.code!=='ESRCH')throw e;}});
    const readyDeadline=Date.now()+3500;
    while(!existsSync(ready)&&parent.exitCode===null&&Date.now()<readyDeadline)await new Promise(resolve=>setTimeout(resolve,20));
    assert(existsSync(ready),stderr);ownedGroup=Number(readFileSync(ready,'utf8'));
    const code=await Promise.race([exit,new Promise((_,reject)=>{const timer=setTimeout(()=>reject(Error('Supervision failure did not settle')),7000);timer.unref();})]);
    assert.equal(code,0,stderr);
    const result=JSON.parse(readFileSync(report,'utf8'));assert(result.elapsed<4500);
    assert(result.messages.some(e=>e.includes(String(result.group))));
    if(mode==='signals-denied') {assert.doesNotThrow(()=>process.kill(result.group,0));process.kill(-result.group,'SIGKILL');await new Promise(resolve=>setTimeout(resolve,100));}
    const state=spawnSync('/bin/ps',['-p',String(result.group),'-o','state='],{encoding:'utf8'});
    assert(state.status===1&&!state.stdout.trim()||state.status===0&&/^\s*Z/.test(state.stdout));
    assert.doesNotThrow(()=>process.kill(sentinel.pid,0));
});
