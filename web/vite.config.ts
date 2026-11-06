import { reactRouter } from '@react-router/dev/vite';
import { defineConfig } from 'vite';
import { existsSync, readFileSync } from 'node:fs';
import path from 'node:path';
const dir = path.resolve(import.meta.dirname, '../.build/dev');
export default defineConfig(({ command, isPreview }) => {
    // React Router's build-time preview uses HTTP; only the browser fixture opts into HTTPS preview.
    const https = command === 'serve' && (!isPreview || process.env.COSMOFORMER_PREVIEW_HTTPS === '1');
    if (https && !existsSync(path.join(dir, 'cert.pem')))
        throw new Error('Run make dev-setup (or node scripts/dev.mjs cert for tests) before starting the HTTPS frontend.');
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
                '/api': { target: process.env.COSMOFORMER_DEV_API ?? 'http://127.0.0.1:18080' },
                '/health': { target: process.env.COSMOFORMER_DEV_API ?? 'http://127.0.0.1:18080' },
            },
        },
    };
});
