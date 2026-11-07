import { readFileSync, writeFileSync } from 'node:fs';
import { pathToFileURL } from 'node:url';

// Profiles describe the same checkout on different operating systems. Keep each
// source block once, with its largest observed count; coverage is a union, not
// an average of percentages or a sum of duplicate executable-line totals.
export function mergeProfiles(profiles) {
    const blocks = new Map();
    for (const profile of profiles) {
        const [mode, ...lines] = profile.trim().split('\n');
        if (mode !== 'mode: atomic') throw new Error('Expected atomic Go coverage');
        for (const line of lines) {
            const match = /^(\S+:\d+\.\d+,\d+\.\d+) (\d+) (\d+)$/.exec(line);
            if (!match) throw new Error('Invalid Go coverage block');
            const [, key, statements, count] = match;
            const previous = blocks.get(key);
            if (previous && previous.statements !== statements) throw new Error('Conflicting source versions');
            blocks.set(key, { statements, count: previous && BigInt(previous.count) > BigInt(count) ? previous.count : count });
        }
    }
    if (!blocks.size) throw new Error('No Go coverage blocks');
    return 'mode: atomic\n' + [...blocks].sort(([a], [b]) => a.localeCompare(b)).map(([key, value]) => `${key} ${value.statements} ${value.count}\n`).join('');
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
    const [output, ...inputs] = process.argv.slice(2);
    if (!output || inputs.length < 2) throw new Error('Usage: merge-go-coverage output input input...');
    writeFileSync(output, mergeProfiles(inputs.map(path => readFileSync(path, 'utf8'))));
}
