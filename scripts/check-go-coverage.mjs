import { readFileSync } from 'node:fs';
import { pathToFileURL } from 'node:url';
import { mergeProfiles } from './merge-go-coverage.mjs';

export function checkGoCoverage(profile) {
    const lines = mergeProfiles([profile]).trim().split('\n').slice(1);
    let covered = 0n;
    let total = 0n;
    for (const line of lines) {
        const [, statements, count] = / (\d+) (\d+)$/.exec(line);
        total += BigInt(statements);
        if (BigInt(count) > 0n) covered += BigInt(statements);
    }
    if (!total) throw new Error('No executable Go statements');
    // Compare exact counts: a rounded display must never let exactly 80% pass.
    return { covered, total, passes: covered * 100n > total * 80n };
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
    try {
        if (process.argv.length !== 3) throw new Error('Usage: check-go-coverage profile');
        const result = checkGoCoverage(readFileSync(process.argv[2], 'utf8'));
        const percent = Number(result.covered * 10000n / result.total) / 100;
        console.log(`Go statement coverage: ${percent.toFixed(2)}% (${result.covered}/${result.total}); required >80%.`);
        if (!result.passes) process.exitCode = 1;
    } catch (error) {
        console.error(error.message);
        process.exitCode = 1;
    }
}
