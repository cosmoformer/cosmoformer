// Only disposable local containers, synthetic inputs and a separately marked DB.
import {execFileSync} from 'node:child_process';
import {randomUUID, randomBytes} from 'node:crypto';
import {chmodSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync} from 'node:fs';
import path from 'node:path';
export const root = path.resolve(import.meta.dirname, '..');
export const helperImage = 'nginx:1.28-alpine@sha256:a8b39bd9cf0f83869a2162827a0caf6137ddf759d50a171451b335cecc87d236';
export function docker(args, options = {}) { return execFileSync('docker', args, {cwd: root, encoding:'utf8', timeout:60000, maxBuffer:16*1024*1024, ...options}).trim(); }
export const delay = ms => new Promise(resolve => setTimeout(resolve, ms));
export function writeBrowserCredentials(file, bytes) {
    // Never replace credentials belonging to another active or interrupted run.
    writeFileSync(file, bytes, {mode:0o600,flag:'wx'});
}
export async function fixture({port=5174}={}) {
    const id = `cf-package-${randomUUID()}`, dir = path.join(root,'.build/test-runs',id);
    mkdirSync(dir,{recursive:true,mode:0o700});
    const env = {...process.env, COSMOFORMER_TEST_DIR:dir};
    for (const key of Object.keys(env)) if (key.startsWith('COSMOFORMER_') && key !== 'COSMOFORMER_TEST_DIR') delete env[key];
    const containers=[], volumes=[], networks=[];
    const receipt=()=>writeFileSync(path.join(dir,'package-resources.json'),JSON.stringify({containers,volumes,networks}),{mode:0o600});
    const f = {id,dir,env,containers,volumes,networks,origin:`https://localhost:${port}`};
    f.images=JSON.parse(readFileSync(path.join(root,'.build/package/images.json'),'utf8'));
    const resolved=new Map();
    f.image=target=>{
        if(resolved.has(target))return resolved.get(target);
        const expected=f.images.images[target]?.image_id;
        if(!/^sha256:[0-9a-f]{64}$/.test(expected??''))throw Error(`Build required package target: ${target}`);
        const actual=JSON.parse(docker(['image','inspect',expected]))[0];
        if(actual.Config.Labels['io.cosmoformer.source.sha256']!==f.images.source_sha256 || actual.Architecture!=='amd64')throw Error('Package image/source identity mismatch');
        resolved.set(target,expected);return expected;
    };

    f.volume = suffix => {const name=`${id}-${suffix}`;volumes.push(name);receipt();docker(['volume','create','--label',`cosmoformer.package=${id}`,name]);return name;};
    f.run = (suffix,args,options) => {const name=`${id}-${suffix}`;containers.push(name);receipt();return docker(['run','--name',name,'--label',`cosmoformer.package=${id}`, ...args],options);};
    f.put = (volume,file,value,mode='600') => docker(['run','--rm','-i','--network=none','--mount',`type=volume,src=${volume},dst=/inputs`,'--entrypoint','sh',helperImage,'-ec',`umask 077; chown 10001:10001 /inputs; chmod 700 /inputs; cat > /inputs/${file}; chown 10001:10001 /inputs/${file}; chmod ${mode} /inputs/${file}`],{input:value});
    f.read = (volume,file) => docker(['run','--rm','--network=none','--mount',`type=volume,src=${volume},dst=/inputs,readonly`,'--entrypoint','cat',helperImage,`/inputs/${file}`]);
    f.stop = async () => {
        const failed=[];
        for (const name of [...containers].reverse()) try {docker(['rm','-f',name],{stdio:'pipe'});}catch{failed.push(`container ${name}`);}
        for (const name of [...volumes].reverse()) try {docker(['volume','rm',name],{stdio:'pipe'});}catch{failed.push(`volume ${name}`);}
        try {execFileSync(process.execPath,['scripts/dev.mjs','test-stop'],{cwd:root,env,stdio:'pipe',timeout:60000});}catch{failed.push('owned PostgreSQL container');}
        // PostgreSQL also owns an endpoint on this network; stop it first.
        for (const name of [...networks].reverse()) try {docker(['network','rm',name],{stdio:'pipe'});}catch{failed.push(`network ${name}`);}
        if(failed.length)throw Error(`Cleanup incomplete (${failed.join(', ')}); retain owned resource receipt: ${dir}`);
        rmSync(dir,{recursive:true,force:true});
    };
    try {
        execFileSync(process.execPath,['scripts/dev.mjs','test-start'],{cwd:root,env,stdio:'pipe',timeout:90000});
        f.pg=readFileSync(path.join(dir,'container'),'utf8').trim();
        f.network=`${id}-isolated`;networks.push(f.network);receipt();
        docker(['network','create','--internal','--label',`cosmoformer.package=${id}`,f.network]);
        docker(['network','connect',f.network,f.pg]);
        docker(['network','disconnect','bridge',f.pg]);

        f.inputs=f.volume('inputs');f.socket=f.volume('socket');f.tls=f.volume('tls');
        f.put(f.inputs,'marker',readFileSync(path.join(dir,'marker')));
        f.put(f.inputs,'encryption.key',randomBytes(32));
        execFileSync('/usr/bin/openssl',['req','-x509','-newkey','rsa:2048','-nodes','-keyout',path.join(dir,'key.pem'),'-out',path.join(dir,'cert.pem'),'-days','1','-subj','/CN=localhost','-addext','subjectAltName=DNS:localhost,IP:127.0.0.1'],{stdio:'pipe'});
        chmodSync(path.join(dir,'key.pem'),0o600);
        f.put(f.tls,'key.pem',readFileSync(path.join(dir,'key.pem')));
        f.put(f.tls,'cert.pem',readFileSync(path.join(dir,'cert.pem')));
        writeFileSync(path.join(dir,'access.conf'),'allow all;\n',{mode:0o644});
        // All platform/fixture DB traffic stays in this owned PG network namespace.
        f.common=['--platform=linux/amd64','--read-only','--user','10001:10001','--cap-drop=ALL','--security-opt=no-new-privileges','--cpus=2','--memory=1g','--pids-limit=256','--tmpfs','/tmp:rw,noexec,nosuid,nodev,size=512m,uid=10001,gid=10001,mode=0700','--network',`container:${f.pg}`,'--mount',`type=volume,src=${f.inputs},dst=/run/inputs,readonly`];
        f.apiEnv=['-e',`COSMOFORMER_ORIGIN=${f.origin}`,'-e','COSMOFORMER_UNIX_SOCKET=/run/cosmoformer/api.sock','-e','COSMOFORMER_DATABASE_DSN_FILE=/run/inputs/runtime.dsn','-e','COSMOFORMER_ENCRYPTION_KEY_FILE=/run/inputs/encryption.key'];
        f.proxy = (port,{name='proxy',tls=f.tls}={}) => f.run(name,['-d','--platform=linux/amd64','--read-only','--user','10001:10001','--cap-drop=ALL','--security-opt=no-new-privileges','--cpus=1','--memory=256m','--pids-limit=128','--tmpfs','/tmp:rw,noexec,nosuid,nodev,size=32m,uid=10001,gid=10001,mode=0700','-p',`127.0.0.1:${port}:8443`,'--mount',`type=volume,src=${f.socket},dst=/run/cosmoformer,readonly,volume-nocopy`,'--mount',`type=volume,src=${tls},dst=/run/tls,readonly`,'--mount',`type=bind,src=${path.join(dir,'access.conf')},dst=/etc/nginx/private-access.conf,readonly`,f.image('proxy')]);
        return f;
    } catch(e) {await f.stop();throw e;}
}
export async function waitHTTPS(url, {run=execFileSync, sleep=delay, attempts=120, signal}={}) {
    for(let i=0;i<attempts;i++) {
        signal?.throwIfAborted();
        try {
            const result=run('/usr/bin/curl',['--silent','--insecure','--fail','--write-out','\n%{http_code}',url],{timeout:2000,encoding:'utf8'});
            if(result==='ok\n\n200')return;
        }catch{}
        await sleep(250);
    }
    signal?.throwIfAborted();
    throw Error(`Packaged HTTPS did not become ready: ${url}`);
}
