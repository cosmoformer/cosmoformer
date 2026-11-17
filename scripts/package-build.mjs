// Freeze the public build inputs once so both images consume exactly one tree.
import {execFileSync} from 'node:child_process';
import {createHash} from 'node:crypto';
import {existsSync, lstatSync, mkdirSync, readFileSync, rmSync, writeFileSync} from 'node:fs';
import path from 'node:path';
import {fileURLToPath} from 'node:url';
const root = path.resolve(import.meta.dirname, '..'), output = path.join(root, '.build/package');
const run = (command, args, options = {}) => execFileSync(command, args, {cwd: root, encoding: 'utf8', maxBuffer: 16*1024*1024, ...options});
export function buildPackages(targets = []) {
const lock=path.join(root,'.build/package-build.lock');
mkdirSync(path.dirname(lock),{recursive:true});
try {mkdirSync(lock,{mode:0o700});}catch{throw Error('A package build is already recorded; inspect .build/package-build.lock before another build');}
writeFileSync(path.join(lock,'pid'),String(process.pid),{mode:0o600});
try {
const context = path.join(output, 'source');
mkdirSync(output, {recursive: true});
rmSync(context, {recursive: true, force: true});
mkdirSync(context);
const files = {};
const excluded = /(^|\/)(node_modules|build|\.generated|\.react-router|test-results|playwright-report|\.DS_Store)(\/|$)|(^|\/)\.env(?:\.|$)|\.(?:pem|key|dsn|tfstate|tfplan)$/;
const tracked = run('git', ['ls-files', '-z']).split('\0').filter(Boolean);
const added = run('git', ['ls-files', '--others', '--exclude-standard', '-z']).split('\0').filter(file => /^(?:deploy\/platform\/[^/]+|deploy\/worker\/(?:compose.yaml|\.env.example)|scripts\/(?:package-[^/]+|go-coverage(?:\.test)?)\.mjs|internal\/(?:store\/(?:migrations\/017_auth_throttle_capacity.sql|auth_capacity_test.go)|httpapi\/auth_capacity_integration_test.go)|tests\/acceptance\/packaged_test.go|web\/tests\/(?:fixtures\/test.ts|csp-monitor.spec.ts))$/.test(file));
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
const source = {schema_version: 1, revision: run('git', ['rev-parse','HEAD']).trim(), dirty: run('git',['status','--porcelain']).length > 0, source_sha256: createHash('sha256').update(JSON.stringify(files)).digest('hex'), files};
writeFileSync(path.join(context, 'packaging-source.json'), JSON.stringify(source, null, 2)+'\n');
writeFileSync(path.join(output, 'source.json'), JSON.stringify(source, null, 2)+'\n');
const epoch = run('git', ['show','-s','--format=%ct','HEAD']).trim();
const images = {};
rmSync(path.join(output,'images.json'),{force:true});
rmSync(path.join(output,'verification.json'),{force:true});
for (const target of targets.length ? targets : ['platform','proxy']) {
    if (!['platform','proxy','browser-fixture','worker-opentofu','worker-terraform'].includes(target)) throw Error('Unknown package target');
    const tag = `cosmoformer-${target}:local`;
    const worker=target.startsWith('worker-') ? JSON.parse(readFileSync(path.join(context,'deploy/worker/runtimes.json'),'utf8')).runtimes.find(item=>item.engine===target.slice(7)) : undefined;
    if(worker && (worker.arch!=='linux_amd64' || !/@sha256:[a-f0-9]{64}$/.test(worker.base_image_ref)))throw Error('Worker base image and architecture must be pinned');
    const buildArgs=worker?Object.entries({ENGINE:worker.engine,ENGINE_VERSION:worker.version,ENGINE_URL:worker.engine_archive.url,ENGINE_SHA256:worker.engine_archive.sha256,ENGINE_MEMBER:worker.engine_archive.member,BASE_IMAGE:worker.base_image_ref,SOURCE_REVISION:source.revision,SOURCE_SHA256:source.source_sha256}).flatMap(([key,value])=>['--build-arg',`${key}=${value}`]):[];
    const archive=path.join(output,`${target}.tar`), metadataFile=path.join(output,`${target}.build.json`);
    rmSync(metadataFile,{force:true});
    run('docker',['build','--provenance=false',`--output=type=docker,dest=${archive},rewrite-timestamp=true`,'--metadata-file',metadataFile,'--platform=linux/amd64',...(process.env.COSMOFORMER_PACKAGE_NO_CACHE==='1'?['--no-cache']:[]),'--target',worker?'worker':target,'-f',path.join(context,worker?'deploy/worker/Dockerfile':'deploy/platform/Dockerfile'),...buildArgs,'--build-arg',`SOURCE_DATE_EPOCH=${epoch}`,'--label',`org.opencontainers.image.revision=${source.revision}`,'--label',`io.cosmoformer.source.sha256=${source.source_sha256}`,'-t',tag,context], {stdio:'inherit'});
    run('docker',['load','--input',archive],{stdio:'inherit'});
    const inspected=JSON.parse(run('docker',['image','inspect',tag]))[0];
    images[target]={image_id:inspected.Id,repo_digests:inspected.RepoDigests,architecture:inspected.Architecture,archive_sha256:createHash('sha256').update(readFileSync(archive)).digest('hex'),build_metadata:JSON.parse(readFileSync(metadataFile,'utf8'))};
    const container=run('docker',['create','--network=none',tag]).trim();
    try {
        const checksum=path.join(output,`${target}.SHA256SUMS`);
        run('docker',['cp',`${container}:/usr/local/share/cosmoformer/SHA256SUMS`,checksum]);
        images[target].checksums=readFileSync(checksum,'utf8').trim().split('\n');
    } finally {run('docker',['rm',container]);}

}
writeFileSync(path.join(output,'images.json'), JSON.stringify({source_sha256:source.source_sha256, revision:source.revision, dirty:source.dirty, images},null,2)+'\n');
console.log('Local package identities recorded in .build/package; nothing published.');

} finally {rmSync(lock,{recursive:true,force:true});}
}
if(process.argv[1] && path.resolve(process.argv[1])===fileURLToPath(import.meta.url))buildPackages(process.argv.slice(2));
