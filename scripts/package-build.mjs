// Freeze the public build inputs once so both images consume exactly one tree.
import {execFileSync} from 'node:child_process';
import {createHash, randomUUID} from 'node:crypto';
import {existsSync, lstatSync, mkdirSync, readFileSync, rmSync, writeFileSync} from 'node:fs';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
import {isDeepStrictEqual} from 'node:util';
import {child as supervisedChild} from './workflow-lib.mjs';
const root = path.resolve(import.meta.dirname, '..'), output = path.join(root, '.build/package');
const run = (command, args, options = {}) => execFileSync(command, args, {cwd: root, encoding: 'utf8', maxBuffer: 16*1024*1024, ...options});
export async function buildPackages(targets = [], {outputDir = output, tagPrefix = 'cosmoformer', noCache = process.env.COSMOFORMER_PACKAGE_NO_CACHE === '1', git = args => run('git', args)} = {}) {
const output = outputDir;
const lock=path.join(root,'.build/package-build.lock');
mkdirSync(path.dirname(lock),{recursive:true});
try {mkdirSync(lock,{mode:0o700});}catch{throw Error('A package build is already recorded; inspect .build/package-build.lock before another build');}
let container, failure;
const resources=path.join(output,`build-resources-${randomUUID()}.json`);
const removeContainer = async ({signal=AbortSignal.any([controller.signal,AbortSignal.timeout(10000)])} = {}) => {
    if (!container) return;
    await docker(['rm','-f',container],{signal});
    container=undefined;
    rmSync(resources,{force:true});
};
const controller=new AbortController();
const interrupt = signal => controller.abort(Error(`Package build interrupted: ${signal}`));
const onINT=()=>interrupt('SIGINT'),onTERM=()=>interrupt('SIGTERM');
process.on('SIGINT',onINT);process.on('SIGTERM',onTERM);
// Every Docker operation yields to signal handlers. Cleanup uses its own bounded
// signal so it still runs after the build has been interrupted.
const docker = async (args, {signal=controller.signal, quiet=true} = {}) => {
    signal.throwIfAborted();
    const child=supervisedChild('docker',args,{quiet,captureStdout:true});
    let rejectCancellation;
    const cancelled=new Promise((_,reject)=>{rejectCancellation=reject;});
    const cancel=()=>{
        child.stop({graceMs:1500}).then(
            ()=>rejectCancellation(signal.reason),
            error=>rejectCancellation(new AggregateError([signal.reason,error],`Package Docker ${args[0]} interrupted; child shutdown could not be verified`,{cause:signal.reason})),
        );
    };
    signal.addEventListener('abort',cancel,{once:true});
    try {
        const result=await Promise.race([child.done,cancelled]);
        if(signal.aborted)await cancelled;
        signal.throwIfAborted();
        if(result.code!==0)throw Error(`Package Docker ${args[0]} failed (${result.code}): ${result.output}`);
        if(result.stdoutTruncated)throw Error('Package Docker output exceeded capture limit');
        return result.stdout;
    } finally {signal.removeEventListener('abort',cancel);}
};
try {
writeFileSync(path.join(lock,'pid'),String(process.pid),{mode:0o600});
const context = path.join(output, 'source');
mkdirSync(output, {recursive: true});
rmSync(context, {recursive: true, force: true});
mkdirSync(context);
const epoch = git(['show','-s','--format=%ct','HEAD']).trim();
if (!/^\d+$/.test(epoch)) throw Error('Invalid source commit epoch');
const files = {};
const excluded = /(^|\/)(node_modules|build|\.generated|\.react-router|test-results|playwright-report|\.DS_Store)(\/|$)|(^|\/)\.env(?:\.|$)|\.(?:pem|key|dsn|tfstate|tfplan)$/;
const tracked = git(['ls-files', '-z']).split('\0').filter(Boolean);
const added = git(['ls-files', '--others', '--exclude-standard', '-z']).split('\0').filter(file => /^(?:deploy\/platform\/[^/]+|deploy\/worker\/(?:compose.yaml|\.env.example)|scripts\/(?:package-[^/]+|go-coverage(?:\.test)?)\.mjs|internal\/(?:store\/(?:migrations\/017_auth_throttle_capacity.sql|auth_capacity_test.go)|httpapi\/auth_capacity_integration_test.go)|tests\/acceptance\/packaged_test.go|web\/tests\/(?:fixtures\/test.ts|csp-monitor.spec.ts))$/.test(file));
for (const relative of [...new Set([...tracked, ...added])].sort()) {
    if (!/^(cmd\/|internal\/|web\/|deploy\/|scripts\/|tests\/|go.mod$|go.sum$|LICENSE$|Makefile$)/.test(relative) || excluded.test(relative)) continue;
    const file = path.join(root, relative);
    if (!existsSync(file)) throw Error(`Missing tracked build input: ${relative}`);
    const parts=relative.split('/');
    for(let i=1;i<parts.length;i++)if(lstatSync(path.join(root,...parts.slice(0,i))).isSymbolicLink())throw Error('Symlinked build input');
    const info=lstatSync(file);
    if (!info.isFile() || info.nlink !== 1) throw Error(`Nonregular build input: ${relative}`);
    const data=readFileSync(file);
    files[relative]=createHash('sha256').update(data).digest('hex');
    mkdirSync(path.dirname(path.join(context, relative)),{recursive:true});
    writeFileSync(path.join(context,relative),data,{mode:info.mode & 0o777});
}
const source = {schema_version: 1, revision: git(['rev-parse','HEAD']).trim(), dirty: git(['status','--porcelain']).length > 0, source_date_epoch: Number(epoch), source_sha256: createHash('sha256').update(JSON.stringify(files)).digest('hex'), files};
writeFileSync(path.join(context, 'packaging-source.json'), JSON.stringify(source, null, 2)+'\n');
writeFileSync(path.join(output, 'source.json'), JSON.stringify(source, null, 2)+'\n');
// Keep fresh snapshot mtimes: BuildKit's local metadata differ must notice
// same-size edits at the same commit epoch. The exporter clamps these newer
// timestamps to SOURCE_DATE_EPOCH; mutable stage caches are epoch-bound.
const images = {};
rmSync(path.join(output,'images.json'),{force:true});
rmSync(path.join(output,'verification.json'),{force:true});
for (const target of targets.length ? targets : ['platform','proxy']) {
    if (!['platform','proxy','browser-fixture','worker-opentofu','worker-terraform'].includes(target)) throw Error('Unknown package target');
    const tag = `${tagPrefix}-${target}:local`;
    const worker=target.startsWith('worker-') ? JSON.parse(readFileSync(path.join(context,'deploy/worker/runtimes.json'),'utf8')).runtimes.find(item=>item.engine===target.slice(7)) : undefined;
    if(worker && (worker.arch!=='linux_amd64' || !/@sha256:[a-f0-9]{64}$/.test(worker.base_image_ref)))throw Error('Worker base image and architecture must be pinned');
    const buildArgs=worker?Object.entries({ENGINE:worker.engine,ENGINE_VERSION:worker.version,ENGINE_URL:worker.engine_archive.url,ENGINE_SHA256:worker.engine_archive.sha256,ENGINE_MEMBER:worker.engine_archive.member,BASE_IMAGE:worker.base_image_ref,SOURCE_REVISION:source.revision,SOURCE_SHA256:source.source_sha256}).flatMap(([key,value])=>['--build-arg',`${key}=${value}`]):[];
    const archive=path.join(output,`${target}.tar`), metadataFile=path.join(output,`${target}.build.json`);
    rmSync(metadataFile,{force:true});
    await docker(['buildx','build','--provenance=false',`--output=type=docker,dest=${archive},rewrite-timestamp=true`,'--metadata-file',metadataFile,'--platform=linux/amd64',...(noCache?['--no-cache']:[]),'--target',worker?'worker':target,'-f',path.join(context,worker?'deploy/worker/Dockerfile':'deploy/platform/Dockerfile'),...buildArgs,'--build-arg',`SOURCE_DATE_EPOCH=${epoch}`,'--label',`org.opencontainers.image.revision=${source.revision}`,'--label',`io.cosmoformer.source.sha256=${source.source_sha256}`,'--label',`io.cosmoformer.source.dirty=${source.dirty}`,'-t',tag,context],{quiet:false});
    await docker(['load','--input',archive],{quiet:false});
    const inspected=JSON.parse(await docker(['image','inspect',tag]))[0];
    images[target]={image_id:inspected.Id,repo_digests:inspected.RepoDigests,architecture:inspected.Architecture,archive_sha256:createHash('sha256').update(readFileSync(archive)).digest('hex'),build_metadata:JSON.parse(readFileSync(metadataFile,'utf8'))};
    // Record a known name before creation: cancellation may lose Docker's ID response.
    container=`cosmoformer-package-${randomUUID()}`;
    writeFileSync(resources,JSON.stringify({container},null,2)+'\n',{mode:0o600});
    const created=(await docker(['create','--name',container,'--network=none',tag])).trim();
    if(!created)throw Error('Package Docker create returned no container identity');
    container=created;
    writeFileSync(resources,JSON.stringify({container},null,2)+'\n',{mode:0o600});
    {
        const checksum=path.join(output,`${target}.SHA256SUMS`);
        await docker(['cp',`${container}:/usr/local/share/cosmoformer/SHA256SUMS`,checksum]);
        images[target].checksums=readFileSync(checksum,'utf8').trim().split('\n');
        const embedded=path.join(output,`${target}.source.json`);
        await docker(['cp',`${container}:/usr/local/share/cosmoformer/source.json`,embedded]);
        const expected=worker?{revision:source.revision,source_sha256:source.source_sha256}:source;
        if(!isDeepStrictEqual(JSON.parse(readFileSync(embedded,'utf8')),expected))throw Error(`Embedded package source identity disagrees with admitted source: ${target}`);
        const license=path.join(output,`${target}.LICENSE`);
        await docker(['cp',`${container}:/usr/local/share/cosmoformer/licenses/Cosmoformer-MIT`,license]);
        if(createHash('sha256').update(readFileSync(license)).digest('hex')!==source.files.LICENSE)throw Error(`Embedded package license disagrees with admitted source: ${target}`);
    }
    await removeContainer();

}
const receipt = {source_sha256:source.source_sha256, revision:source.revision, dirty:source.dirty, source_date_epoch:source.source_date_epoch, images};
writeFileSync(path.join(output,'images.json'), JSON.stringify(receipt,null,2)+'\n');
console.log(`Local package identities recorded in ${path.relative(root,output)}; nothing published.`);
return receipt;

} catch(error) {failure=error;throw error;} finally {
    try {await removeContainer({signal:AbortSignal.timeout(10000)});}
    catch(error) {
        const cleanupError=Error(`Package container cleanup failed; owned resource recorded in ${resources}`,{cause:error});
        if(failure)throw new AggregateError([failure,cleanupError],'Package build and container cleanup failed',{cause:failure});
        throw cleanupError;
    } finally {
        try {rmSync(lock,{recursive:true,force:true});}
        finally {process.removeListener('SIGINT',onINT);process.removeListener('SIGTERM',onTERM);}
    }
}
}
if(process.argv[1] && path.resolve(process.argv[1])===fileURLToPath(import.meta.url))await buildPackages(process.argv.slice(2));
