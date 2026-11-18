// Exercise actual packaged binaries and HTTPS proxy, never a deployed service.
import assert from 'node:assert/strict';
import https from 'node:https';
import {execFileSync} from 'node:child_process';
import {createHash, randomUUID} from 'node:crypto';
import {readFileSync, writeFileSync, rmSync} from 'node:fs';
import path from 'node:path';
import {packageTargets} from './package-inventory.mjs';
import {fixture, docker, delay, root, waitHTTPS} from './package-fixture.mjs';
const schemaVersion=Number(readFileSync(path.join(root,'internal/store/store.go'),'utf8').match(/^const SchemaVersion int32 = (\d+)$/m)?.[1]);
const f=await fixture({port:5175,targets:packageTargets}), results=[];
rmSync(path.join(root,'.build/package/verification.json'),{force:true});
const check=(name, fn)=>{fn();results.push(name);console.log(`PASS ${name}`);};
const sql=statement=>docker(['exec','-i',f.pg,'psql','-U','postgres','-d','cosmoformer_package','-At','-v','ON_ERROR_STOP=1'],{input:statement});
let cookie='',csrf='';
function request(url, {method='GET',body,headers={},tls={}}={}) {
    return new Promise((resolve,reject)=>{
        const data=body===undefined?undefined:typeof body==='string'?body:JSON.stringify(body);
        const r=https.request(f.origin+url,{method,ca:readFileSync(path.join(f.dir,'cert.pem')),rejectUnauthorized:true,agent:false,...tls,headers:{...(data===undefined?{}:{'Content-Type':'application/json','Content-Length':Buffer.byteLength(data)}),...headers}},response=>{
            const parts=[];response.on('data',part=>parts.push(part));response.on('end',()=>resolve({status:response.statusCode,headers:response.headers,text:Buffer.concat(parts).toString()}));
        });
        r.setTimeout(45000,()=>r.destroy(Error('HTTPS fixture timeout')));r.on('error',reject);r.end(data);
    });
}
async function api(method,url,body,status=200) {
    const r=await request('/api/v1'+url,{method,body,headers:{Cookie:cookie,Origin:f.origin,'X-Cosmoformer-Request':'browser','X-CSRF-Token':csrf,'Idempotency-Key':randomUUID()}});
    assert.equal(r.status,status,`${method} ${url}: ${r.status}`);
    if(r.headers['set-cookie'])cookie=r.headers['set-cookie'][0].split(';')[0];
    assert.equal(r.headers['cache-control'],'no-store');
    return JSON.parse(r.text);
}
try {
    for(const kind of ['platform','worker']) {
        const example=readFileSync(path.join(root,`deploy/${kind}/.env.example`),'utf8');
        const values=Object.fromEntries(example.split('\n').filter(line=>line&&!line.startsWith('#')).map(line=>[line.split('=')[0],'/synthetic/input']));
        Object.assign(values,{COMPOSE_PROJECT_NAME:'cosmoformer-package-check',PLATFORM_IMAGE:'sha256:'+'a'.repeat(64),PROXY_IMAGE:'sha256:'+'b'.repeat(64),WORKER_IMAGE:'sha256:'+'c'.repeat(64),COSMOFORMER_ORIGIN:f.origin,COSMOFORMER_PLATFORM_ORIGIN:f.origin,HTTPS_BIND_IP:'127.0.0.1',HTTPS_PORT:'5174',POSTGRES_VOLUME:'synthetic-prepared-db',ARTIFACT_BUCKET:'synthetic',ARTIFACT_REGION:'eu-west-1',ARTIFACT_EXPECTED_OWNER:'111122223333',COSMOFORMER_RUNTIME_ID:randomUUID(),COSMOFORMER_IMAGE_DIGEST:'sha256:'+'a'.repeat(64),COSMOFORMER_EXPECTED_BOOTSTRAP_PRINCIPAL:'arn:aws:iam::111122223333:role/synthetic'});
        const file=path.join(f.dir,`${kind}.env`);writeFileSync(file,Object.entries(values).map(([k,v])=>`${k}=${v}`).join('\n'),{mode:0o600});
        const config=JSON.parse(docker(['compose','--env-file',file,'-f',`deploy/${kind}/compose.yaml`,'config','--format','json']));
        check(`${kind} Compose configuration and boundaries`,()=>{
            for(const service of Object.values(config.services)) {
                assert.equal(service.read_only,true);assert.ok(service.cap_drop.includes('ALL'));
                assert.equal(service.privileged??false,false);assert.ok(service.security_opt.includes('no-new-privileges:true'));
                assert.equal(service.build,undefined);assert.equal(service.restart,undefined);
                for(const mount of service.volumes??[])assert.ok(!mount.source?.includes('docker.sock'));
            }
            if(kind==='platform') {assert.equal(config.services.platform.ports,undefined);assert.equal(config.services.postgres.ports,undefined);assert.equal(config.services.nginx.ports.length,1);assert.deepEqual(config.services.postgres.entrypoint,['postgres']);assert.equal(config.volumes.database.external,true);}
            else {assert.equal(config.services.worker.ports,undefined);assert.equal(config.services.worker.init,true);assert.ok(config.services.worker.tmpfs.some(m=>m.includes('/var/lib/cosmoformer/attempts:rw,exec')));assert.equal(Object.keys(config.services.worker.environment).some(k=>/DSN|ENCRYPTION/.test(k)),false);}
        });
        const blank=Object.fromEntries(Object.keys(values).map(key=>[key,'']));
        check(`${kind} Compose rejects missing operator inputs`,()=>assert.throws(()=>docker(['compose','--env-file','/dev/null','-f',`deploy/${kind}/compose.yaml`,'config','--quiet'],{env:{...process.env,...blank},stdio:'pipe'})));
    }
    docker(['exec','-i',f.pg,'psql','-U','postgres','-d','cosmoformer_test_control','-v','ON_ERROR_STOP=1'],{input:'CREATE DATABASE cosmoformer_package OWNER cosmoformer_owner;'});
    f.put(f.inputs,'runtime.dsn','postgres://cosmoformer_app@127.0.0.1:5432/cosmoformer_package?sslmode=disable');
    f.put(f.inputs,'migration.dsn','postgres://cosmoformer_owner@127.0.0.1:5432/cosmoformer_package?sslmode=disable');
    const operatorArgs=[...f.common,'-e','COSMOFORMER_MIGRATION_DSN_FILE=/run/inputs/migration.dsn','-e','COSMOFORMER_ENCRYPTION_KEY_FILE=/run/inputs/encryption.key',f.image('platform')];
    const migrations=f.run('inventory',[...operatorArgs,'migrate','--list']);
    check('complete embedded forward migration inventory',()=>assert.equal(migrations.split('\n').length,schemaVersion));
    check('missing required startup input fails closed',()=>assert.throws(()=>f.run('missing',[...f.common,f.image('platform'),'serve']),/Command failed/));
    f.run('migration',[...operatorArgs,'migrate']);
    f.run('key',[...operatorArgs,'register-key']);
    // Bootstrap writes only to its explicit bounded private output; no runtime
    // container ever receives the migration credential or bootstrap authority.
    f.run('bootstrap',[...f.common,'--mount',`type=volume,src=${f.inputs},dst=/operator`,'-e','COSMOFORMER_MIGRATION_DSN_FILE=/run/inputs/migration.dsn',f.image('platform'),'bootstrap-admin','--username','packageadmin','--setup-code-file','/operator/setup-code']);
    // A distinct input volume keeps the owner DSN off the running API.
    const runtime=f.volume('runtime');
    f.put(runtime,'runtime.dsn','postgres://cosmoformer_app@127.0.0.1:5432/cosmoformer_package?sslmode=disable');
    // Binary key bytes cannot pass through a text helper; copy them within the
    // two owned volumes without returning them to the terminal.
    docker(['run','--rm','--network=none','--mount',`type=volume,src=${f.inputs},dst=/from,readonly`,'--mount',`type=volume,src=${runtime},dst=/to`,'--entrypoint','sh',f.image('proxy'),'-ec','cp /from/encryption.key /to/encryption.key; chown 10001:10001 /to/encryption.key; chmod 600 /to/encryption.key']);
    const apiArgs=f.common.map(value=>value===`type=volume,src=${f.inputs},dst=/run/inputs,readonly`?`type=volume,src=${runtime},dst=/run/inputs,readonly`:value);
    f.run('api',['-d',...apiArgs,...f.apiEnv,'--mount',`type=volume,src=${f.socket},dst=/run/cosmoformer`,f.image('platform')]);
    f.proxy(5175);
    await waitHTTPS(`${f.origin}/health/ready`);
    const trusted=await request('/health/ready');
    check('HTTPS accepts the explicitly trusted localhost certificate',()=>assert.equal(trusted.status,200));
    await assert.rejects(request('/health/ready',{tls:{ca:[]}}),{code:'DEPTH_ZERO_SELF_SIGNED_CERT'});
    check('HTTPS rejects an untrusted certificate',()=>{});
    await assert.rejects(request('/health/ready',{tls:{servername:'wrong.invalid'}}),{code:'ERR_TLS_CERT_ALTNAME_INVALID'});
    check('HTTPS rejects a certificate for another hostname',()=>{});
    // Exercise expiry through the actual Nginx image with a separate synthetic
    // certificate and listener. No host clock or shared proxy is changed.
    const openssl=args=>execFileSync('/usr/bin/openssl',args,{cwd:f.dir,stdio:'pipe',timeout:10000});
    openssl(['req','-new','-key','key.pem','-subj','/CN=localhost','-out','expired.csr']);
    writeFileSync(path.join(f.dir,'expired-index'),'');writeFileSync(path.join(f.dir,'expired-serial'),'01\n');
    writeFileSync(path.join(f.dir,'expired.cnf'),'[ca]\ndefault_ca=local\n[local]\ndatabase=expired-index\nserial=expired-serial\nnew_certs_dir=.\ndefault_md=sha256\npolicy=local_policy\n[local_policy]\ncommonName=supplied\n[server]\nsubjectAltName=DNS:localhost,IP:127.0.0.1\n');
    openssl(['ca','-config','expired.cnf','-selfsign','-keyfile','key.pem','-cert','cert.pem','-in','expired.csr','-startdate','20200101000000Z','-enddate','20200102000000Z','-extensions','server','-batch','-notext','-out','expired.pem']);
    const expired=f.volume('expired-tls'),expiredCert=readFileSync(path.join(f.dir,'expired.pem'));
    f.put(expired,'key.pem',readFileSync(path.join(f.dir,'key.pem')));f.put(expired,'cert.pem',expiredCert);
    f.proxy(5176,{name:'expired-proxy',tls:expired});
    await waitHTTPS('https://localhost:5176/health/ready');
    await assert.rejects(request('/health/ready',{tls:{port:5176,ca:expiredCert}}),{code:'CERT_HAS_EXPIRED'});
    check('HTTPS rejects an expired certificate even when explicitly trusted',()=>{});
    docker(['stop','--time=15',`${f.id}-expired-proxy`]);
    const info=JSON.parse(docker(['inspect',`${f.id}-api`]))[0];
    check('platform non-root, read-only, bounded writable mounts and no host ports',()=>{
        assert.equal(info.Config.User,'10001:10001');assert.equal(info.HostConfig.ReadonlyRootfs,true);
        assert.deepEqual(info.HostConfig.CapDrop,['ALL']);assert.equal(info.HostConfig.Privileged,false);
        assert.equal(info.HostConfig.Memory,1073741824);assert.equal(info.HostConfig.PidsLimit,256);
        assert.equal(info.HostConfig.SecurityOpt.includes('no-new-privileges'),true);
        assert.equal(Object.keys(info.NetworkSettings.Ports??{}).length,0);
        assert.equal(info.Mounts.some(m=>m.Destination.includes('docker.sock')),false);
        assert.equal(info.Mounts.find(m=>m.Destination==='/run/inputs').RW,false);
        assert.equal(info.Config.Env.some(v=>v.startsWith('COSMOFORMER_MIGRATION_DSN_FILE=')),false);
    });
    for(const [url,status] of [['/health/live',200],['/api/v1/session',401],['/api/v1/absent',401],['/worker/v1/absent',404],['/webhooks/bitbucket/no/no',405],['/oauth/bitbucket/callback',422]]) {
        const r=await request(url);check(`real upstream route ${url}`,()=>{assert.equal(r.status,status);assert.equal(r.headers['cache-control'],'no-store');assert.ok(!r.text.startsWith('<!DOCTYPE'));});
    }
    for(const url of ['/','/login','/organizations','/account/reset','/o/one/p/two/runs/three']) {
        const r=await request(url);check(`SPA refresh ${url}`,()=>{assert.equal(r.status,200);assert.match(r.text,/<!DOCTYPE html>/);assert.equal(r.headers['cache-control'],'no-store');});
    }
    const html=await request('/index.html'), asset=html.text.match(/href="(\/assets\/[^\"]+\.css)"/)[1];
    const hashed=await request(asset);
    check('hashed asset immutable caching',()=>assert.match(hashed.headers['cache-control'],/max-age=31536000, immutable/));
    for(const url of ['/assets/missing.js','/brand/missing.png','/missing.js','/not-a-browser-route']) {const r=await request(url);check(`missing asset/path ${url}`,()=>assert.equal(r.status,404));}
    check('CSP uses exact bootstrap hashes without script inline/eval escape',()=>{
        const csp=html.headers['content-security-policy'];assert.ok(csp);assert.doesNotMatch(csp,/unsafe-eval/);
        assert.doesNotMatch(csp.split(';').find(v=>v.trim().startsWith('script-src ')),/unsafe-inline/);
        for(const [,script] of html.text.matchAll(/<script\b[^>]*>([\s\S]*?)<\/script>/g))assert.ok(csp.includes(`'sha256-${createHash('sha256').update(script).digest('base64')}'`));
        assert.match(csp,/style-src-attr 'unsafe-inline'/);
    });
    const setupCode=f.read(f.inputs,'setup-code');
    await api('POST','/account/setup',{code:setupCode,password:'synthetic package password 2026'});
    await api('POST','/session/login',{username:'packageadmin',password:'synthetic package password 2026'});
    const session=await api('GET','/session');csrf=session.csrf_token;
    check('packaged executable setup and authenticated session',()=>assert.equal(session.actor.username,'packageadmin'));
    const denied=await request('/api/v1/organizations',{method:'POST',body:{name:'forged'},headers:{Cookie:cookie,Origin:'https://evil.invalid','X-Cosmoformer-Request':'browser','X-CSRF-Token':csrf,'X-Forwarded-Host':'localhost:5175','X-Forwarded-Proto':'https','Forwarded':'host=localhost:5175;proto=https'}});
    check('forwarding headers cannot replace canonical browser origin',()=>assert.equal(denied.status,403));
    const org=(await api('POST','/organizations',{name:'Packaged synthetic organization'})).id;
    const pool=(await api('POST',`/organizations/${org}/worker-pools`,{name:'Synthetic idle pool'})).id;
    const digest='sha256:'+'a'.repeat(64);
    const runtimeID=(await api('POST','/admin/runtimes',{engine:'opentofu',version:'1.13.8',architecture:'amd64',image_digest:digest,approved:true})).id;
    const worker=await api('POST',`/organizations/${org}/workers`,{name:'Synthetic poll worker',pool_id:pool,tags:[]});
    await api('POST',`/organizations/${org}/workers/${worker.id}/resume`,{expected_revision:1});
    const agent=randomUUID();
    const admission=await request('/worker/v1/admit',{method:'POST',headers:{Authorization:`Bearer ${worker.token}`},body:{protocol_version:1,worker_id:worker.id,agent_instance:agent,runtime_id:runtimeID,engine:'opentofu',version:'1.13.8',arch:'amd64',image_digest:digest,git_package_version:'1:2.47.3-0+deb13u1',git_version:'git version 2.47.3',supported_work_phases:['preflight'],clock_source:'boottime',clock_ok:true}});
    assert.equal(admission.status,200);
    const generation=JSON.parse(admission.text).generation, started=Date.now();
    const poll=await request('/worker/v1/poll',{method:'POST',headers:{Authorization:`Bearer ${worker.token}`},body:{worker_id:worker.id,generation,agent_instance:agent,request_id:randomUUID(),wait_seconds:20}});
    check('actual authenticated 20-second worker long poll through Nginx',()=>{assert.equal(poll.status,200);assert.equal(JSON.parse(poll.text).assignment,null);assert.ok(Date.now()-started>=19500);});
    for(const [url,size] of [['/api/v1/session/login',65537],['/worker/v1/poll',1048577],['/webhooks/bitbucket/a/b',1048577],['/worker/v1/attempts/11111111-1111-4111-8111-111111111111/artifacts/22222222-2222-4222-8222-222222222222',67108866]]) {
        const r=await request(url,{method:'PUT',headers:{'Content-Length':size}});check(`request bound ${size} ${url}`,()=>assert.equal(r.status,413));
    }
    // Same peer and changing spoofed identities must share the authentication
    // limiter. Username failures are different on every request.
    let last;
    for(let i=0;i<24;i++)last=await request('/api/v1/session/login',{method:'POST',body:{username:`missing${i}`,password:'synthetic wrong password'},headers:{Origin:f.origin,'X-Cosmoformer-Request':'browser','X-Cosmoformer-Client-IP':`203.0.113.${i+1}`,'X-Forwarded-For':`198.51.100.${i+1}`}});
    check('spoofed client addresses cannot evade per-peer login throttling',()=>assert.equal(last.status,429));
    sql('UPDATE cosmoformer.installation SET maintenance=true WHERE id;');
    docker(['stop','--time=15',`${f.id}-api`]);
    const stoppedInfo=JSON.parse(docker(['inspect',`${f.id}-api`]))[0];
    check('SIGTERM exits packaged PID 1 gracefully',()=>assert.equal(stoppedInfo.State.ExitCode,0));
    const failure=await request('/api/v1/session');check('upstream failure remains 502',()=>assert.equal(failure.status,502));
    const callbackFailure=await request('/oauth/bitbucket/callback?code=packaging-code-canary&state=packaging-state-canary');
    check('upstream errors do not log OAuth callback credentials',()=>{assert.equal(callbackFailure.status,502);assert.doesNotMatch(docker(['logs',`${f.id}-proxy`]),/packaging-(?:code|state)-canary/);});

    docker(['start',`${f.id}-api`]);await waitHTTPS(`${f.origin}/health/ready`);
    check('restart preserves maintenance and does not replay browser actions',()=>{assert.equal(sql('SELECT maintenance FROM cosmoformer.installation WHERE id;'),'t');assert.equal(sql('SELECT count(*) FROM cosmoformer.organizations;'),'1');});
    // Deny-all replacement proves real TCP peer ACL enforcement, with forged
    // forwarding headers unable to create an approved peer.
    writeFileSync(path.join(f.dir,'access.conf'),'deny all; \n');
    await delay(500);docker(['exec',`${f.id}-proxy`,'nginx','-t']);docker(['exec',`${f.id}-proxy`,'nginx','-s','reload']);await delay(300);
    const restricted=await request('/login',{headers:{'X-Forwarded-For':'127.0.0.1','X-Cosmoformer-Client-IP':'127.0.0.1'}});
    check('restricted access denies spoofed forwarding claims',()=>assert.equal(restricted.status,403));
    const webhook='/webhooks/bitbucket/11111111-1111-4111-8111-111111111111/22222222-2222-4222-8222-222222222222';
    const publicDelivery=await request(webhook,{method:'POST',body:{}});
    check('only exact webhook POST reaches signed application handler outside client ACL',()=>{assert.ok(publicDelivery.headers['x-request-id']);assert.notEqual(publicDelivery.status,200);});
    for(const [method,url] of [['GET',webhook],['POST',webhook+'/extra'],['POST','/webhooks/bitbucket/no/no'],['GET','/oauth/bitbucket/callback'],['GET','/worker/v1/poll'],['GET','/api/v1/session']]) {
        const denied=await request(url,{method,body:method==='POST'?{}:undefined});
        check(`public exception does not open ${method} ${url}`,()=>{assert.equal(denied.status,403);assert.equal(denied.headers['x-request-id'],undefined);});
    }

    for(const target of ['platform','proxy']) {
        const image=JSON.parse(docker(['image','inspect',f.image(target)]))[0];
        check(`${target} image runtime identity`,()=>{assert.equal(image.Architecture,'amd64');assert.equal(image.Config.User,'10001:10001');});
    }
    docker(['cp',`${f.id}-api:/usr/local/bin/cosmoformer`,path.join(f.dir,'cosmoformer')]);
    docker(['cp',`${f.id}-api:/usr/local/share/cosmoformer/SHA256SUMS`,path.join(f.dir,'SHA256SUMS')]);
    check('recorded executable SHA256 matches image bytes',()=>assert.ok(readFileSync(path.join(f.dir,'SHA256SUMS'),'utf8').includes(createHash('sha256').update(readFileSync(path.join(f.dir,'cosmoformer'))).digest('hex'))));
    for(const suffix of ['api','proxy']) {
        const file=path.join(f.dir,`${suffix}.tar`);docker(['export','-o',file,`${f.id}-${suffix}`]);
        const names=execFileSync('/usr/bin/tar',['-tf',file],{encoding:'utf8',maxBuffer:16*1024*1024});
        check(`${suffix} runtime excludes build tools, fixtures and source`,()=>{assert.doesNotMatch(names,/(?:^|\/)(?:node_modules|\.git|\.npm)(?:\/|$)|^(?:src|go|fixtures|tests|usr\/local\/go|usr\/local\/bin\/(?:node|npm|go))(?:\/|$)/m);assert.match(names,/licenses\//);});
    }
    for(const engine of ['opentofu','terraform']) {
        const tag=f.image(`worker-${engine}`), binary=engine==='opentofu'?'tofu':'terraform';
        const base=['--platform=linux/amd64','--network=none','--read-only','--cap-drop=ALL','--security-opt=no-new-privileges','--cpus=2','--memory=1g','--pids-limit=128'];
        const profile=JSON.parse(f.run(`worker-${engine}-metadata`,[...base,'--entrypoint','cat',tag,'/usr/local/share/cosmoformer/runtime.json']));
        const hash=f.run(`worker-${engine}-hash`,[...base,'--entrypoint','sha256sum',tag,`/usr/local/bin/${binary}`]).split(/\s+/)[0];
        check(`${engine} worker baked engine identity`,()=>{assert.equal(profile.engine,engine);assert.equal(profile.version,engine==='opentofu'?'1.13.8':'1.17.1');assert.equal(profile.binary_sha256,hash);assert.equal(profile.arch,'amd64');assert.equal(profile.git_package_version,'1:2.47.3-0+deb13u1');});
        const verified=f.run(`worker-${engine}-runtime`,[...base,'--entrypoint','sh',tag,'-ec','test "$(id -u)" = 10001; test "$(id -g)" = 10001; test ! -e /var/run/docker.sock; test ! -e /usr/local/go; test ! -e /fixtures; test -n "$(ls -A /usr/local/share/cosmoformer/licenses/engine)"; cd /usr/local/bin; sha256sum -c /usr/local/share/cosmoformer/SHA256SUMS; git --version']);
        check(`${engine} worker non-root, executable and license contents`,()=>{assert.match(verified,/cosmoformer-worker: OK/);assert.match(verified,/git version 2.47.3/);});
        check(`${engine} worker rejects absent enrollment token`,()=>assert.throws(()=>f.run(`worker-${engine}-missing`,[...base,tag]),/cannot open protected input file/));
    }
    const restartResult=f.run('uncertain-restart',[...f.common,'-e','COSMOFORMER_TEST_PACKAGED=1','-e','COSMOFORMER_TEST_OWNER_DSN=postgres://postgres@127.0.0.1:5432/cosmoformer_test_control?sslmode=disable','-e','COSMOFORMER_TEST_MARKER_FILE=/run/inputs/marker',f.image('browser-fixture'),'-test.run=^TestPackagedRestartPreservesUncertainOwnership$','-test.v','-test.timeout=2m'],{timeout:150000});
    check('packaged executable preserves uncertain permission, reservations and recovery across two graceful restarts',()=>assert.match(restartResult,/--- PASS: TestPackagedRestartPreservesUncertainOwnership/));
    // Exercise the database service's actual non-root/read-only reference after
    // all writers are stopped. Reuse only this test's owned prepared volume.
    docker(['stop','--time=15',`${f.id}-api`,`${f.id}-proxy`]);
    const pgInfo=JSON.parse(docker(['inspect',f.pg]))[0];
    const data=pgInfo.Mounts.find(m=>m.Destination==='/var/lib/postgresql');
    assert.equal(data.Type,'volume');
    // dev.mjs uses --rm: hold its owned anonymous volume before stopping it,
    // then explicitly remove that retained volume during this fixture's cleanup.
    f.volumes.push(data.Name);
    f.run('data-keeper',['-d','--network=none','--read-only','--user=70:70','--cap-drop=ALL','--security-opt=no-new-privileges','--cpus=0.1','--memory=16m','--pids-limit=16','--mount',`type=volume,src=${data.Name},dst=/data,readonly`,'--entrypoint','sleep',f.image('proxy'),'300']);
    docker(['stop','--time=15',f.pg]);
    const postgres='postgres:18.6-alpine@sha256:8a334ab9502c8ccb8d7dec4e83b36b5ce8ed7b396e585f1c86b0e69c416f4fca';
    const pgArgs=['--platform=linux/amd64','--read-only','--user=70:70','--cap-drop=ALL','--security-opt=no-new-privileges','--cpus=2','--memory=1g','--pids-limit=256','--tmpfs','/tmp:rw,noexec,nosuid,nodev,size=64m,uid=70,gid=70,mode=0700','--tmpfs','/var/run/postgresql:rw,noexec,nosuid,nodev,size=16m,uid=70,gid=70,mode=0700'];
    f.run('reference-postgres',['-d',...pgArgs,'--network',f.network,'--mount',`type=volume,src=${data.Name},dst=/var/lib/postgresql`,'--entrypoint','postgres',postgres,'-D','/var/lib/postgresql/18/docker']);
    let pgReady=false;
    for(let i=0;i<120;i++){try{docker(['exec',`${f.id}-reference-postgres`,'pg_isready','-h','127.0.0.1','-U','postgres','-d','cosmoformer_package'],{stdio:'pipe'});pgReady=true;break;}catch{await delay(250);}}
    assert.equal(pgReady,true,'hardened PostgreSQL reference did not restart');
    const schema=docker(['exec',`${f.id}-reference-postgres`,'psql','-U','postgres','-d','cosmoformer_package','-At','-c','SELECT version FROM cosmoformer.schema_version']);
    check('PostgreSQL reference restarts prepared persistent data as UID 70 with a read-only root and no host port',()=>{assert.equal(schema,String(schemaVersion));assert.equal(docker(['exec',`${f.id}-reference-postgres`,'id','-u']),'70');const info=JSON.parse(docker(['inspect',`${f.id}-reference-postgres`]))[0];assert.equal(info.HostConfig.ReadonlyRootfs,true);assert.deepEqual(info.HostConfig.CapDrop,['ALL']);assert.deepEqual(info.HostConfig.PortBindings??{},{});assert.ok(Object.values(info.NetworkSettings.Ports??{}).every(value=>value===null));});
    check('PostgreSQL reference fails on unprepared data without initializing credentials',()=>assert.throws(()=>f.run('unprepared-postgres',[...pgArgs,'--network=none','--entrypoint','postgres',postgres,'-D','/tmp/unprepared-cluster'])));
    const report={verified_at:new Date().toISOString(),checks:results,images:f.images};
    writeFileSync(path.join(root,'.build/package/verification.json'),JSON.stringify(report,null,2)+'\n');
    console.log(`${results.length} packaged checks passed.`);
} catch(error) {
    for(const suffix of ['api','proxy'])try{console.error(docker(['logs',`${f.id}-${suffix}`]));}catch{}
    throw error;
} finally {await f.stop();}
