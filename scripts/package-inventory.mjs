import {readFileSync} from 'node:fs';

// Shared receipt validation for comparison and every runtime/browser consumer.
export const packageTargets = ['platform', 'proxy', 'browser-fixture', 'worker-opentofu', 'worker-terraform'];
export const digest = /^sha256:[a-f0-9]{64}$/;
export const sha256 = /^[a-f0-9]{64}$/;
export function assertInventory(receipt, targets = packageTargets) {
    if (!receipt || !sha256.test(receipt.source_sha256 ?? '') || !/^[a-f0-9]{40}$/.test(receipt.revision ?? '') ||
        typeof receipt.dirty !== 'boolean' || !Number.isSafeInteger(receipt.source_date_epoch) || receipt.source_date_epoch < 0) {
        throw Error('Package dependency failure: invalid source/revision/dirty-state/epoch receipt');
    }
    for (const target of targets) {
        const image = receipt.images?.[target];
        if (!packageTargets.includes(target) || !image || !digest.test(image.image_id ?? '') || image.architecture !== 'amd64' ||
            !sha256.test(image.archive_sha256 ?? '') || !digest.test(image.build_metadata?.['containerimage.digest'] ?? '') ||
            !digest.test(image.build_metadata?.['containerimage.config.digest'] ?? '') ||
            ![image.build_metadata?.['containerimage.digest'], image.build_metadata?.['containerimage.config.digest']].includes(image.image_id) ||
            !Array.isArray(image.checksums) || image.checksums.length === 0 ||
            !image.checksums.every(line => typeof line === 'string' && /^[a-f0-9]{64}  .+$/.test(line))) {
            throw Error(`Package dependency failure: missing or malformed package target: ${target}`);
        }
    }
}
export function verifyInventory(receipt, targets, inspect) {
    assertInventory(receipt, targets);
    for (const target of targets) {
        const expected = receipt.images[target];
        let actual;
        try {actual=inspect(expected.image_id);}
        catch {throw Error(`Package dependency failure: image unavailable: ${target}`);}
        const labels = actual?.Config?.Labels;
        if (actual?.Id !== expected.image_id || actual.Architecture !== 'amd64' ||
            labels?.['io.cosmoformer.source.sha256'] !== receipt.source_sha256 ||
            labels?.['org.opencontainers.image.revision'] !== receipt.revision ||
            labels?.['io.cosmoformer.source.dirty'] !== String(receipt.dirty)) {
            throw Error(`Package dependency failure: image/source identity mismatch: ${target}`);
        }
    }
}

export function readInventory(file) {
    try {return JSON.parse(readFileSync(file,'utf8'));}
    catch {throw Error('Package dependency failure: image inventory is missing or malformed');}
}
