// @vitest-environment node
import { beforeEach, it, expect, vi } from 'vitest';
import path from 'node:path';
const f = vi.hoisted(() => ({ files: new Map<string, Buffer>(), views: [] as any[], closed: 0, fail: false, source: '' }));
vi.mock('node:fs', async () => {
    const actual = await vi.importActual<typeof import('node:fs')>('node:fs');
    return {
        readFileSync: (p: string, encoding?: string) => { const b = f.files.get(p) ?? (p.endsWith('source.svg') ? Buffer.from(f.source || actual.readFileSync(p, 'utf8')) : undefined); if (!b)
            throw Error('Missing export ' + p); return encoding ? b.toString() : b; },
        writeFileSync: (p: string, value: any) => f.files.set(p, Buffer.from(value)), mkdirSync: vi.fn(), statSync: (p: string) => ({ size: f.files.get(p)!.length }),
    };
});
vi.mock('node:module', () => ({ createRequire: () => () => ({ chromium: { launch: async () => ({ newPage: async () => ({
                    setViewportSize: async (size: any) => { f.views.push(size); }, setContent: async (html: string) => { expect(html).toContain('<img'); },
                    locator: () => ({ evaluate: async () => { }, evaluateAll: async () => { } }),
                    screenshot: async ({ path: p }: any) => { if (f.fail)
                        throw Error('render failed'); f.files.set(p, Buffer.from('synthetic PNG payload')); },
                }), close: async () => { f.closed++; } }) } }) }));
vi.mock('node:child_process', () => ({ execFileSync: vi.fn() }));
beforeEach(() => { vi.resetModules(); f.files.clear(); f.views = []; f.closed = 0; f.fail = false; f.source = ''; vi.spyOn(console, 'log').mockImplementation(() => { }); });
const root = path.resolve(import.meta.dirname, '../../..');
async function render() {
    // @ts-expect-error Executable JavaScript helper.
    await import('../../../scripts/brand.mjs');
}
it('exports accessible palettes, manifest sizes and valid ICO offsets for the approved artwork', async () => {
    await render();
    const get = (name: string) => f.files.get(path.join(root, 'web/public/brand', name))!;
    const manifest = JSON.parse(get('manifest.json').toString());
    expect(manifest.files).toHaveLength(32);
    for (const item of manifest.files)
        expect(item.bytes).toBe(get(item.file).length);
    expect(get('mark-mono-white.svg').toString()).toContain('#ffffff');
    expect(get('mark-mono-white.svg').toString()).not.toContain('#167760');
    expect(get('mark-color.svg').toString()).toContain('aria-label="Cosmoformer galaxy mark"');
    const ico = get('favicon.ico');
    expect(ico.readUInt16LE(2)).toBe(1);
    expect(ico.readUInt16LE(4)).toBe(3);
    let offset = 54;
    for (let i = 0; i < 3; i++) {
        const entry = 6 + i * 16;
        expect(ico[entry]).toBe([16, 32, 48][i]);
        expect(ico.readUInt32LE(entry + 12)).toBe(offset);
        const size = ico.readUInt32LE(entry + 8);
        expect(ico.subarray(offset, offset + size).toString()).toBe('synthetic PNG payload');
        offset += size;
    }
    expect(offset).toBe(ico.length);
    expect(f.closed).toBe(1);
    expect(f.views).toContainEqual({ width: 192, height: 192 });
});
it('closes the browser when image generation fails and does not publish a partial manifest', async () => {
    f.fail = true;
    await expect(render()).rejects.toThrow('render failed');
    expect(f.closed).toBe(1);
    expect([...f.files.keys()].some(p => p.endsWith('manifest.json'))).toBe(false);
});
it('rejects missing master artwork before starting the browser', async () => { f.source = '<svg></svg>'; await expect(render()).rejects.toThrow('Missing master artwork group'); expect(f.views).toHaveLength(0); });
