import assert from 'node:assert/strict';
import {execFileSync} from 'node:child_process';
import {mkdirSync, writeFileSync, rmSync} from 'node:fs';
import {randomUUID} from 'node:crypto';
import path from 'node:path';
import {buildPackages} from './package-build.mjs';
import {assertInventory, verifyInventory, readInventory, digest, sha256} from './package-inventory.mjs';
const root=path.resolve(import.meta.dirname,'..'), dir=path.join(root,'.build/package');
const docker=args=>execFileSync('docker',args,{cwd:root,encoding:'utf8',stdio:'pipe'}).trim();
const sameHash = (a, b, pattern = digest) => typeof a === 'string' && pattern.test(a) && a === b;
export function compareRebuilds(first, second, selected = ['platform', 'proxy']) {
    const targets = {};
    let valid = true;
    try { assertInventory(first, []); assertInventory(second, []); } catch { valid = false; }
    for (const target of selected) {
        const a = first?.images?.[target] ?? {}, b = second?.images?.[target] ?? {};
        let inventoryValid = true;
        try { assertInventory(first,[target]); assertInventory(second,[target]); } catch { inventoryValid = false; }
        targets[target] = {first: a, second: b, inventory_valid: inventoryValid,
            image_equal: sameHash(a.image_id, b.image_id),
            bytes_equal: Array.isArray(a.checksums) && a.checksums.length > 0 && a.checksums.every(value => typeof value === 'string' && /^[a-f0-9]{64}  .+$/u.test(value)) && JSON.stringify(a.checksums) === JSON.stringify(b.checksums),
            archive_equal: sameHash(a.archive_sha256, b.archive_sha256, sha256),
            manifest_equal: sameHash(a.build_metadata?.['containerimage.digest'], b.build_metadata?.['containerimage.digest']),
            config_equal: sameHash(a.build_metadata?.['containerimage.config.digest'], b.build_metadata?.['containerimage.config.digest']),
        };
    }
    return {cache_disabled: true, source_sha256: first?.source_sha256,
        source_equal: sameHash(first?.source_sha256, second?.source_sha256, sha256),
        source_identity_equal: valid && ['revision','dirty','source_date_epoch'].every(key => first[key] === second[key]), targets};
}
export function assertRebuildsEqual(result) {
    assert.equal(result?.source_equal, true, 'no-cache source snapshot changed');
    assert.equal(result.source_identity_equal, true, 'no-cache source identity or inventory invalid');
    assert.ok(result.targets && ['platform','proxy'].every(target => Object.hasOwn(result.targets,target)), 'missing comparison targets');
    for (const [target, value] of Object.entries(result.targets)) {
        for (const [field, label] of [['bytes_equal', 'artifact bytes'], ['image_equal', 'image identity'], ['archive_equal', 'archive bytes'], ['manifest_equal', 'manifest identity'], ['config_equal', 'config identity'], ['inventory_valid', 'receipt inventory']]) {
            assert.equal(value?.[field], true, `${target} ${label} differ or are missing`);
        }
    }
}
export async function verifyReproducibility() {
    rmSync(path.join(dir,'reproducibility.json'),{force:true});
    const first=readInventory(path.join(dir,'images.json'));
    const selected=Object.keys(first.images ?? {});
    assertInventory(first, ['platform','proxy',...selected]);
    const inspect=id=>JSON.parse(docker(['image','inspect',id]))[0];
    verifyInventory(first,selected,inspect);
    const suffix=randomUUID(), evidence=path.join(dir,`repro-${suffix}`), tags={};
    mkdirSync(evidence,{recursive:true});
    const put=(name,value)=>writeFileSync(path.join(evidence,name),JSON.stringify(value,null,2)+'\n');
    put('first.json',first);
    const metadata={};
    const resources=()=>put('resources.json',tags);
    let result, failure;
    const old=process.env.COSMOFORMER_PACKAGE_NO_CACHE;
    try {
        for(const target of selected) {
            const tag=`cosmoformer-repro-${target}:${suffix}`;tags[target]=tag;resources();
            metadata[target]=inspect(first.images[target].image_id);
            docker(['tag',first.images[target].image_id,tag]);
        }
        process.env.COSMOFORMER_PACKAGE_NO_CACHE='1';
        // Never replace the active receipt, source snapshot, or first archives.
        const second=await buildPackages(selected,{outputDir:evidence});
        put('second.json',second);
        result=compareRebuilds(first,second,selected);
        for(const target of selected) {
            const after=inspect(second.images[target].image_id);
            const tag=`cosmoformer-rebuilt-${target}:${suffix}`;tags[`second-${target}`]=tag;resources();
            docker(['tag',second.images[target].image_id,tag]);
            Object.assign(result.targets[target],{created:[metadata[target].Created,after.Created],layers:[metadata[target].RootFS.Layers,after.RootFS.Layers]});
        }
        assertRebuildsEqual(result);
    } catch(error) {failure=error;}
    finally {
        if(old===undefined)delete process.env.COSMOFORMER_PACKAGE_NO_CACHE;else process.env.COSMOFORMER_PACKAGE_NO_CACHE=old;
        // Consumers resolve immutable IDs from the original complete inventory.
        // Restore only tags owned by this package invocation, never prune images.
        const cleanupErrors=[];
        for(const target of selected) {
            try {docker(['tag',first.images[target].image_id,`cosmoformer-${target}:local`]);}
            catch(error) {cleanupErrors.push(error.message);}
        }
        if(!failure && cleanupErrors.length===0) {
            for(const [key,tag] of Object.entries(tags)) {
                try {docker(['image','rm',tag]);delete tags[key];resources();}
                catch(error) {cleanupErrors.push(error.message);}
            }
        }
        if(cleanupErrors.length && !failure)failure=Error('Reproduction cleanup incomplete; see retained resource receipt');
        const report={...result,evidence:path.relative(root,evidence),...(failure?{error:failure.message}:{}),...(cleanupErrors.length?{cleanup_errors:cleanupErrors}:{})};
        put('comparison.json',report);
        writeFileSync(path.join(dir,'reproducibility.json'),JSON.stringify(report,null,2)+'\n');
        if(!failure)rmSync(path.join(evidence,'resources.json'),{force:true});
        // Failure preserves both receipts/archives and named image references.
    }
    if(failure)throw failure;
    console.log('Independent no-cache rebuild matched source, artifact/archive bytes and image/manifest/config identities: '+selected.join(', '));
}
