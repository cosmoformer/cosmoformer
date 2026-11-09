import { reactRouter } from '@react-router/dev/vite';
import { defineConfig } from 'vite';
import { existsSync, readFileSync, statSync } from 'node:fs';
import path from 'node:path';
const dir = path.resolve(import.meta.dirname, '../.build/dev');
export default defineConfig(({ command, isPreview }) => {
    // React Router's build-time preview uses HTTP; only the browser fixture opts into HTTPS preview.
    const https = command === 'serve' && (!isPreview || process.env.COSMOFORMER_PREVIEW_HTTPS === '1');
    if (https && !existsSync(path.join(dir, 'cert.pem')))
        throw new Error('Run make dev-setup (or node scripts/dev.mjs cert for tests) before starting the HTTPS frontend.');
    if (https && !isPreview) {
        const info = statSync(dir);
        if (info.uid !== process.getuid!() || (info.mode & 0o077)) throw new Error('The local API directory must belong to this user with mode 0700. Run make dev-setup.');
    }
    const api = process.env.COSMOFORMER_DEV_API ?? { protocol: 'http:', host: 'localhost', port: 80, socketPath: path.join(dir, 'api.sock') };
    return {
        plugins: [reactRouter()],
        server: {
            host: '127.0.0.1',
            port: Number(process.env.COSMOFORMER_DEV_PORT ?? 5173),
            strictPort: true,
            https: https ? {
                cert: readFileSync(path.join(dir, 'cert.pem')),
                key: readFileSync(path.join(dir, 'key.pem')),
            } : undefined,
            proxy: {
                '/api': {
                    target: api,
                    bypass(request) {
                        // Set trusted metadata before creating the outgoing request:
                        // the proxyReq event is skipped for Expect: 100-continue.
                        // The API accepts it only over its owner-only Unix socket.
                        request.headers['x-cosmoformer-client-ip'] = request.socket.remoteAddress ?? '';
                    },
                },
                '/health': { target: api },
            },
        },
    };
});
