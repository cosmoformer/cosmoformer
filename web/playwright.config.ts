import { defineConfig, devices } from '@playwright/test';
import { mkdirSync } from 'node:fs';
import path from 'node:path';
// Playwright 1.64 Firefox shares app data with desktop Firefox on macOS 27.
// A private app-data root avoids touching the personal profile.
// https://github.com/microsoft/playwright/issues/42768
const firefoxHome = path.resolve(import.meta.dirname, '../.build/firefox-home');
if (process.platform === 'darwin')
    mkdirSync(firefoxHome, { recursive: true, mode: 0o700 });
export default defineConfig({ testDir: './tests', testMatch: '**/*.spec.ts', fullyParallel: false, workers: 1, retries: 0, timeout: 60000,
    use: { baseURL: 'https://localhost:5174', ignoreHTTPSErrors: true, trace: 'retain-on-failure' },
    projects: [{ name: 'chromium', use: { ...devices['Desktop Chrome'] } }, { name: 'firefox', use: { ...devices['Desktop Firefox'], launchOptions: process.platform === 'darwin' ? { env: { ...process.env, CFFIXED_USER_HOME: firefoxHome } } : {} } }],
    webServer: [{ command: 'make -C .. browser-server', url: 'http://127.0.0.1:18081/health/ready', reuseExistingServer: false, timeout: 90000, gracefulShutdown: { signal: 'SIGTERM', timeout: 10000 } }, { command: 'npm run dev', url: 'https://localhost:5174/login', ignoreHTTPSErrors: true, reuseExistingServer: false, timeout: 60000, env: { COSMOFORMER_DEV_API: 'http://127.0.0.1:18081', COSMOFORMER_DEV_PORT: '5174' } }] });
