#!/usr/bin/env node
// Synthetic local exercise. Each invocation owns a new marked test container;
// never selects developer/application databases or external artifact storage.
import {mkdirSync, readFileSync, rmSync} from 'node:fs';
import {spawnSync} from 'node:child_process';
import {randomUUID} from 'node:crypto';
import path from 'node:path';
const root=path.resolve(import.meta.dirname,'..');
const dir=path.join(root,'.build/test-runs',`operator-${randomUUID()}`);
mkdirSync(dir,{recursive:true,mode:0o700});
const env={...process.env,COSMOFORMER_TEST_DIR:dir,GOCACHE:path.join(root,'.build/go-cache'),GOPATH:path.join(root,'.build/gopath')};
// Inherited application settings never select the exercise's database.
for(const key of Object.keys(env))if(key.startsWith('COSMOFORMER_')&&key!=='COSMOFORMER_TEST_DIR')delete env[key];
function run(command,args){const r=spawnSync(command,args,{cwd:root,env,stdio:'inherit'});if(r.error||r.status!==0)throw Error(`Recovery exercise failed (${command}); inspect the retained test receipt at ${dir}`);}
let complete=false;
try{
 run(process.execPath,['scripts/dev.mjs','test-start']);
 env.COSMOFORMER_TEST_OWNER_DSN=readFileSync(path.join(dir,'owner.dsn'),'utf8').trim();
 env.COSMOFORMER_TEST_MARKER_FILE=path.join(dir,'marker');
 env.COSMOFORMER_TEST_PG_CONTAINER=readFileSync(path.join(dir,'container'),'utf8').trim();
 run(path.join(root,'.build/tools/go/bin/go'),['test','-race','-count=1','-v','-timeout=10m','-tags=integration','./internal/store','-run','^TestOperator']);
 complete=true;
}finally{
 run(process.execPath,['scripts/dev.mjs','test-stop']);
 if(complete)rmSync(dir,{recursive:true,force:true});
}
console.log('Synthetic rotation, backup protection, clock reset and actual PostgreSQL restore exercise passed.');
