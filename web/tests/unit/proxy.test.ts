// @vitest-environment node
import http from 'node:http';
import type { AddressInfo } from 'node:net';
import { createServer, type UserConfigFn } from 'vite';
import { expect, test, vi } from 'vitest';

vi.mock('@react-router/dev/vite', () => ({ reactRouter: () => ({ name: 'fixture-router' }) }));
import viteConfig from '../../vite.config';

test('API proxy overwrites caller client-address claims with its actual socket peer', async () => {
    const backend = http.createServer((request, response) => {
        response.setHeader('Content-Type', 'application/json');
        response.end(JSON.stringify({
            address: request.headers['x-cosmoformer-client-ip'],
            copies: request.rawHeaders.filter((value, index) => index % 2 === 0 && value.toLowerCase() === 'x-cosmoformer-client-ip').length,
        }));
    });
    await new Promise<void>((resolve, reject) => {
        backend.once('error', reject);
        backend.listen(0, '127.0.0.1', resolve);
    });
    const backendPort = (backend.address() as AddressInfo).port;
    vi.stubEnv('COSMOFORMER_DEV_API', `http://127.0.0.1:${backendPort}`);
    let frontend: Awaited<ReturnType<typeof createServer>> | undefined;
    try {
        const config = await (viteConfig as UserConfigFn)({ command: 'build', mode: 'test', isPreview: false });
        frontend = await createServer({
            configFile: false,
            plugins: [],
            optimizeDeps: { noDiscovery: true, entries: [] },
            server: { ...config.server, port: 0, https: undefined, hmr: false, watch: null },
        });
        await frontend.listen();
        const port = (frontend.httpServer!.address() as AddressInfo).port;
        for (const [claimed, connection, expectContinue] of [[['203.0.113.1'], 'close', false], [['203.0.113.1', '198.51.100.1'], 'close', false], [[''], 'close', false], [['203.0.113.1'], 'X-Cosmoformer-Client-IP', false], [['203.0.113.1'], 'close', true]] as const) {
            const observed = await new Promise<{ address: string; copies: number }>((resolve, reject) => {
                const request = http.get({
                    host: '127.0.0.1', port, path: '/api/v1/session/login',
                    headers: { 'X-Cosmoformer-Client-IP': [...claimed], 'X-Forwarded-For': '203.0.113.2', Connection: connection, ...(expectContinue ? { Expect: '100-continue' } : {}) },
                    timeout: 2000,
                }, (response) => {
                    let body = '';
                    response.setEncoding('utf8').on('data', (part) => { body += part; });
                    response.on('end', () => {
                        try {
                            expect(response.statusCode).toBe(200);
                            resolve(JSON.parse(body));
                        } catch (error) { reject(error); }
                    });
                });
                request.on('error', reject);
                request.on('timeout', () => { request.destroy(new Error('fixture timeout')); });
            });
            expect(observed).toEqual({ address: '127.0.0.1', copies: 1 });
        }
    } finally {
        vi.unstubAllEnvs();
        await frontend?.close();
        backend.closeAllConnections();
        await new Promise<void>((resolve, reject) => backend.close((error) => { if (error) reject(error); else resolve(); }));
    }
}, 15000);
