import assert from 'node:assert/strict';
import {execFileSync} from 'node:child_process';
import {readFileSync, writeFileSync, rmSync} from 'node:fs';
import {randomUUID} from 'node:crypto';
import path from 'node:path';
import {buildPackages} from './package-build.mjs';
const root=path.resolve(import.meta.dirname,'..'), dir=path.join(root,'.build/package');
const docker=args=>execFileSync('docker',args,{cwd:root,encoding:'utf8',stdio:'pipe'}).trim();
const digest = /^sha256:[a-f0-9]{64}$/;
const sameHash = (a, b, pattern = digest) => typeof a === 'string' && pattern.test(a) && a === b;
export function compareRebuilds(first, second) {
    const targets = {};
    for (const target of ['platform', 'proxy']) {
        const a = first.images?.[target] ?? {}, b = second.images?.[target] ?? {};
        targets[target] = {first: a, second: b,
            image_equal: sameHash(a.image_id, b.image_id),
            bytes_equal: Array.isArray(a.checksums) && a.checksums.length > 0 && a.checksums.every(value => typeof value === 'string' && /^[a-f0-9]{64}  .+$/u.test(value)) && JSON.stringify(a.checksums) === JSON.stringify(b.checksums),
            archive_equal: sameHash(a.archive_sha256, b.archive_sha256, /^[a-f0-9]{64}$/),
            manifest_equal: sameHash(a.build_metadata?.['containerimage.digest'], b.build_metadata?.['containerimage.digest']),
            config_equal: sameHash(a.build_metadata?.['containerimage.config.digest'], b.build_metadata?.['containerimage.config.digest']),
        };
    }
    return {cache_disabled: true, source_sha256: first.source_sha256, source_equal: sameHash(first.source_sha256, second.source_sha256, /^[a-f0-9]{64}$/), targets};
}
export function assertRebuildsEqual(result) {
    assert.equal(result.source_equal, true, 'no-cache source snapshot changed');
    for (const [target, value] of Object.entries(result.targets)) {
        for (const [field, label] of [['bytes_equal', 'artifact bytes'], ['image_equal', 'image identity'], ['archive_equal', 'archive bytes'], ['manifest_equal', 'manifest identity'], ['config_equal', 'config identity']]) {
            assert.equal(value[field], true, `${target} ${label} differ or are missing`);
        }
    }
}
export function verifyReproducibility() {
    rmSync(path.join(dir,'reproducibility.json'),{force:true});
    const first=JSON.parse(readFileSync(path.join(dir,'images.json'),'utf8'));
    const tags={}, metadata={};
    const suffix=randomUUID();
    // A named reference retains the first images while the build replaces its
    // ordinary local tags. The containerd store may discard untagged manifests.
    for(const target of ['platform','proxy']) {
        const tag=`cosmoformer-repro-${target}:${suffix}`;tags[target]=tag;
        const image=first.images[target].image_id;
        metadata[target]=JSON.parse(docker(['image','inspect',image]))[0];
        writeFileSync(path.join(dir,'repro-resources.json'),JSON.stringify(tags,null,2));
        docker(['tag',image,tag]);
    }
    const old=process.env.COSMOFORMER_PACKAGE_NO_CACHE;
    process.env.COSMOFORMER_PACKAGE_NO_CACHE='1';
    let success=false;
    try {
        buildPackages(['platform','proxy']);
        const second=JSON.parse(readFileSync(path.join(dir,'images.json'),'utf8'));
        const result=compareRebuilds(first,second);
        for(const target of ['platform','proxy']) {
            const b=second.images[target];
            const after=JSON.parse(docker(['image','inspect',b.image_id]))[0];
            Object.assign(result.targets[target],{created:[metadata[target].Created,after.Created],layers:[metadata[target].RootFS.Layers,after.RootFS.Layers]});
        }
        writeFileSync(path.join(dir,'reproducibility.json'),JSON.stringify(result,null,2)+'\n');
        assertRebuildsEqual(result);
        // Restore the complete set, including unchanged exact worker/test images.
        writeFileSync(path.join(dir,'images.json'),JSON.stringify({...first,images:{...first.images,...second.images}},null,2)+'\n');
        success=true;
        console.log('Independent no-cache rebuild matched source, artifact/archive bytes and image/manifest/config identities.');
    } finally {
        if(old===undefined)delete process.env.COSMOFORMER_PACKAGE_NO_CACHE;else process.env.COSMOFORMER_PACKAGE_NO_CACHE=old;
        if(success) {
            for(const tag of Object.values(tags))docker(['image','rm',tag]);
            rmSync(path.join(dir,'repro-resources.json'),{force:true});
        }
        // On failure preserve the named first images and receipt for diagnosis.
    }
}
