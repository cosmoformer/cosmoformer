import { test, expect, type Page } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { approvalSummary, reviewSession, projects, runResponse, org, project, env, run, reviewID } from './fixtures/review';
const path = `/o/${org}/p/${project}/runs/${run}?env=${env}`;
async function synthetic(page: Page) {
    const m = { approvals: structuredClone(approvalSummary), run: structuredClone(runResponse), mutations: [] as { method: string; body: any }[], mutationStatus: 200, revoked: false, signedIn: true, activity: 0 };
    await page.route('**/api/v1/**', async route => {
        const r = route.request(), p = new URL(r.url()).pathname;
        const send = (json: unknown, status = 200) => route.fulfill({ json, status });
        if (p.endsWith('/session/login')) { m.signedIn = true; return send({ ok: true }); }
        if (p.endsWith('/session/logout')) { m.signedIn = false; return send({ ok: true }); }
        if (p.endsWith('/session/activity')) { m.activity++; return send({ ok: true }); }
        if (!m.signedIn) return send({ code: 'SIGN_IN_REQUIRED', message: 'Sign in again.' }, 401);
        if (p.endsWith('/session')) return send(reviewSession);
        if (p.endsWith('/projects')) return send(projects);
        if (r.method() !== 'GET') {
            m.mutations.push({ method: r.method(), body: r.postDataJSON() });
            if (m.mutationStatus !== 200) { if (m.mutationStatus === 401) m.signedIn = false; return send({ code: 'SIGN_IN_REQUIRED', message: 'Sign in and start the action again.' }, m.mutationStatus); }
            const approved = r.method() === 'POST';
            m.approvals = { ...m.approvals, own_approved: approved, own_counted: approved, can_withdraw: approved, counted_approvals: approved ? 1 : 0 };
            return send({ ...m.approvals, vote_event_id: reviewID(21) });
        }
        if (m.revoked) return send({ code: 'FORBIDDEN', message: 'Current access revoked.' }, 403);
        if (p.endsWith('/approvals')) return send(m.approvals);
        return send(m.run);
    });
    return m;
}
test('real PostgreSQL approval binds retained evidence and withdraws from the exact run', async ({ page }) => {
    const c = JSON.parse(readFileSync(new URL('../../.build/testdb/browser.json', import.meta.url), 'utf8'));
    const r = c.review.run;
    await page.goto(`/o/${c.org}/p/${r.project_id}/runs/${r.id}?env=${r.environment_id}`);
    await page.getByLabel('Username', { exact: true }).fill(c.review_username);
    await page.getByLabel('Password', { exact: true }).fill(c.review_password);
    await page.getByRole('button', { name: 'Sign in', exact: true }).click();
    await expect(page.getByText('0 of 1', { exact: true })).toBeVisible();
    await page.getByRole('button', { name: 'Approve', exact: true }).click();
    const dialog = page.getByRole('dialog');
    await expect(dialog.getByText(r.commit, { exact: true })).toBeVisible();
    await expect(dialog.getByText(r.plan.manifest_digest, { exact: true })).toBeVisible();
    await page.getByRole('button', { name: 'Confirm approval' }).click();
    await expect(page.getByText('1 of 1', { exact: true })).toBeVisible();
    await page.reload();
    await expect(page.getByText('Your approval currently counts.')).toBeVisible();
    await page.getByRole('button', { name: 'Withdraw my approval' }).click();
    await expect(page.getByText('0 of 1', { exact: true })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Apply', exact: true })).toHaveCount(0);
});
test('exact confirmation, distinct counts, withdrawal and wide/narrow theme layouts', async ({ page }, info) => {
    const m = await synthetic(page);
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.goto(path);
    await expect(page.getByText('0 of 2', { exact: true })).toBeVisible();
    await page.getByRole('button', { name: 'Approve', exact: true }).click();
    const dialog = page.getByRole('dialog');
    await expect(dialog.getByText(m.run.run.stack_name, { exact: true })).toBeVisible();
    await expect(dialog.getByText(m.run.run.environment_name, { exact: true })).toBeVisible();
    for (const text of [m.run.run.stack_id, m.run.run.environment_id, m.run.run.commit, m.run.run.plan!.id, m.run.run.plan!.manifest_digest]) await expect(dialog.getByText(text, { exact: true })).toBeVisible();
    await page.screenshot({ path: info.outputPath('approval-confirm-wide-light.png'), fullPage: true });
    await page.keyboard.press('Escape');
    expect(m.mutations).toHaveLength(0);
    await page.getByRole('button', { name: 'Dark theme' }).click();
    await page.setViewportSize({ width: 390, height: 844 });
    await page.getByRole('button', { name: 'Approve', exact: true }).click();
    await page.screenshot({ path: info.outputPath('approval-confirm-narrow-dark.png'), fullPage: true });
    expect(await dialog.evaluate(e => e.scrollWidth <= e.clientWidth)).toBe(true);
    await page.getByRole('button', { name: 'Confirm approval' }).click();
    await expect(page.getByText('1 of 2', { exact: true })).toBeVisible();
    await expect(page.getByRole('button', { name: 'Approve', exact: true })).toBeDisabled();
    expect(m.mutations[0].body).toEqual({ expected_plan_revision: 2, manifest_digest: m.run.run.plan!.manifest_digest });
    await page.screenshot({ path: info.outputPath('approval-narrow-dark.png'), fullPage: true });
    await page.getByRole('button', { name: 'Light theme' }).click();
    await page.screenshot({ path: info.outputPath('approval-narrow-light.png'), fullPage: true });
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.screenshot({ path: info.outputPath('approval-wide-light.png'), fullPage: true });
    await page.getByRole('button', { name: 'Dark theme' }).click();
    await page.screenshot({ path: info.outputPath('approval-wide-dark.png'), fullPage: true });
    await page.getByRole('button', { name: 'Withdraw my approval' }).click();
    await expect(page.getByText('0 of 2', { exact: true })).toBeVisible();
    expect(m.mutations[1].method).toBe('DELETE');
});
test('historical plans retain votes, disable approval and allow own withdrawal', async ({ page }) => {
    const m = await synthetic(page);
    m.run.run.plan!.status = 'invalidated'; m.run.run.plan!.revision = 3;
    m.approvals = { ...m.approvals, revision: 3, eligible: false, counted_approvals: 0, own_approved: true, can_approve: false, can_withdraw: true, blocked_reasons: ['PLAN_INVALIDATED', 'APPROVAL_AUTHORITY_REQUIRED'] };
    await page.goto(path);
    await expect(page.getByText(/Historical review only/)).toBeVisible();
    await expect(page.getByRole('button', { name: 'Approve', exact: true })).toBeDisabled();
    await expect(page.getByText(/Your past vote is retained/)).toBeVisible();
    await page.getByRole('button', { name: 'Withdraw my approval' }).click();
    await expect(page.getByText('Your approval was withdrawn.')).toBeVisible();
});
test('scoped review revocation clears protected context and a pending confirmation', async ({ page }) => {
    const m = await synthetic(page); await page.clock.install();
    await page.goto(path);
    await page.getByRole('button', { name: 'Approve', exact: true }).click();
    m.revoked = true;
    await page.clock.fastForward(5000);
    await expect(page.getByRole('heading', { name: 'Review access changed' })).toBeVisible();
    await expect(page.getByText(m.run.run.commit, { exact: true })).toHaveCount(0);
    await expect(page.getByRole('dialog')).toHaveCount(0);
});
test('session expiry requires sign-in and a new explicit action, without replay', async ({ page }) => {
    const m = await synthetic(page);
    await page.goto(path);
    await page.getByRole('button', { name: 'Approve', exact: true }).click();
    m.mutationStatus = 401;
    await page.getByRole('button', { name: 'Confirm approval' }).click();
    await expect(page.getByRole('heading', { name: 'Sign in' })).toBeVisible();
    expect(m.mutations).toHaveLength(1); m.mutationStatus = 200;
    await page.getByLabel('Username', { exact: true }).fill('reviewer');
    await page.getByLabel('Password', { exact: true }).fill('synthetic browser password');
    await page.getByRole('button', { name: 'Sign in', exact: true }).click();
    await expect(page.getByRole('button', { name: 'Approve', exact: true })).toBeVisible();
    await expect(page.getByRole('dialog')).toHaveCount(0);
    expect(m.mutations).toHaveLength(1);
    await page.getByRole('button', { name: 'Approve', exact: true }).click();
    await page.getByRole('button', { name: 'Confirm approval' }).click();
    await expect(page.getByText('1 of 2', { exact: true })).toBeVisible();
    expect(m.mutations).toHaveLength(2);
});
test('unauthorized accounts cannot approve and passive polling does not record user activity', async ({ page }) => {
    const m = await synthetic(page); m.approvals.can_approve = false; m.approvals.blocked_reasons = ['APPROVAL_AUTHORITY_REQUIRED'];
    await page.clock.install(); await page.goto(path);
    await expect(page.getByRole('button', { name: 'Approve', exact: true })).toBeDisabled();
    await expect(page.getByText(/Administration, planning, deployment/)).toBeVisible();
    const activity = m.activity;
    await page.clock.fastForward(15000);
    expect(m.activity).toBe(activity); expect(m.mutations).toHaveLength(0);
});
