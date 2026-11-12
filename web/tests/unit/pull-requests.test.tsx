import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Route, Routes } from 'react-router';
import { beforeEach, expect, it, vi } from 'vitest';
import Project from '../../app/routes/project';
import { AppearanceProvider } from '../../app/lib/appearance';
import { SessionProvider } from '../../app/lib/session';
import { renderPage, reply } from './helpers';
import { reviewSession, projects, runFixture, org, project, env, run, reviewID } from '../fixtures/review';
import type { Schema } from '../../app/lib/api';
let response: Schema['PullRequestsOutputBody'], revoked: boolean;
beforeEach(() => {
    revoked = false;
    response = { pull_requests: [{ id: reviewID(40), repository_id: reviewID(8), repository_uuid: '{synthetic}', repository_url: runFixture.source_url, number: '42', commit: runFixture.commit, state: 'OPEN', current: true, feedback_status: 'failed', runs: [{ run: { ...runFixture, reason: 'NO_CHANGES' }, needs_attention: true }], runs_next: run }], next_cursor: reviewID(40) };
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL) => {
        const path = String(input);
        if (path.endsWith('/session')) return reply(reviewSession);
        if (path.endsWith('/projects')) return reply(projects);
        if (revoked) return reply({ code: 'FORBIDDEN', message: 'Access changed.' }, 403);
        return reply(response);
    }));
});
function mount() { return renderPage(<AppearanceProvider><SessionProvider><Routes><Route path="/o/:org/p/:project" element={<Project/>}/></Routes></SessionProvider></AppearanceProvider>, `/o/${org}/p/${project}?env=${env}&view=pull-requests`); }
it('groups exact scoped runs and keeps review separate from deployment and delivery failure', async () => {
    mount();
    expect(await screen.findByText('Current source · OPEN')).toBeVisible();
    expect(screen.getByText('Feedback delivery: Failed — operator attention required')).toHaveAttribute('role', 'alert');
    expect(screen.getByText('No changes')).toBeVisible();
    expect(screen.getByText('Not requested')).toBeVisible();
    expect(screen.getByText('Recovery needs attention')).toBeVisible();
    expect(screen.getByRole('link', { name: 'Changes' })).toHaveAttribute('href', `/o/${org}/p/${project}/runs/${run}?env=${env}&view=changes`);
    expect(screen.queryByRole('button', { name: 'Apply' })).not.toBeInTheDocument();
    expect(vi.mocked(fetch).mock.calls.every(([, options]) => options?.method === 'GET')).toBe(true);
});
it('paginates heads and stack runs with the current project and environment', async () => {
    mount(); const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Older pull request heads' }));
    await waitFor(() => expect(vi.mocked(fetch).mock.calls.some(([p]) => String(p).includes(`before=${reviewID(40)}`))).toBe(true));
    await user.click(screen.getByRole('button', { name: 'More stack runs' }));
    await waitFor(() => expect(vi.mocked(fetch).mock.calls.some(([p]) => String(p).includes(`run_before=${run}`) && String(p).includes(`head=${reviewID(40)}`) && String(p).includes(`environment_id=${env}`))).toBe(true));
    expect(screen.getByRole('link', { name: 'Newest pull requests' })).toHaveAttribute('href', `/o/${org}/p/${project}?view=pull-requests&env=${env}`);
});
it('labels obsolete source and clears the old protected page on a denied refresh', async () => {
    response.pull_requests[0].current = false;
    mount();
    expect(await screen.findByText('Historical source · OPEN')).toBeVisible();
    expect(screen.getByText(/Historical results preserve their original source/)).toBeVisible();
    revoked = true;
    await userEvent.setup().click(screen.getByRole('button', { name: 'Older pull request heads' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Access changed.');
    expect(screen.queryByRole('heading', { name: 'Pull request #42' })).not.toBeInTheDocument();
});
it('shows an empty scoped state without introducing any execution action', async () => {
    response = { pull_requests: [] }; mount();
    expect(await screen.findByRole('heading', { name: 'No visible pull request runs' })).toBeVisible();
    expect(screen.queryByRole('button', { name: 'Apply' })).not.toBeInTheDocument();
});
