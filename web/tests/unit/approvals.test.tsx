import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, expect, it, vi } from 'vitest';
import { Approvals } from '../../app/components/approvals';
import { deferred, renderPage, reply } from './helpers';
import { approvalSummary, runFixture, plan, reviewID } from '../fixtures/review';

let summary = structuredClone(approvalSummary), status = 200, mutations: RequestInit[], denied: ReturnType<typeof vi.fn<() => void>>;
beforeEach(() => {
    summary = structuredClone(approvalSummary); status = 200; mutations = []; denied = vi.fn();
    vi.mocked(fetch).mockImplementation(async (_path, options) => {
        if (options?.method === 'GET') return reply(summary);
        mutations.push(options!);
        if (status !== 200) return reply({ code: status === 401 ? 'SIGN_IN_REQUIRED' : 'SOURCE_UNAVAILABLE', message: 'Explicit action required.' }, status);
        const approved = options?.method === 'POST';
        summary = { ...summary, own_approved: approved, own_counted: approved, can_withdraw: approved, counted_approvals: approved ? 1 : 0 };
        return reply({ ...summary, vote_event_id: reviewID(21) });
    });
});
function mount() { return renderPage(<Approvals org="synthetic-org" run={runFixture} onDenied={denied}/>); }
it('confirms exact stack, environment, commit, plan revision, manifest and expiry, then withdraws', async () => {
    mount(); const user = userEvent.setup();
    expect(await screen.findByText('0 of 2')).toBeVisible();
    await user.click(screen.getByRole('button', { name: 'Approve' }));
    const dialog = within(screen.getByRole('dialog'));
    for (const text of [runFixture.stack_name, runFixture.environment_name, runFixture.stack_id, runFixture.environment_id, runFixture.commit, plan.id, plan.manifest_digest]) expect(dialog.getByText(text, { exact: true })).toBeVisible();
    expect(dialog.getByText(/2026/)).toBeVisible();
    await user.click(dialog.getByRole('button', { name: 'Confirm approval' }));
    expect(await screen.findByText('Your approval currently counts.')).toBeVisible();
    expect(screen.getByRole('button', { name: 'Approve' })).toBeDisabled();
    expect(JSON.parse(String(mutations[0].body))).toEqual({ expected_plan_revision: 2, manifest_digest: plan.manifest_digest });
    await user.click(screen.getByRole('button', { name: 'Withdraw my approval' }));
    expect(await screen.findByText('Your approval was withdrawn.')).toBeVisible();
    expect(mutations[1].method).toBe('DELETE');
    expect(JSON.parse(String(mutations[1].body))).toEqual({ expected_plan_revision: 2 });
});
it('keeps historical votes visible and withdrawal available after approval authority loss', async () => {
    summary = { ...summary, eligible: false, own_approved: true, can_approve: false, can_withdraw: true, blocked_reasons: ['PLAN_INVALIDATED', 'APPROVAL_AUTHORITY_REQUIRED'] };
    mount();
    expect(await screen.findByText(/Your past vote is retained/)).toBeVisible();
    expect(screen.getByRole('button', { name: 'Approve' })).toBeDisabled();
    expect(screen.getByText(/Retained Changes and past votes remain historical/)).toBeVisible();
    expect(screen.getByRole('button', { name: 'Withdraw my approval' })).toBeEnabled();
});
it('handles unavailable summaries and mismatched identities without a confirm action', async () => {
    vi.mocked(fetch).mockResolvedValueOnce(reply({ code: 'UNAVAILABLE', message: 'Temporarily unavailable.' }, 503));
    mount(); await screen.findByText('Temporarily unavailable.');
    summary = { ...summary, manifest_digest: 'f'.repeat(64) };
    fireEvent.click(screen.getByRole('button', { name: /Try again/ }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Plan identity changed');
    expect(screen.queryByRole('button', { name: 'Approve' })).not.toBeInTheDocument();
});
it.each([401, 403, 404, 409, 503])('never replays a %s mutation and clears confirmation', async code => {
    status = code; mount();
    fireEvent.click(await screen.findByRole('button', { name: 'Approve' }));
    fireEvent.click(screen.getByRole('button', { name: 'Confirm approval' }));
    await waitFor(() => expect(mutations).toHaveLength(1));
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    if ([403, 404].includes(code)) expect(denied).toHaveBeenCalled();
    else expect(await screen.findByRole('alert')).toBeVisible();
    expect(mutations).toHaveLength(1);
});
it('cancels confirmation by button and Escape without recording a vote', async () => {
    mount(); fireEvent.click(await screen.findByRole('button', { name: 'Approve' }));
    fireEvent.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: 'Approve' }));
    fireEvent(screen.getByRole('dialog'), new Event('cancel', { bubbles: true, cancelable: true }));
    expect(mutations).toHaveLength(0);
});
it('serializes votes and ignores late responses after unmount', async () => {
    const late = deferred<Response>();
    vi.mocked(fetch).mockImplementation(async (_path, options) => options?.method === 'GET' ? reply(summary) : late.promise);
    const mounted = mount(); fireEvent.click(await screen.findByRole('button', { name: 'Approve' }));
    fireEvent.click(screen.getByRole('button', { name: 'Confirm approval' }));
    expect(screen.getByRole('button', { name: 'Checking…' })).toBeDisabled();
    fireEvent(screen.getByRole('dialog'), new Event('cancel', { bubbles: true, cancelable: true }));
    const signal = vi.mocked(fetch).mock.calls.at(-1)?.[1]?.signal;
    mounted.unmount(); expect(signal?.aborted).toBe(true);
    await act(async () => late.resolve(reply({ ...summary, vote_event_id: reviewID(22) })));
    expect(denied).not.toHaveBeenCalled();
});
it('removes action context and protected content on polling revocation', async () => {
    vi.useFakeTimers(); mount();
    await act(async () => { await Promise.resolve(); });
    vi.mocked(fetch).mockResolvedValueOnce(reply({ code: 'FORBIDDEN', message: 'Review access revoked.' }, 403));
    await act(async () => { vi.advanceTimersByTime(5000); });
    expect(denied).toHaveBeenCalled();
    expect(screen.queryByText('0 of 2')).not.toBeInTheDocument();
});
