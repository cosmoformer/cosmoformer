import path from 'node:path';
import { defineConfig } from 'vitest/config';

// Unit tests need neither the development HTTPS server nor the router build plugin.
export default defineConfig({
    test: {
        environment: 'jsdom',
        environmentOptions: { jsdom: { url: 'https://localhost:5173' } },
        include: ['tests/unit/**/*.test.{ts,tsx}'],
        setupFiles: ['tests/unit/setup.ts'],
        clearMocks: true,
        restoreMocks: true,
        coverage: {
            provider: 'v8',
            include: ['app/**/*.{ts,tsx}', '../scripts/dev.mjs', '../scripts/tools.mjs'],
            allowExternal: true,
            exclude: ['app/.generated/**'],
            reportsDirectory: '../.build/coverage/web',
            reporter: ['text', 'html', ['lcov', { projectRoot: path.resolve(import.meta.dirname, '..') }], 'json-summary'],
            thresholds: {
                statements: 90, branches: 85, functions: 90, lines: 90,
                'app/**/*.{ts,tsx}': { statements: 90, branches: 85, functions: 90, lines: 90 },
            },
        },
    },
});
