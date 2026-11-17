// Build-time CSP for the exact React Router SPA, never a runtime nonce or rewrite.
import {createHash} from 'node:crypto';
import {readFileSync, writeFileSync, readdirSync, mkdirSync, copyFileSync, existsSync} from 'node:fs';
import path from 'node:path';
const root = path.resolve(import.meta.dirname, '../..'), web = path.join(root, 'web');
const html = readFileSync(path.join(web, 'build/client/index.html'), 'utf8');
const hashes = [...html.matchAll(/<script\b([^>]*)>([\s\S]*?)<\/script>/g)].map(([, attrs, body]) => {
    if (/\bsrc\s*=/.test(attrs)) throw Error('Review newly emitted external script tags before packaging');
    return `'sha256-${createHash('sha256').update(body).digest('base64')}'`;
});
if (!hashes.length || /<(?:script|link)[^>]*(?:src|href)=["'](?:https?:)?\/\//i.test(html)) throw Error('Unexpected SPA bootstrap');
// Astryx and React use runtime style attributes for component state/layout. Only
// style attributes permit inline CSS; style elements remain same-origin only.
const policy = `default-src 'none'; script-src 'self' ${[...new Set(hashes)].join(' ')}; script-src-attr 'none'; style-src 'self'; style-src-attr 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self'; base-uri 'none'; object-src 'none'; frame-ancestors 'none'; form-action 'self'`;
writeFileSync(path.join(web, 'build/csp.conf'), `add_header Content-Security-Policy "${policy}" always;\nadd_header X-Content-Type-Options nosniff always;\nadd_header Referrer-Policy no-referrer always;\n`);
const licenses = path.join(web, 'build/licenses');
mkdirSync(licenses, {recursive: true});
const lock = JSON.parse(readFileSync(path.join(web, 'package-lock.json'), 'utf8'));
const manifest = {};
for (const [name, item] of Object.entries(lock.packages)) {
    if (!name) continue;
    manifest[name] = {version: item.version, integrity: item.integrity, license: item.license};
    const dir = path.join(web, name), dest = path.join(licenses, name.replaceAll('/', '_'));
    if (!existsSync(dir) && item.optional) continue;
    for (const file of readdirSync(dir).filter(file => /^(licen[sc]e|notice|copying|copyright)/i.test(file))) {
        mkdirSync(dest, {recursive: true});
        // Only regular upstream license files are shipped.
        try { copyFileSync(path.join(dir, file), path.join(dest, file)); } catch (e) { if (e.code !== 'EISDIR') throw e; }
    }
}
writeFileSync(path.join(licenses, 'packages.json'), JSON.stringify(manifest, null, 2));

const checksums=[];
function sum(relative) {
    const file=path.join(web,'build/client',relative);
    for(const entry of readdirSync(file,{withFileTypes:true}).sort((a,b)=>a.name<b.name?-1:a.name>b.name?1:0)) {
        const child=relative ? `${relative}/${entry.name}` : entry.name;
        if(entry.isDirectory())sum(child);
        else {
            const bytes=readFileSync(path.join(file,entry.name));
            if(child.endsWith('.css') && /(?:@import|url\()\s*(?:url\()?['"]?(?:https?:)?\/\//i.test(bytes.toString()))throw Error('External stylesheet resource in production build');
            checksums.push(`${createHash('sha256').update(bytes).digest('hex')}  ${child}`);
        }
    }
}
sum('');
writeFileSync(path.join(web,'build/SHA256SUMS'),checksums.join('\n')+'\n');
