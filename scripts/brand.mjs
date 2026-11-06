// Export the approved Spiral Ribbon artwork; no image service or font is needed.
import { readFileSync, writeFileSync, mkdirSync, statSync } from 'node:fs';
import { createRequire } from 'node:module';
import { execFileSync } from 'node:child_process';
import path from 'node:path';
const root = path.resolve(import.meta.dirname, '..');
const out = path.join(root, 'web/public/brand');
const review = path.join(root, '.build/brand');
const source = readFileSync(path.join(root, 'assets/brand/source.svg'), 'utf8');
const require = createRequire(path.join(root, 'web/package.json'));
const { chromium } = require('@playwright/test');
mkdirSync(out, { recursive: true });
mkdirSync(review, { recursive: true });

function group(id) {
    const match = source.match(new RegExp(`<g id="${id}" data-box="([\\d.]+) ([\\d.]+)"[^>]*>([\\s\\S]*?)</g>`));
    if (!match) throw new Error(`Missing master artwork group: ${id}`);
    return { width: Number(match[1]), height: Number(match[2]), paths: match[3] };
}
const mark = group('mark'), word = group('wordmark');
const palettes = {
    color: ['#167760', '#123c32'],
    reversed: ['#a1caba', '#ffffff'],
    'mono-dark': ['#123c32', '#123c32'],
    'mono-white': ['#ffffff', '#ffffff'],
};
function paint(art, colors) {
    return art.replaceAll('#167760', colors[0]).replaceAll('#123c32', colors[1]);
}
const position = (art, x, y, scale = 1) => `<g transform="translate(${x} ${y}) scale(${scale})">${art}</g>`;
function svg(width, height, content, label = 'Cosmoformer') {
    return `<svg xmlns="http://www.w3.org/2000/svg" width="${width}" height="${height}" viewBox="0 0 ${width} ${height}" role="img" aria-label="${label}"><title>${label}</title>${content}</svg>\n`;
}
const exports = [];
function save(name, artwork, width, height, pngWidth) {
    writeFileSync(path.join(out, name + '.svg'), artwork);
    exports.push({ name, width, height, ...(pngWidth ? { pngWidth } : {}) });
}
for (const [variant, colors] of Object.entries(palettes)) {
    const m = paint(mark.paths, colors), w = paint(word.paths, colors);
    const raster = variant === 'color' || variant === 'reversed';
    save(`logo-horizontal-${variant}`, svg(1954, 427, position(m, 32, 32) + position(w, 552, 103)), 1954, 427, raster ? 1600 : undefined);
    save(`logo-stacked-${variant}`, svg(1000, 650,
        position(m, 500 - mark.width * 0.9 / 2, 54, 0.9) + position(w, 80, 444, 840 / word.width)), 1000, 650, raster ? 1000 : undefined);
    save(`mark-${variant}`, svg(512, 512,
        position(m, 51.2, (512 - mark.height * 409.6 / mark.width) / 2, 409.6 / mark.width), 'Cosmoformer galaxy mark'), 512, 512, raster ? 512 : undefined);
    save(`wordmark-${variant}`, svg(1434, 227, position(w, 32, 32)), 1434, 227, raster ? 1434 : undefined);
}
const reversed = paint(mark.paths, palettes.reversed);
const favicon = svg(512, 512, '<rect width="512" height="512" rx="112" fill="#123c32"/>' +
    position(reversed, 51.2, (512 - mark.height * 409.6 / mark.width) / 2, 409.6 / mark.width), 'Cosmoformer favicon');
writeFileSync(path.join(out, 'favicon.svg'), favicon);

const browser = await chromium.launch();
try {
    const page = await browser.newPage({ deviceScaleFactor: 1 });
    async function render(artwork, width, height, destination) {
        await page.setViewportSize({ width, height });
        await page.setContent(`<html><body style="margin:0;background:transparent"><img width="${width}" height="${height}" src="data:image/svg+xml;base64,${Buffer.from(artwork).toString('base64')}"></body></html>`);
        await page.locator('img').evaluate(image => image.decode());
        await page.screenshot({ path: destination, omitBackground: true });
    }
    for (const entry of exports.filter(e => e.pngWidth)) {
        const height = Math.round(entry.pngWidth * entry.height / entry.width);
        await render(readFileSync(path.join(out, entry.name + '.svg'), 'utf8'), entry.pngWidth, height, path.join(out, entry.name + '.png'));
    }
    const icons = [];
    for (const size of [16, 32, 48]) {
        const file = path.join(out, `favicon-${size}.png`);
        await render(favicon, size, size, file);
        icons.push({ size, bytes: readFileSync(file) });
    }
    // ICO directory entries point to lossless PNG payloads.
    const header = Buffer.alloc(6 + icons.length * 16);
    header.writeUInt16LE(1, 2); header.writeUInt16LE(icons.length, 4);
    let offset = header.length;
    icons.forEach(({ size, bytes }, i) => {
        const at = 6 + i * 16;
        header[at] = size; header[at + 1] = size;
        header.writeUInt16LE(1, at + 4); header.writeUInt16LE(32, at + 6);
        header.writeUInt32LE(bytes.length, at + 8); header.writeUInt32LE(offset, at + 12);
        offset += bytes.length;
    });
    writeFileSync(path.join(out, 'favicon.ico'), Buffer.concat([header, ...icons.map(i => i.bytes)]));
    const appIcon = svg(512, 512, '<rect width="512" height="512" fill="#123c32"/>' +
        position(reversed, 51.2, (512 - mark.height * 409.6 / mark.width) / 2, 409.6 / mark.width));
    for (const [name, size] of [['apple-touch-icon', 180], ['icon-192', 192], ['icon-512', 512]]) {
        await render(appIcon, size, size, path.join(out, name + '.png'));
    }
    const data = name => 'data:image/svg+xml;base64,' + readFileSync(path.join(out, name + '.svg')).toString('base64');
    await page.setViewportSize({ width: 1400, height: 1100 });
    await page.setContent(`<html><body style="margin:0;padding:48px;background:#edf1ef;color:#123c32;font:16px system-ui;box-sizing:border-box"><h1 style="margin:0 0 8px;font-size:30px">Cosmoformer · Spiral Ribbon</h1><p style="margin:0 0 30px;color:#63776f">Primary artwork · transparent SVG and PNG exports · outlined lettering</p><div style="display:grid;grid-template-columns:1fr 1fr;gap:20px">${[
        ['Horizontal · light', 'logo-horizontal-color', false, 120], ['Horizontal · dark', 'logo-horizontal-reversed', true, 120],
        ['Stacked · light', 'logo-stacked-color', false, 230], ['Stacked · dark', 'logo-stacked-reversed', true, 230],
        ['Icon · light', 'mark-color', false, 170], ['Icon · dark', 'mark-reversed', true, 170],
        ['Wordmark · light', 'wordmark-color', false, 90], ['Wordmark · dark', 'wordmark-reversed', true, 90],
    ].map(([label, name, dark, height]) => `<section style="border-radius:14px;padding:22px 28px;background:${dark ? '#123c32' : '#fff'};color:${dark ? '#d3e6dc' : '#63776f'}"><div style="font-size:12px;margin-bottom:14px">${label}</div><div style="height:${height}px;display:grid;place-items:center"><img style="display:block;min-width:0;min-height:0;object-fit:contain;width:${String(name).startsWith('mark-') ? '170px' : '100%'};height:${height}px" src="${data(name)}"></div></section>`).join('')}</div></body></html>`);
    await page.locator('img').evaluateAll(images => Promise.all(images.map(image => image.decode())));
    await page.screenshot({ path: path.join(review, 'preview.png'), fullPage: true });
} finally { await browser.close(); }
const files = [...exports.map(e => e.name + '.svg'), ...exports.filter(e => e.pngWidth).map(e => e.name + '.png'),
    'favicon.svg', 'favicon.ico', 'favicon-16.png', 'favicon-32.png', 'favicon-48.png', 'apple-touch-icon.png', 'icon-192.png', 'icon-512.png'];
writeFileSync(path.join(out, 'manifest.json'), JSON.stringify({ name: 'Cosmoformer Spiral Ribbon', colors: { forest: '#123c32', emerald: '#167760', mint: '#a1caba', white: '#ffffff' },
    files: files.map(file => ({ file, bytes: statSync(path.join(out, file)).size })) }, null, 2) + '\n');
execFileSync('/usr/bin/zip', ['-qr', path.join(review, 'cosmoformer-brand.zip'), 'assets/brand', 'web/public/brand'], { cwd: root });
console.log(`Exported ${files.length} assets (${files.reduce((sum, name) => sum + statSync(path.join(out, name)).size, 0)} bytes).`);
console.log('Preview and download archive: .build/brand/');
