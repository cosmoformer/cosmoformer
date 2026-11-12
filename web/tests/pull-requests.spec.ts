import { test, expect, type Page } from '@playwright/test';
import { reviewSession, projects, runFixture, runResponse, changesResponse, approvalSummary, org, project, env, run, reviewID } from './fixtures/review';
import type { Schema } from '../app/lib/api';
const path = `/o/${org}/p/${project}?env=${env}&view=pull-requests`;
const prs: Schema['PullRequestsOutputBody'] = { pull_requests: [{ id: reviewID(41), repository_id: reviewID(8), repository_uuid: '{synthetic-repository}', repository_url: runFixture.source_url, number: '42', commit: runFixture.commit, state: 'OPEN', current: true, feedback_status: 'failed', runs: [{ run: runFixture, needs_attention: false }, { run: { ...runFixture, id: reviewID(45), stack_name: 'Database', status: 'queued', queue_reason: 'NO_COMPATIBLE_WORKER', plan: undefined }, needs_attention: true }] }, { id: reviewID(42), repository_id: reviewID(8), repository_uuid: '{synthetic-repository}', repository_url: runFixture.source_url, number: '42', commit: 'c'.repeat(40), state: 'OPEN', current: false, feedback_status: 'failed', runs: [{ run: { ...runFixture, id: reviewID(46), commit: 'c'.repeat(40), plan: { ...runFixture.plan!, status: 'invalidated' } }, needs_attention: false }] }] };
async function fixture(page: Page) {
    const model = { authenticated: false, revoked: false, mutations: [] as string[], reads: [] as string[] };
    await page.route('**/api/v1/**', async route => {
        const r = route.request(), u = new URL(r.url()), p = u.pathname;
        const send = (json: unknown, status = 200) => route.fulfill({ json, status });
        if (p.endsWith('/session/login')) { model.authenticated = true; return send({ ok: true }); }
        if (p.endsWith('/session/activity')) return send({ ok: true });
        if (p.endsWith('/session')) return model.authenticated ? send(reviewSession) : send({ code: 'SIGN_IN_REQUIRED', message: 'Sign in again.' }, 401);
        if (r.method() !== 'GET') { model.authenticated = false; model.mutations.push(p); return send({ code: 'SIGN_IN_REQUIRED', message: 'Sign in again.' }, 401); }
        if (p.endsWith('/projects')) return send(projects);
        if (model.revoked) return send({ code: 'FORBIDDEN', message: 'Access changed.' }, 403);
        model.reads.push(r.url());
        if (p.endsWith('/pull-requests')) return send(prs);
        if (p.endsWith('/approvals')) return send({ ...approvalSummary, can_apply: true, counted_approvals: 2, apply_blocked_reasons: [] });
        if (p.endsWith('/changes')) return send(changesResponse);
        return send(runResponse);
    });
    return model;
}
test('authenticated PR navigation preserves scope, exact Changes links and both responsive themes', async ({ page }, info) => {
    const m = await fixture(page);
    await page.goto(path);
    await page.getByLabel('Username', { exact: true }).fill('reviewer');
    await page.getByLabel('Password', { exact: true }).fill('synthetic password');
    await page.getByRole('button', { name: 'Sign in', exact: true }).click();
    await expect(page.getByRole('navigation', { name: 'Project views' }).getByRole('link', { name: 'Pull requests' })).toHaveAttribute('aria-current', 'page');
    await expect(page.getByText('Recovery needs attention')).toBeVisible();
    await expect(page.getByText('Historical source · OPEN')).toBeVisible();
    expect(m.reads.some(u => u.includes(`environment_id=${env}`) && u.includes(`project_id=${project}`))).toBe(true);
    for (const theme of ['light', 'dark']) {
        if (theme === 'dark') await page.getByRole('button', { name: 'Dark theme' }).click();
        for (const [name, width, height] of [['wide', 1440, 1000], ['narrow', 390, 844]] as const) {
            await page.setViewportSize({ width, height });
            await expect(page.getByRole('heading', { name: 'Pull request #42' }).first()).toBeVisible();
            expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
            await page.screenshot({ path: info.outputPath(`pr-${name}-${theme}.png`), fullPage: true });
        }
    }
    const changes = page.getByRole('region', { name: 'Pull request 42 current head' }).getByRole('link', { name: 'Changes', exact: true }).first();
    await expect(changes).toHaveAttribute('href', `/o/${org}/p/${project}/runs/${run}?env=${env}&view=changes`);
    await changes.focus(); await page.keyboard.press('Enter');
    await expect(page.getByRole('heading', { name: 'Change summary' })).toBeVisible();
    await page.reload();
    await expect(page.getByRole('heading', { name: 'Change summary' })).toBeVisible();
    expect(m.mutations).toHaveLength(0);
});
test('login and reconnect require a fresh explicit Apply and PR revocation clears protected content', async ({ page, context }) => {
    const m = await fixture(page); m.authenticated = true;
    await page.goto(`/o/${org}/p/${project}/runs/${run}?env=${env}`);
    await page.getByRole('button', { name: 'Apply', exact: true }).click();
    await page.getByRole('button', { name: 'Confirm Apply' }).click();
    await expect(page.getByRole('button', { name: 'Sign in', exact: true })).toBeVisible();
    await page.getByLabel('Username', { exact: true }).fill('reviewer');
    await page.getByLabel('Password', { exact: true }).fill('synthetic password');
    await page.getByRole('button', { name: 'Sign in', exact: true }).click();
    await expect(page.getByRole('button', { name: 'Apply', exact: true })).toBeVisible();
    await expect(page.getByRole('dialog')).toHaveCount(0);
    await context.setOffline(true); await page.waitForFunction(() => !navigator.onLine); await context.setOffline(false); await page.reload();
    await expect(page.getByRole('button', { name: 'Apply', exact: true })).toBeVisible();
    expect(m.mutations).toHaveLength(1);
    await page.goto(path); await expect(page.getByText('Recovery needs attention')).toBeVisible();
    m.revoked = true;
    await expect(page.getByRole('alert').filter({ hasText: 'Access changed.' })).toBeVisible({ timeout: 10000 });
    await expect(page.getByRole('heading', { name: 'Pull request #42' })).toHaveCount(0);
    expect(m.mutations).toHaveLength(1);
});
