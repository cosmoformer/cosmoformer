import { fireEvent, screen, waitFor, within } from '@testing-library/react';
import { beforeEach, expect, it, vi } from 'vitest';
import { ExecutionControls } from '../../app/components/execution-controls';
import type { Schema } from '../../app/lib/api';
import { renderPage, reply } from './helpers';
import { org, runFixture, attempt, reviewID } from '../fixtures/review';
const active: Schema['AttemptPublic'] = { id: attempt, phase: 'execution', state: 'running', outcome: '', revision: 8, command_ordinal: 3, can_control: true, created_at: runFixture.created_at, contact_warning: true };
export const recovery: Schema['RecoveryPublic'] = { id: reviewID(30), stack_id: runFixture.stack_id, target_id: reviewID(31), origin_attempt_id: attempt, status: 'open', revision: 3, evidence_revision: 2, stop_verified: true, reconciliation_complete: true, infrastructure_outcome: 'unknown', stop_authority: 'manual_host', minimum: 2, counted_approvals: 1, own_counted: false, can_submit_evidence: true, can_approve: true, can_clear: false, can_plan: true, evidence: { stop_proof: { method: 'host', statement: 'Host stop independently verified.', protected_reference: '' }, reconciliation: { complete: true, statement: 'State and infrastructure reconciled.', protected_references: [] } } };
let mutations: RequestInit[], denied: ReturnType<typeof vi.fn<() => void>>, reload: ReturnType<typeof vi.fn<() => void>>;
beforeEach(() => { mutations = []; denied = vi.fn(); reload = vi.fn(); vi.mocked(fetch).mockImplementation(async (_p, options) => { mutations.push(options!); return reply({ control_id: reviewID(32), kind: 'force_stop', state: 'requested' }, 202); }); });
function mount(r?: Schema['RecoveryPublic']) { return renderPage(<ExecutionControls org={org} run={runFixture} attempts={[active]} recovery={r} reload={reload} onDenied={denied}/>); }
it('confirms Force Stop with exact attempt, stack, environment and revision; Escape never sends', async () => {
    mount(); fireEvent.click(screen.getByRole('button', { name: 'Force Stop' }));
    const dialog = within(screen.getByRole('dialog'));
    for (const text of [runFixture.stack_id, runFixture.environment_id, attempt]) expect(dialog.getByText(text, { exact: true })).toBeVisible();
    fireEvent(screen.getByRole('dialog'), new Event('cancel', { bubbles: true, cancelable: true })); expect(mutations).toHaveLength(0);
    fireEvent.click(screen.getByRole('button', { name: 'Force Stop' })); fireEvent.click(screen.getByRole('button', { name: 'Confirm Force Stop for this attempt' }));
    await waitFor(() => expect(mutations).toHaveLength(1));
    expect(JSON.parse(String(mutations[0].body))).toEqual({ expected_attempt_revision: 8, confirm: true, confirmed_context: { stack_id: runFixture.stack_id, environment_id: runFixture.environment_id, attempt_id: attempt } });
    expect(await screen.findByRole('status')).toHaveTextContent('Delivery and verified stop'); expect(screen.getByText(/Contact missing/)).toBeVisible();
});
it.each([401, 403, 404, 409, 503])('closes confirmation and never retries a rejected %s mutation', async status => {
    vi.mocked(fetch).mockImplementation(async (_p, options) => { mutations.push(options!); return reply({ code: status === 401 ? 'SIGN_IN_REQUIRED' : 'CONFLICT', message: 'Explicit action required.' }, status); });
    mount(); fireEvent.click(screen.getByRole('button', { name: 'Cancel execution' })); fireEvent.click(screen.getByRole('button', { name: 'Confirm request' }));
    await waitFor(() => expect(mutations).toHaveLength(1)); await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    if ([401, 403, 404].includes(status)) expect(denied).toHaveBeenCalled(); else expect(await screen.findByRole('alert')).toBeVisible();
    expect(mutations).toHaveLength(1);
});
it('keeps manual authority and recovery votes distinct and clearance disabled without enough votes', () => {
    mount(recovery); expect(screen.getByText('manual host')).toBeVisible(); expect(screen.getByRole('button', { name: 'Clear recovery' })).toBeDisabled();
    expect(screen.getByText(/1 of 2 distinct eligible/)).toBeVisible(); expect(screen.getByText(/never deployable/)).toBeVisible();
    expect(screen.queryByText(/protected-host/)).not.toBeInTheDocument();
});
it('binds recovery approval to the exact evidence revision without source checks', async () => {
    vi.mocked(fetch).mockImplementation(async (_p, options) => { mutations.push(options!); return reply({ ...recovery, own_counted: true, counted_approvals: 2, vote_event_id: reviewID(33) }); });
    mount(recovery); fireEvent.click(screen.getByRole('button', { name: 'Approve recovery evidence' })); fireEvent.click(screen.getByRole('button', { name: 'Confirm recovery action' }));
    await waitFor(() => expect(mutations).toHaveLength(1)); expect(JSON.parse(String(mutations[0].body))).toEqual({ expected_evidence_revision: 2 }); await waitFor(() => expect(reload).toHaveBeenCalled());
});
it('closes stale Force Stop context when permission or revision changes', () => {
    const view = mount(); fireEvent.click(screen.getByRole('button', { name: 'Force Stop' }));
    view.rerender(<ExecutionControls org={org} run={runFixture} attempts={[{ ...active, can_control: false }]} reload={reload} onDenied={denied}/>);
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument(); expect(mutations).toHaveLength(0);
});
it('submits bounded stop and reconciliation evidence with protected references and current revision', async () => {
    vi.mocked(fetch).mockImplementation(async (_p, options) => { mutations.push(options!); return reply({ ...recovery, revision: 4, evidence_revision: 3 }); });
    mount(recovery); fireEvent.click(screen.getByText('Submit or revise recovery evidence'));
    fireEvent.change(screen.getByLabelText('Stop authority'), { target: { value: 'agent' } });
    fireEvent.change(screen.getByLabelText('Stop statement'), { target: { value: 'Accepted exact protocol proof.' } });
    fireEvent.change(screen.getByLabelText('Protected stop reference'), { target: { value: attempt } });
    fireEvent.change(screen.getByLabelText('Reconciliation statement'), { target: { value: 'Reconciled independently.' } });
    fireEvent.change(screen.getByLabelText(/Protected reconciliation references/), { target: { value: 'protected-a\n\n protected-b ' } });
    fireEvent.click(screen.getByLabelText('Infrastructure and state reconciliation is complete'));
    fireEvent.click(screen.getByRole('button', { name: 'Save evidence revision' }));
    await waitFor(() => expect(reload).toHaveBeenCalled());
    expect(mutations[0].method).toBe('PATCH');
    expect(JSON.parse(String(mutations[0].body))).toEqual({ expected_revision: 3, stop_proof: { method: 'agent', statement: 'Accepted exact protocol proof.', protected_reference: attempt }, reconciliation: { complete: true, statement: 'Reconciled independently.', protected_references: ['protected-a', 'protected-b'] } });
});
it.each(['clear', 'plan'] as const)('confirms recovery %s with the immutable case and evidence identity', async action => {
    const paths: string[] = [];
    vi.mocked(fetch).mockImplementation(async (p, options) => { paths.push(String(p)); mutations.push(options!); return reply(action === 'clear' ? { ...recovery, status: 'cleared' } : { request_id: reviewID(35), subject_id: reviewID(36), revision: 1, status: 'queued', operation_id: reviewID(37) }); });
    mount({ ...recovery, can_clear: true });
    fireEvent.click(screen.getByRole('button', { name: action === 'clear' ? 'Clear recovery' : 'Request diagnostic plan' }));
    expect(within(screen.getByRole('dialog')).getByText(recovery.id, { exact: true })).toBeVisible();
    fireEvent.click(screen.getByRole('button', { name: 'Go back' })); expect(mutations).toHaveLength(0);
    fireEvent.click(screen.getByRole('button', { name: action === 'clear' ? 'Clear recovery' : 'Request diagnostic plan' }));
    fireEvent.click(screen.getByRole('button', { name: 'Confirm recovery action' }));
    await waitFor(() => expect(reload).toHaveBeenCalled());
    const body = JSON.parse(String(mutations[0].body));
    if (action === 'clear') expect(body).toEqual({ expected_evidence_revision: 2 });
    else { expect(body).toMatchObject({ intent: 'recovery_plan', recovery_case_id: recovery.id, expected_evidence_revision: 2, source: { kind: 'branch' } }); expect(paths[0]).toContain(`/stacks/${recovery.stack_id}/plans`); }
});
it.each([401, 403, 404, 409, 503])('recovery action fails closed on %s without replay', async status => {
    vi.mocked(fetch).mockImplementation(async (_p, options) => { mutations.push(options!); return reply({ code: 'CONFLICT', message: 'Recovery evidence changed.' }, status); });
    mount(recovery); fireEvent.click(screen.getByRole('button', { name: 'Approve recovery evidence' })); fireEvent.click(screen.getByRole('button', { name: 'Confirm recovery action' }));
    await waitFor(() => expect(mutations).toHaveLength(1)); await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument());
    if ([401, 403, 404].includes(status)) expect(denied).toHaveBeenCalled(); else expect(await screen.findByRole('alert')).toHaveTextContent('Recovery evidence changed.');
});
it('escalates explicitly and shows retained requested, observed, and closed states', async () => {
    vi.mocked(fetch).mockImplementation(async (_p, options) => { mutations.push(options!); return reply({ ok: true }); });
    renderPage(<ExecutionControls org={org} run={runFixture} attempts={[active, { ...active, id: reviewID(40), state: 'terminal', closed_at: runFixture.created_at, controls: [{ control_id: reviewID(41), kind: 'cancel', state: 'observed', signal_sent: '', command_ordinal: 2 }] }]} reload={reload} onDenied={denied}/>);
    expect(screen.getByText('Execution positively closed.')).toBeVisible(); expect(screen.getByText(/no signal delivery recorded/)).toBeVisible();
    fireEvent.click(screen.getAllByRole('button', { name: 'Escalate to recovery' })[0]); fireEvent.click(screen.getByRole('button', { name: 'Confirm request' }));
    await waitFor(() => expect(reload).toHaveBeenCalled()); expect(JSON.parse(String(mutations[0].body))).toEqual({ expected_attempt_revision: 8 });
});
it.each(['preflight', 'execution'] as const)('submits independent exact %s stop proof without inventing a clearance', async phase => {
    const paths: string[] = [];
    vi.mocked(fetch).mockImplementation(async (p, options) => { paths.push(String(p)); mutations.push(options!); return reply({ ok: true }); });
    renderPage(<ExecutionControls org={org} run={runFixture} attempts={[{ ...active, phase, state: 'uncertain' }]} reload={reload} onDenied={denied}/>);
    fireEvent.click(screen.getByText(phase === 'preflight' ? 'Submit independent source process stop proof' : 'Submit independent attempt stop proof'));
    fireEvent.change(screen.getByLabelText('Independent stop statement'), { target: { value: 'Exact process and descendants independently stopped.' } });
    fireEvent.change(screen.getByLabelText('Protected independent stop reference'), { target: { value: 'protected-stop' } });
    fireEvent.click(screen.getByRole('button', { name: 'Record independent stop proof' }));
    await waitFor(() => expect(reload).toHaveBeenCalled()); expect(paths[0]).toContain(phase === 'preflight' ? '/source-stop' : '/stop-proof');
    expect(JSON.parse(String(mutations[0].body))).toMatchObject({ expected_revision: 8, reconciliation: { complete: false } });
});
it.each([403, 409])('independent stop proof preserves ownership on %s', async status => {
    vi.mocked(fetch).mockImplementation(async (_p, options) => { mutations.push(options!); return reply({ code: 'CONFLICT', message: 'Proof not accepted.' }, status); });
    renderPage(<ExecutionControls org={org} run={runFixture} attempts={[{ ...active, phase: 'preflight', state: 'uncertain' }]} reload={reload} onDenied={denied}/>);
    fireEvent.click(screen.getByText('Submit independent source process stop proof')); fireEvent.submit(screen.getByRole('button', { name: 'Record independent stop proof' }).closest('form')!);
    await waitFor(() => expect(mutations).toHaveLength(1)); if (status === 403) await waitFor(() => expect(denied).toHaveBeenCalled()); else expect(await screen.findByRole('alert')).toHaveTextContent('Proof not accepted.');
});
it('displays protected references only when included by the authorized response', () => {
    mount({ ...recovery, own_counted: true, evidence: { stop_proof: { method: 'agent', statement: 'Agent proof matched.', protected_reference: 'protected-record' }, reconciliation: { complete: true, statement: 'Reconciled.', protected_references: ['protected-reconciliation'] } } });
    expect(screen.getByText('protected-record')).toBeVisible(); expect(screen.getByText('protected-reconciliation')).toBeVisible(); expect(screen.getByRole('button', { name: 'Approve recovery evidence' })).toBeDisabled();
});
it.each(['approval', 'clear'] as const)('shows trusted-clock rejection for recovery %s without retrying', async action => {
    vi.mocked(fetch).mockImplementation(async (_p, options) => { mutations.push(options!); return reply({ code: 'CLOCK_UNRELIABLE', message: 'Trusted server time is unavailable. Approval is blocked.' }, 503); });
    mount({ ...recovery, can_clear: true });
    fireEvent.click(screen.getByRole('button', { name: action === 'clear' ? 'Clear recovery' : 'Approve recovery evidence' }));
    fireEvent.click(screen.getByRole('button', { name: 'Confirm recovery action' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Trusted server time is unavailable.');
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    expect(mutations).toHaveLength(1); expect(reload).toHaveBeenCalledOnce(); expect(denied).not.toHaveBeenCalled();
});
