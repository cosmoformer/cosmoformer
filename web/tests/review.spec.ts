import { test, expect, type Page } from '@playwright/test';
import { readFileSync } from 'node:fs';
import { changes, changesResponse, reviewSession, projects, stackFixture, runFixture, runResponse, org, project, env, stack, run, attempt, plan, reviewID } from './fixtures/review';
const base = `/o/${org}/p/${project}`;
async function synthetic(page: Page) {
    const model = { session: structuredClone(reviewSession), projects: structuredClone(projects), stacks: [structuredClone(stackFixture)], run: structuredClone(runResponse), changes: structuredClone(changesResponse), mutations: [] as any[], requestStatus: 200, changesStatus: 200, revoked: false, logs: 0, activity: 0 };
    await page.route('**/api/v1/**', async route => {
        const r = route.request(), path = new URL(r.url()).pathname;
        const send = (json: unknown, status = 200) => route.fulfill({ json, status });
        if (path === '/api/v1/session/activity') { model.activity++; return send({ ok: true }); }
        if (path === '/api/v1/session/login') return send({ ok: true });
        if (path === '/api/v1/session/logout') return send({ ok: true });
        if (path === '/api/v1/session') return send(model.session);
        if (r.method() !== 'GET') {
            model.mutations.push(r.postDataJSON());
            return model.requestStatus === 200 ? send({ request_id: 'request', subject_id: run, revision: 1, status: 'queued', operation_id: reviewID(9) }) : send({ code: 'SIGN_IN_REQUIRED', message: 'Sign in again.' }, model.requestStatus);
        }
        if (path.endsWith('/projects')) return send(model.projects);
        if (model.revoked) return send({ code: 'FORBIDDEN', message: 'Current review access was revoked.' }, 403);
        if (path.endsWith('/stacks')) return send({ stacks: model.stacks });
        if (path.endsWith('/runs')) return send({ runs: [model.run.run] });
        if (path.endsWith('/changes')) return model.changesStatus === 200 ? send(model.changes) : send({ code: 'CONFLICT', message: 'Retained Changes are unavailable.' }, model.changesStatus);
        if (path.endsWith('/logs')) {
            const n = model.logs++;
            return send({ cursor: `cursor-${n}`, reset: n === 1, ...(n === 1 ? { reset_reason: 'epoch_changed' } : {}), segments: n === 0 ? [{ sequence: 0, byte_start: 0, byte_end: 30, text: 'Synthetic redacted console\n' }] : n === 1 ? [{ sequence: 1, byte_start: 30, byte_end: 40, gap: { reason: 'archive_unavailable', unknown_extent: false } }, { sequence: 2, byte_start: 40, byte_end: 40, gap: { reason: 'preview_reset', unknown_extent: true } }] : [], archive_complete: false, has_more: n === 0 });
        }
        return send(model.run);
    });
    return model;
}

test('real retained evidence: scoped HTTPS project, stack, exact run, refresh and Changes', async ({ page }, info) => {
    const c = JSON.parse(readFileSync(new URL('../../.build/testdb/browser.json', import.meta.url), 'utf8'));
    const r = c.review.run;
    await page.goto(`/o/${c.org}/p/${r.project_id}/runs/${r.id}?env=${r.environment_id}&view=changes`);
    await page.getByLabel('Username', { exact: true }).fill(c.review_username);
    await page.getByLabel('Password', { exact: true }).fill(c.review_password);
    await page.getByRole('button', { name: 'Sign in', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Coverage & limitations' })).toBeVisible();
    await expect(page.getByText(/Generic actions only/)).toBeVisible();
    await page.getByText('other_item.fixture', { exact: true }).first().click();
    await expect(page.getByText('9007199254740993', { exact: true })).toBeVisible();
    await page.reload();
    await expect(page.getByRole('heading', { name: 'Change summary' })).toBeVisible();
    await expect(page.getByText(r.commit, { exact: true })).toBeVisible();
    await expect(page.getByRole('link', { name: /Download/ })).toHaveCount(0);
    await page.screenshot({ path: info.outputPath('real-retained-review.png'), fullPage: true });
});

test('risk-ordered Changes, lossless/hidden values, keyboard expansion and responsive themes', async ({ page }, info) => {
    const errors: string[] = []; page.on('pageerror', e => errors.push(e.message));
    await synthetic(page);
    await page.setViewportSize({ width: 1440, height: 1000 });
    await page.goto(`${base}/runs/${run}?env=${env}&view=changes`);
    await expect(page.getByRole('heading', { name: 'Coverage & limitations' })).toBeVisible();
    await expect(page.getByText(/Partial coverage/)).toBeVisible();
    const summary = page.locator('.resource summary').first();
    await summary.focus(); await page.keyboard.press('Enter');
    await expect(page.getByText('9007199254740993', { exact: true }).first()).toBeVisible();
    await expect(page.getByText('[hidden]', { exact: true }).first()).toBeVisible();
    await expect(page.getByText('[absent]', { exact: true }).first()).toBeVisible();
    await expect(page.getByText('null', { exact: true }).first()).toBeVisible();
    await expect(page.getByText('"safe sibling"', { exact: true }).first()).toBeVisible();
    await expect(page.locator('img[src="x"]')).toHaveCount(0);
    await page.evaluate(() => window.scrollTo(0, 0));
    await page.screenshot({ path: info.outputPath('changes-wide-light.png'), fullPage: true });
    await page.getByRole('button', { name: 'Dark theme' }).click();
    await page.screenshot({ path: info.outputPath('changes-wide-dark.png'), fullPage: true });
    await page.setViewportSize({ width: 390, height: 844 });
    await page.screenshot({ path: info.outputPath('changes-narrow-dark.png'), fullPage: true });
    await page.getByRole('button', { name: 'Light theme' }).click();
    await page.screenshot({ path: info.outputPath('changes-narrow-light.png'), fullPage: true });
    expect(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth)).toBe(true);
    await page.getByLabel('Search Changes').fill('hidden');
    await expect(page.locator('.resource summary').first()).toContainText('Hidden resource');
    await page.getByLabel('Search Changes').clear();
    await page.getByLabel('Action', { exact: true }).selectOption('delete');
    await expect(page.locator('.resource-section').first().locator('details')).toHaveCount(1);
    expect(errors).toEqual([]);
});

test('keyboard users can reach and scroll overflow values and retained console output', async ({ page }) => {
    await synthetic(page);
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto(`${base}/runs/${run}?view=changes`);
    const summary = page.locator('.resource summary').first();
    await summary.focus(); await page.keyboard.press('Enter'); await page.keyboard.press('Tab');
    const values = page.getByRole('region', { name: 'Before and after values' }).first();
    await expect(values).toBeFocused();
    await expect(values).toHaveCSS('outline-style', 'solid');
    expect(await values.evaluate(e => e.scrollWidth > e.clientWidth)).toBe(true);
    await page.keyboard.press('ArrowRight');
    await expect.poll(() => values.evaluate(e => e.scrollLeft)).toBeGreaterThan(0);

    let requests = 0;
    const text = Array.from({ length: 100 }, (_, i) => `Retained redacted line ${i}`).join('\n');
    await page.route('**/logs?*', async route => route.fulfill({ json: {
        cursor: 'keyboard-cursor', reset: false,
        segments: requests++ === 0 ? [{ sequence: 0, byte_start: 0, byte_end: text.length, text }] : [],
        archive_complete: true, has_more: false,
    } }));
    await page.goto(`${base}/runs/${run}?view=logs`);
    const output = page.getByRole('region', { name: 'Console output' });
    await expect(output).toContainText('Retained redacted line 99');
    const pause = page.getByRole('button', { name: 'Pause', exact: true });
    await pause.click(); await page.getByRole('button', { name: 'Follow', exact: true }).focus();
    await page.keyboard.press('Tab');
    await expect(output).toBeFocused();
    await expect(output).toHaveCSS('outline-style', 'solid');
    await page.keyboard.press('Home');
    await expect.poll(() => output.evaluate(e => e.scrollTop)).toBe(0);
    await page.keyboard.press('PageDown');
    await expect.poll(() => output.evaluate(e => e.scrollTop)).toBeGreaterThan(0);
});

test('project and stack actions retain context and navigate to the exact accepted run', async ({ page }) => {
    const model = await synthetic(page);
    await page.goto(base);
    await page.getByLabel('Environment filter').selectOption(env);
    await page.getByRole('link', { name: 'Network edge', exact: true }).click();
    await expect(page.getByText('b'.repeat(40), { exact: true })).toBeVisible();
    await page.getByRole('button', { name: 'Check source', exact: true }).click();
    await page.keyboard.press('Escape');
    await expect(page.getByRole('dialog')).not.toBeVisible();
    await page.getByRole('button', { name: 'Request plan', exact: true }).click();
    await page.getByLabel('Source', { exact: true }).selectOption('pr');
    await page.getByLabel('PR number').fill('42');
    await page.getByRole('button', { name: 'Confirm request' }).click();
    await expect(page).toHaveURL(new RegExp(`/runs/${run}`));
    await expect(page.getByText('a'.repeat(40), { exact: true })).toBeVisible();
    expect(model.mutations).toEqual([{ intent: 'native_plan', source: { kind: 'pr', pr_number: 42 }, priority: 'medium' }]);
    await page.reload();
    expect(model.mutations).toHaveLength(1);
    await expect(page.getByText('Pull request #42 · exact run source')).toBeVisible();
});

test('historical, no-change, output-only and unavailable evidence remain distinct', async ({ page }) => {
    const model = await synthetic(page);
    model.run.run.plan = { ...plan, status: 'expired' };
    await page.goto(`${base}/runs/${run}?view=changes`);
    await expect(page.getByText(/Historical review only/)).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Change summary' })).toBeVisible();
    model.changes.changes = { ...changes, resources: [], outputs: [], drift: [], findings: [], coverage: [], summary: { actions: {}, findings: {} } };
    await page.reload(); await expect(page.getByText('No planned resource or output changes.')).toBeVisible();
    model.changes.changes = { ...model.changes.changes, outputs: changes.outputs };
    await page.reload(); await expect(page.getByText('No planned resource or output changes.')).toHaveCount(0);
    await expect(page.getByText('public_summary', { exact: true })).toBeVisible();
    model.changesStatus = 409;
    await page.reload(); await expect(page.getByText(/Exact retained Changes are unavailable/)).toBeVisible();
    await expect(page.getByRole('heading', { name: 'Change summary' })).toHaveCount(0);
});

test('log follow, pause, bounded archive continuation, reset, gaps and revocation', async ({ page }, info) => {
    const model = await synthetic(page);
    await page.goto(`${base}/runs/${run}?view=logs`);
    await expect(page.getByText(/Synthetic redacted console/)).toBeVisible();
    await expect(page.getByText(/Preview reset \(epoch changed\)/)).toBeVisible();
    await expect(page.getByText(/Gap: archive unavailable/)).toHaveText(/30–40 redacted bytes/);
    await expect(page.getByText(/Gap: preview reset/)).toHaveText(/unknown extent/);
    expect(await page.evaluate(() => window.scrollY)).toBe(0);
    await page.getByRole('button', { name: 'Pause', exact: true }).click();
    const count = model.logs, activity = model.activity;
    await page.waitForTimeout(2300);
    expect(model.logs).toBe(count); expect(model.activity).toBe(activity);
    await page.evaluate(() => window.scrollTo(0, 0));
    await page.screenshot({ path: info.outputPath('logs-gaps-paused.png'), fullPage: true });
    model.revoked = true;
    await page.getByRole('button', { name: 'Follow', exact: true }).click();
    await expect(page.getByText(/Protected content has been cleared/)).toBeVisible();
    await expect(page.getByText(run, { exact: true })).toHaveCount(0);
    await expect(page.locator('.console')).toHaveCount(0);
});

test('expiry requires a fresh user action after re-login and never replays planning', async ({ page }) => {
    const model = await synthetic(page);model.requestStatus = 401;
    await page.goto(`${base}/stacks/${stack}?env=${env}`);
    await page.getByRole('button', { name: 'Request plan', exact: true }).click();
    await page.getByRole('button', { name: 'Confirm request' }).click();
    await expect(page.getByRole('heading', { name: 'Sign in to your workspace' })).toBeVisible();
    await page.getByLabel('Username', { exact: true }).fill('reviewer');
    await page.getByLabel('Password', { exact: true }).fill('synthetic login value');
    await page.getByRole('button', { name: 'Sign in', exact: true }).click();
    await expect(page.getByRole('heading', { name: 'Current source' })).toBeVisible();
    expect(model.mutations).toHaveLength(1);
    await expect(page.getByRole('dialog')).not.toBeVisible();
});

test('empty access and missing grants expose no execution or raw controls', async ({ page }) => {
    const model = await synthetic(page);model.projects = { projects: [], can_manage: false };
    await page.goto(base);await expect(page.getByText('No project access')).toBeVisible();
    model.projects = projects;model.stacks = [{ ...stackFixture, can_plan: false, can_native_plan: false, can_review: false, plan_reason: 'Planning permission is required.' }];
    await page.goto(`${base}/stacks/${stack}`);
    await expect(page.getByRole('button', { name: 'Request plan', exact: true })).toBeDisabled();
    await expect(page.getByText(/Viewer grant is required/)).toBeVisible();
    await expect(page.getByRole('link', { name: /Download/ })).toHaveCount(0);
});

test('transient log retry retains history and offline polling resumes without replay', async ({ page, context }) => {
    await synthetic(page);
    let requests = 0;
    await page.route('**/logs?*', async route => {
        const n = requests++;
        if (n === 1) return route.fulfill({ status: 503, json: { code: 'UNAVAILABLE', message: 'Temporary archive interruption.' } });
        return route.fulfill({ json: { cursor: 'accepted-cursor', reset: false, segments: n === 0 ? [{ sequence: 0, byte_start: 0, byte_end: 8, text: 'retained' }] : [], archive_complete: true, has_more: false } });
    });
    await page.goto(`${base}/runs/${run}?view=logs&env=${env}`);
    await expect(page.getByText('retained', { exact: true })).toBeVisible();
    await expect(page.getByRole('alert')).toContainText('Temporary archive interruption.');
    await expect(page.getByText('retained', { exact: true })).toBeVisible();
    await page.getByRole('button', { name: 'Retry logs' }).click();
    await expect(page.getByRole('alert')).toHaveCount(0);
    await expect(page.getByText('retained', { exact: true })).toBeVisible();
    await context.setOffline(true);
    const paused = requests;
    await page.waitForTimeout(2300);
    expect(requests).toBe(paused);
    await context.setOffline(false);
    await expect.poll(() => requests).toBeGreaterThan(paused);
    await expect(page.getByText('retained', { exact: true })).toBeVisible();
    await page.getByRole('navigation', { name: 'Run context' }).getByRole('link', { name: 'Orbital platform' }).click();
    await expect(page).toHaveURL(new RegExp(`env=${env}`));
    await page.reload();
    await expect(page.getByLabel('Environment filter')).toHaveValue(env);
    await expect(page.getByRole('link', { name: 'Project workspace' })).toHaveAttribute('href', `${base}?env=${env}`);
});
