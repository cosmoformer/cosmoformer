import {test, expect} from '@playwright/test';
import {monitorCSP} from './fixtures/test';

// A deliberate synthetic violation checks the detector independently of the
// application's policy. The real packaged journeys must still have none.
test('CSP monitor records blocked scripts across secondary-context navigation', async ({browser}) => {
    const context = await browser.newContext({ignoreHTTPSErrors: true});
    const violations: string[] = [];
    try {
        await monitorCSP(context, violations);
        await context.route('https://localhost:5174/__csp_probe', route => route.fulfill({
            headers: {'Content-Type': 'text/html', 'Content-Security-Policy': "script-src 'none'"},
            body: '<!doctype html><script>window.__blockedCanary = true</script>',
        }));
        const page = await context.newPage();
        await page.goto('https://localhost:5174/__csp_probe');
        await expect.poll(() => violations.length).toBeGreaterThan(0);
        const beforeReload = violations.length;
        await page.reload();
        await expect.poll(() => violations.length).toBeGreaterThan(beforeReload);
        expect(violations.every(value => value.startsWith('script-src'))).toBe(true);
        expect(await page.evaluate(() => '__blockedCanary' in window)).toBe(false);
    } finally { await context.close(); }
});
