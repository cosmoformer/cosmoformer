import {test} from 'node:test';
import assert from 'node:assert/strict';
import {compareRebuilds, assertRebuildsEqual} from './package-reproducibility.mjs';

function receipt() {
    const image = {image_id: 'sha256:' + 'a'.repeat(64), architecture: 'amd64', archive_sha256: 'b'.repeat(64), checksums: ['c'.repeat(64) + '  artifact'], build_metadata: {'containerimage.digest': 'sha256:' + 'd'.repeat(64), 'containerimage.config.digest': 'sha256:' + 'a'.repeat(64)}};
    return {revision:'1'.repeat(40), dirty:false, source_date_epoch:1700000000, source_sha256: 'f'.repeat(64), images: {platform: structuredClone(image), proxy: structuredClone(image)}};
}
test('rebuild gate requires source, artifacts, archives and all image identities', () => {
    const first = receipt();
    assert.doesNotThrow(() => assertRebuildsEqual(compareRebuilds(first, structuredClone(first))));
    const changedSource = receipt(); changedSource.source_sha256 = '0'.repeat(64);
    assert.throws(() => assertRebuildsEqual(compareRebuilds(first, changedSource)), /source snapshot/);
    for (const target of ['platform', 'proxy']) {
        for (const [field, pattern] of [['archive_sha256', /archive bytes/], ['image_id', /image identity/], ['checksums', /artifact bytes/]]) {
            const next = receipt(); next.images[target][field] = field === 'checksums' ? ['0'.repeat(64) + '  artifact'] : field === 'image_id' ? 'sha256:' + '0'.repeat(64) : '0'.repeat(64);
            assert.throws(() => assertRebuildsEqual(compareRebuilds(first, next)), pattern);
        }
        for (const [field, pattern] of [['containerimage.digest', /manifest identity/], ['containerimage.config.digest', /config identity/]]) {
            const next = receipt(); next.images[target].build_metadata[field] = 'sha256:' + '0'.repeat(64);
            assert.throws(() => assertRebuildsEqual(compareRebuilds(first, next)), pattern);
        }
    }
});
test('two equally incomplete or malformed receipts cannot count as reproducible', () => {
    for (const change of [r => { delete r.images.proxy; }, r => { r.images.platform.checksums = []; }, r => { delete r.images.platform.build_metadata; }, r => { r.images.platform.archive_sha256 = ''; }, r => { r.source_sha256 = ''; }]) {
        const first = receipt(); change(first);
        assert.throws(() => assertRebuildsEqual(compareRebuilds(first, structuredClone(first))));
    }
});

test('comparison rejects missing targets, invalid metadata and source identity drift', () => {
    for (const value of [null, {}, {source_equal:true,source_identity_equal:true,targets:{}}, {source_equal:true,source_identity_equal:true,targets:{platform:{},proxy:{}}}]) {
        assert.throws(()=>assertRebuildsEqual(value));
    }
    for (const change of [r=>r.revision='bad',r=>r.revision='2'.repeat(40),r=>r.dirty=true,r=>r.dirty='false',r=>r.source_date_epoch++,r=>r.source_date_epoch='1700000000',r=>r.images.platform.architecture='arm64',r=>r.images.platform.build_metadata['containerimage.config.digest']='sha256:'+'0'.repeat(64)]) {
        const first=receipt(),second=receipt();change(second);
        assert.throws(()=>assertRebuildsEqual(compareRebuilds(first,second)));
    }
});
