import { test, expect } from '@playwright/test';
import { readFileSync } from 'node:fs';

test('administrators inspect retention and explicitly run a bounded empty cleanup', async ({ page }) => {
    const fixture = JSON.parse(readFileSync(new URL('../../.build/testdb/browser.json', import.meta.url), 'utf8'));
    await page.goto('/login');
    await page.getByLabel('Username', { exact: true }).fill(fixture.username);
    await page.getByLabel('Password', { exact: true }).fill(fixture.password);
    await page.getByRole('button', { name: 'Sign in', exact: true }).click();
    await page.getByRole('button', { name: 'Retention & cleanup', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Artifact retention', exact: true })).toBeVisible();
    await expect(page.getByText('30 days', { exact: true })).toBeVisible();
    await expect(page.getByText('Disabled', { exact: true })).toBeVisible();
    await page.getByLabel('Outcome', { exact: true }).selectOption('eligible');
    await expect(page.getByText('No artifacts match this preview.', { exact: true })).toBeVisible();
    await page.getByLabel('Batch size', { exact: true }).fill('1');
    const accepted = page.waitForResponse(r => r.url().endsWith('/api/v1/admin/cleanup') && r.request().method() === 'POST');
    await page.getByRole('button', { name: 'Run bounded cleanup', exact: true }).click();
    const response = await accepted;
    expect(response.status()).toBe(200);
    expect(await response.json()).toEqual({ $schema: expect.stringMatching(/\/CleanupAcceptedBody\.json$/), claim_ids: [] });
    await expect(page.getByRole('status').filter({ hasText: '0 cleanup claims selected' })).toBeVisible();
    await expect(page.getByText('Disabled', { exact: true })).toBeVisible();
});
