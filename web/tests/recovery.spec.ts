import { test, expect, type Page } from '@playwright/test';
import { reviewSession, projects, runResponse, approvalSummary, org, project, env, run, attempt, reviewID } from './fixtures/review';
import type { Schema } from '../app/lib/api';
const path = `/o/${org}/p/${project}/runs/${run}?env=${env}`;
async function fixture(page: Page) {
    const body = structuredClone(runResponse);
    body.run.status = 'needs_attention'; body.run.plan = undefined;
    body.attempts = [{ id: attempt, phase: 'execution', state: 'uncertain', outcome: '', created_at: body.run.created_at, revision: 8, command_ordinal: 3, can_control: true, contact_warning: true, controls: [{ control_id: reviewID(32), kind: 'cancel', state: 'delivered', command_ordinal: 3, signal_sent: 'SIGINT' }] }];
    body.recovery = { id: reviewID(30), stack_id: body.run.stack_id, target_id: reviewID(31), origin_attempt_id: attempt, status: 'open', revision: 3, evidence_revision: 2, stop_verified: false, reconciliation_complete: false, infrastructure_outcome: 'unknown', stop_authority: 'unverified', counted_approvals: 0, minimum: 2, own_counted: false, can_submit_evidence: true, can_approve: false, can_clear: false, can_plan: false };
    const m = { body, mutations: [] as { path: string; body: any }[], denied: false, status: 202 };
    await page.route('**/api/v1/**', async route => {
        const req = route.request(), p = new URL(req.url()).pathname;
        const send = (json: unknown, status = 200) => route.fulfill({ json, status });
        if (p.endsWith('/session/activity')) return send({ ok: true });
        if (p.endsWith('/session')) return send(reviewSession);
        if (p.endsWith('/projects')) return send(projects);
        if (req.method() !== 'GET') { m.mutations.push({ path: p, body: req.postDataJSON() }); if (m.status >= 400) return send({ code: 'FORBIDDEN', message: 'Access changed.' }, m.status); return send({ control_id: reviewID(34), kind: 'force_stop', state: 'requested' }, 202); }
        if (m.denied) return send({ code: 'FORBIDDEN', message: 'Access changed.' }, 403);
        if (p.endsWith('/approvals')) return send(approvalSummary);
        return send(m.body);
    });
    return m;
}
test('exact Force Stop keyboard confirmation, no replay, and truthful stop status', async ({ page }) => {
    const m = await fixture(page); await page.goto(path);
    await expect(page.getByText(/Stop is unverified/)).toBeVisible(); await expect(page.getByText(/Observed does not mean stopped/)).toBeVisible();
    await page.getByRole('button', { name: 'Force Stop', exact: true }).click();
    const d = page.getByRole('dialog');
    for (const value of [m.body.run.stack_id, m.body.run.environment_id, attempt]) await expect(d.getByText(value, { exact: true })).toBeVisible();
    await page.keyboard.press('Escape'); expect(m.mutations).toHaveLength(0);
    await page.getByRole('button', { name: 'Force Stop', exact: true }).click(); await d.getByRole('button', { name: 'Confirm Force Stop for this attempt' }).focus(); await page.keyboard.press('Enter');
    await expect(page.getByRole('status')).toContainText('Request recorded');
    expect(m.mutations).toHaveLength(1); expect(m.mutations[0].body).toEqual({ expected_attempt_revision: 8, confirm: true, confirmed_context: { stack_id: m.body.run.stack_id, environment_id: env, attempt_id: attempt } });
    await page.reload(); expect(m.mutations).toHaveLength(1); await expect(page.getByRole('button', { name: 'Clear recovery' })).toBeDisabled(); await expect(page.getByRole('button', { name: 'Request diagnostic plan' })).toBeDisabled();
});
test('recovery wide/narrow layouts in both themes and cleared protected context', async ({ page }, info) => {
    const m = await fixture(page); await page.goto(path);
    await page.getByText('Submit or revise recovery evidence', { exact: true }).click();
    for (const theme of ['light', 'dark']) {
        if (theme === 'dark') await page.getByRole('button', { name: 'Dark theme' }).click();
        for (const width of [1440, 390]) {
            await page.setViewportSize({ width, height: width === 390 ? 844 : 1100 });
            await page.screenshot({ path: info.outputPath(`recovery-${width}-${theme}.png`), fullPage: true });
            expect(await page.evaluate(() => document.documentElement.scrollWidth <= window.innerWidth)).toBe(true);
            await page.getByRole('button', { name: 'Force Stop', exact: true }).click();
            const confirmation = page.getByRole('dialog');
            await page.screenshot({ path: info.outputPath(`force-stop-${width}-${theme}.png`), fullPage: true });
            expect(await confirmation.evaluate(e => e.scrollWidth <= e.clientWidth)).toBe(true);
            await page.keyboard.press('Escape');
        }
    }
    await page.getByLabel('Stop statement', { exact: true }).fill('Protected operator draft to clear after access loss.');
    m.status = 403; await page.getByRole('button', { name: 'Force Stop', exact: true }).click(); await page.getByRole('button', { name: 'Confirm Force Stop for this attempt' }).click();
    await expect(page.getByRole('heading', { name: 'Review access changed' })).toBeVisible();
    await expect(page.getByText('Protected operator draft to clear after access loss.')).toHaveCount(0); await expect(page.getByRole('dialog')).toHaveCount(0); expect(m.mutations).toHaveLength(1);
});
