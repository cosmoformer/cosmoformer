// Real package regression, run by make test-package before runtime/browser checks.
// A disposable Git history changes epochs without changing package inputs.
import assert from 'node:assert/strict';
import {execFileSync} from 'node:child_process';
import {randomUUID} from 'node:crypto';
import {copyFileSync, mkdirSync, readFileSync, writeFileSync, rmSync} from 'node:fs';
import path from 'node:path';
import {pathToFileURL} from 'node:url';
import {compareRebuilds, assertRebuildsEqual} from './package-reproducibility.mjs';
import {fixtureGit} from './package-git.mjs';
import {packageTargets} from './package-inventory.mjs';

export async function verifyCrossEpochCache() {
    const root=path.resolve(import.meta.dirname,'..');
    const dir=path.join(root,'.build',`package-cache-${randomUUID()}`);
    mkdirSync(path.join(dir,'repo'),{recursive:true});
    const tagPrefix=`cosmoformer-cache-${randomUUID()}`;
    const repo=path.join(dir,'repo');
    const cleanupErrors=[];
    writeFileSync(path.join(dir,'resources.json'),JSON.stringify({tags:packageTargets.map(target=>`${tagPrefix}-${target}:local`)}));
    let failure;
    const run=(cmd,args,options={})=>execFileSync(cmd,args,{cwd:repo,encoding:'utf8',stdio:'pipe',maxBuffer:32*1024*1024,...options});
    // Read only the same admitted public source inputs as the main package build.
    const source=JSON.parse(readFileSync(path.join(root,'.build/package/source.json'),'utf8'));
    for(const file of Object.keys(source.files)) {
        mkdirSync(path.dirname(path.join(repo,file)),{recursive:true});
        copyFileSync(path.join(root,'.build/package/source',file),path.join(repo,file));
    }
    writeFileSync(path.join(repo,'.gitignore'),'.build/\n');
    const git=fixtureGit(repo);
    git(['init','--quiet']);
    const commit=(label,epoch)=>{git(['add','.']);git(['commit','--quiet','--allow-empty','-m',label],{GIT_AUTHOR_DATE:`@${epoch} +0000`,GIT_COMMITTER_DATE:`@${epoch} +0000`});};
    const {buildPackages}=await import(pathToFileURL(path.join(repo,'scripts/package-build.mjs')));
    const build=async name=>{
        try {await buildPackages(packageTargets,{tagPrefix,noCache:name.startsWith('cold'),git});}
        catch(error){writeFileSync(path.join(dir,`${name}.log`),String(error.stack??error)+String(error.stdout??'')+String(error.stderr??''));throw error;}
        const receipt=JSON.parse(readFileSync(path.join(repo,'.build/package/images.json'),'utf8'));
        writeFileSync(path.join(dir,`${name}.json`),JSON.stringify(receipt,null,2)+'\n');
        return receipt;
    };
    const compare=(name,a,b)=>{
        const result={...compareRebuilds(a,b,packageTargets),cache_disabled:name!=='warm-comparison'};
        writeFileSync(path.join(dir,`${name}.json`),JSON.stringify(result,null,2)+'\n');
        assertRebuildsEqual(result);
    };
    try {
    // Both epochs precede wall clock; raising a clamped cached mtime is the bug.
    const t1=source.source_date_epoch-7200,t2=source.source_date_epoch;
    commit('A',t1);const a=await build('a');
    commit('B',t2);const b=await build('b');
    assert.equal(a.source_sha256,b.source_sha256);
    assert.notEqual(a.revision,b.revision);
    assert.equal(a.dirty,false);assert.equal(b.dirty,false);
    for(const target of packageTargets)assert.notEqual(a.images[target].image_id,b.images[target].image_id);
    compare('warm-comparison',b,await build('warm'));
    const cold=await build('cold');compare('cross-epoch-comparison',b,cold);
    compare('independent-comparison',cold,await build('cold-again'));
    // Repeated dirty edits keep size, revision, epoch and dirty state fixed.
    // Normalizing context mtimes used to hide the second edit from local sync.
    const license=readFileSync(path.join(repo,'LICENSE'));
    const edit=byte=>{const bytes=Buffer.from(license);bytes[0]=byte;writeFileSync(path.join(repo,'LICENSE'),bytes);assert.equal(bytes.length,license.length);};
    edit(license[0]^1);
    const changed=await build('changed');
    assert.equal(changed.revision,b.revision);assert.equal(changed.dirty,true);
    assert.notEqual(changed.source_sha256,b.source_sha256);
    for(const target of packageTargets)assert.notEqual(changed.images[target].image_id,b.images[target].image_id);
    edit(license[0]^2);
    const changedAgain=await build('changed-again');
    assert.equal(changedAgain.revision,b.revision);assert.equal(changedAgain.dirty,true);
    assert.equal(changed.source_date_epoch,b.source_date_epoch);assert.equal(changedAgain.source_date_epoch,b.source_date_epoch);
    assert.notEqual(changedAgain.source_sha256,changed.source_sha256);
    for(const target of packageTargets)assert.notEqual(changedAgain.images[target].image_id,changed.images[target].image_id);
    // Keep all diagnostic receipts. Remove only this fixture's public source tree;
    // Docker cache and images belonging to other invocations are never pruned.
    rmSync(repo,{recursive:true,force:true});
    console.log('Real cross-epoch, warm and independent package comparisons passed: '+dir);
    return dir;
    } catch(error) {failure=error;throw error;} finally {
        for(const target of packageTargets) {
            try {run('docker',['image','rm',`${tagPrefix}-${target}:local`],{cwd:root});}
            catch(error) {cleanupErrors.push(error);}
        }
        if(cleanupErrors.length) {
            writeFileSync(path.join(dir,'cleanup-errors.json'),JSON.stringify(cleanupErrors.map(error=>error.message)));
            if(!failure)throw new AggregateError(cleanupErrors,'Cache fixture tag cleanup failed; evidence: '+dir);
        } else rmSync(path.join(dir,'resources.json'),{force:true});
    }
}
