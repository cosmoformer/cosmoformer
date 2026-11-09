import { act, fireEvent, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import Settings from '../../app/routes/settings';
import { ids, renderPage, reply } from './helpers';

const id = (n: number) => `${n.toString().padStart(8, '0')}-1111-4111-8111-111111111111`;
const kinds = ['integration', 'pool', 'repository', 'worker', 'secret', 'resource_grant', 'runtime', 'aws_connection'];
const items = kinds.map((kind, i) => ({ id: id(i + 10), name: `Synthetic ${kind}`, kind, revision: 1, enabled: true }));
const project = { id: ids.project, name: 'Synthetic project', can_manage: true, environments: [{ id: ids.env, name: 'Local environment' }] };
let requests: { path: string; method: string; body: Record<string, unknown> }[];
let empty = false, broken = false, denied = false;
beforeEach(() => {
    requests = []; empty = false; broken = false; denied = false;
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const path = String(input), method = init?.method ?? 'GET';
        if (method !== 'GET') {
            requests.push({ path, method, body: JSON.parse(String(init?.body ?? '{}')) });
            if (denied) return reply({ code: 'RECONFIRM_SECRET_WRITE', message: 'Confirm a new secret write.' }, 409);
            if (path.endsWith('/connect')) return reply({ authorization_url: 'https://attacker.example', expires_at: new Date().toISOString() });
            if (path.includes('/workers')) return reply({ id: id(13), token: path.endsWith('/pause') ? '' : 'fictional-once-visible-token', paused: path.endsWith('/pause') });
            if (path.endsWith('/sync')) return reply({ repository_id: id(12), commit: 'a'.repeat(40), stack_ids: [id(40)] });
            if (path.endsWith('/plans')) return reply({ request_id: 'request', subject_id: id(41), revision: 1, status: 'queued', operation_id: id(42) });
            if (method === 'PATCH' || path.includes('/retry') || path.endsWith('/credentials')) return reply({ ok: true });
            return reply({ id: id(50) });
        }
        if (broken) return reply({ code: 'UNAVAILABLE', message: 'Synthetic service unavailable.' }, 503);
        if (path.endsWith('/configuration')) return reply({ items: empty ? [] : items, jobs: empty ? [] : [{ id: id(60), kind: 'source_reconcile', status: 'failed', attempts: 12, revision: 1 }, { id: id(61), kind: 'pr_feedback', status: 'pending', attempts: 1, revision: 1 }] });
        if (path.endsWith('/stacks')) return reply({ stacks: empty ? [] : [{ id: id(40), name: 'Fixture stack', project_id: ids.project, environment_id: ids.env, commit: 'a'.repeat(40), archived: false, can_plan: true, can_native_plan: true }, { id: id(43), name: 'Archived stack', project_id: ids.project, environment_id: ids.env, commit: 'b'.repeat(40), archived: true, can_plan: false, can_native_plan: false }] });
        return reply({ preflights: empty ? [] : [{ id: id(42), run_id: id(41), stack: 'Fixture stack', commit: 'a'.repeat(40), native_planning: false, purpose: 'tracked', status: 'failed', priority: 'medium', reason: 'SOURCE_CHECKOUT_FAILED', revision: 2, can_retry: true, can_prioritize: false }, { id: id(44), run_id: id(45), stack: 'Queued stack', commit: 'b'.repeat(40), native_planning: false, purpose: 'pr_preview', status: 'queued', priority: 'medium', reason: '', revision: 1, can_retry: false, can_prioritize: true }, { id: id(46), run_id: id(47), stack: 'Passed stack', commit: 'b'.repeat(40), native_planning: false, purpose: 'tracked', status: 'succeeded', priority: 'low', reason: 'PREFLIGHT_ONLY', revision: 3, can_retry: false, can_prioritize: false }] });
    }));
});
function section(name: string) { return within(screen.getByRole('heading', { name }).closest('section')!); }
async function mount(configuration = true, manage = true, installationAdmin = true) { const view = renderPage(<Settings org={ids.org} projects={[project]} manage={manage} installationAdmin={installationAdmin} configuration={configuration}/>); await screen.findByRole('heading', { name: 'Stacks' }); return view; }
async function fill(name: string, fields: Record<string, string>, button: string) {
    const scope = section(name);
    for (const [label, value] of Object.entries(fields)) { const input = scope.getByLabelText(label); fireEvent.change(input, { target: { value } }); }
    fireEvent.click(scope.getByRole('button', { name: button }));
    await waitFor(() => expect(scope.getByRole('button', { name: button })).not.toBeDisabled());
}
describe('Source settings and preflight controls', () => {
    it('shows source-only results and submits explicit check, priority, retry and delivery actions', async () => {
        await mount(); expect(screen.getByText('Source check passed')).toBeInTheDocument(); expect(screen.getByText(/Pull request preview/)).toBeInTheDocument();
        fireEvent.click(screen.getByRole('button', { name: 'Check source for Fixture stack' })); await screen.findByText('Source check queued.');
        fireEvent.change(screen.getByLabelText('Priority for Queued stack'), { target: { value: 'high' } }); await waitFor(() => expect(requests.some(r => r.body.priority === 'high')).toBe(true));
        await waitFor(() => expect(screen.getByText('Retry preparation')).not.toBeDisabled()); fireEvent.click(screen.getByText('Retry preparation')); await screen.findByText('Source preparation queued again.');
        fireEvent.click(screen.getByText('Retry delivery')); await waitFor(() => expect(requests.some(r => r.path.includes('/source-jobs/'))).toBe(true));
        expect(requests[0].body).toEqual({ source: { kind: 'branch' }, priority: 'medium' });
        fireEvent.click(screen.getByRole('button', { name: 'Plan Fixture stack' })); await screen.findByText('Plan queued.');
        expect(requests.at(-1)?.body).toEqual({ source: { kind: 'branch' }, priority: 'medium', intent: 'native_plan' });
        expect(screen.queryByRole('button', { name: /Apply/ })).not.toBeInTheDocument();
    });
    it('creates pools, connections and repositories with explicit project scope', async () => {
        await mount(); await fill('Add a Bitbucket connection', { Name: 'Local Bitbucket' }, 'Add connection');
        await fill('Add a worker pool', { 'Pool name': 'Local pool' }, 'Add pool');
        await fill('Register a repository', { Project: ids.project, Connection: id(10), 'Bitbucket repository UUID': '{fictional}', 'HTTPS clone URL': 'https://bitbucket.org/fictional/repo.git', 'Tracked branch': 'main' }, 'Register repository');
        expect(requests.at(-1)?.body).toMatchObject({ project_id: ids.project, enabled: true, tracked_branch: 'main' });
        fireEvent.click(screen.getByText('Sync inventory')); await screen.findByText('Tracked inventory synchronized.');
    });
    it('shows newly issued worker credentials once and permits pause and replacement', async () => {
        await mount(); await fill('Enroll a worker', { 'Worker name': 'Fixture worker', Pool: id(11), 'Routing tags, separated by commas': 'aws, local' }, 'Enroll worker');
        expect((screen.getByLabelText('Worker credential') as HTMLTextAreaElement).value).toContain('fictional-once-visible-token');
        expect(requests.at(-1)?.body.tags).toEqual(['aws', 'local']); fireEvent.click(screen.getByText('Dismiss credential')); expect(screen.queryByLabelText('Worker credential')).not.toBeInTheDocument();
        fireEvent.click(screen.getByText('Pause')); await screen.findByText('Worker paused. Existing work retains its ownership.');
        fireEvent.click(screen.getByText('Replace credential')); await screen.findByText('Credential replaced; worker paused.');
        expect((screen.getByLabelText('Worker credential') as HTMLTextAreaElement).value).toContain('fictional-once-visible-token');
    });
    it('does not retain credentials after leaving the scope', async () => {
        const view = await mount(); await fill('Enroll a worker', { 'Worker name': 'Fixture worker', Pool: id(11), 'Routing tags, separated by commas': '' }, 'Enroll worker');
        view.unmount(); await mount(false, false, false); expect(screen.queryByLabelText('Worker credential')).not.toBeInTheDocument(); expect(screen.queryByRole('heading', { name: 'Configured resources' })).not.toBeInTheDocument();
    });
    it('saves secrets, role references, runtime identities and OAuth settings without retrying', async () => {
        await mount(); await fill('Add a secret', { Project: ids.project, 'Secret name': 'Fixture token', 'Secret value': 'private-fixture-value' }, 'Save secret');
        await fill('Execution role reference', { Project: ids.project, 'Role ARN': 'arn:aws:iam::111122223333:role/fixture', 'AWS account': '111122223333' }, 'Save role reference');
        await fill('Approve a worker runtime', { Engine: 'terraform', 'Exact version': '1.17.1', 'Verified image digest': 'sha256:' + 'a'.repeat(64) }, 'Approve runtime');
        await fill('Bitbucket OAuth settings', { Connection: id(10), 'Consumer key': 'fictional-client', 'Consumer secret': 'fictional-consumer-secret', 'Webhook signing secret': 'w'.repeat(32), 'Webhook UUID': 'fictional-hook' }, 'Save OAuth settings');
        expect(requests.at(-1)?.body.expected_revision).toBe(1); expect(requests.filter(r => r.path.endsWith('/credentials'))).toHaveLength(1);
        fireEvent.click(screen.getByText('Connect Bitbucket')); await screen.findByRole('alert'); expect(screen.getByRole('alert')).toHaveTextContent('Unexpected connection destination.');
    });
    it('grants repository, S3 and DynamoDB permissions using exact selected scopes', async () => {
        await mount(); const scope = section('Resource permissions');
        fireEvent.change(scope.getByLabelText('Project'), { target: { value: ids.project } }); fireEvent.change(scope.getByLabelText('Environment'), { target: { value: ids.env } }); fireEvent.change(scope.getByLabelText('Resource'), { target: { value: id(12) } }); fireEvent.click(scope.getByRole('button', { name: 'Grant resource access' })); await screen.findByText('Resource permission saved.');
        fireEvent.change(scope.getByLabelText('Resource kind'), { target: { value: 's3_object' } }); fireEvent.change(scope.getByLabelText('Bucket'), { target: { value: 'fictional-bucket' } }); fireEvent.change(scope.getByLabelText('Exact object key'), { target: { value: 'state.tfstate' } }); fireEvent.click(scope.getByRole('button', { name: 'Grant resource access' })); await waitFor(() => expect(requests.at(-1)?.body.namespace).toBe('aws/fictional-bucket'));
        await waitFor(() => expect(scope.getByRole('button', { name: 'Grant resource access' })).not.toBeDisabled()); fireEvent.change(scope.getByLabelText('Resource kind'), { target: { value: 'ddb_table' } }); fireEvent.change(scope.getByLabelText('Exact table ARN'), { target: { value: 'arn:aws:dynamodb:eu-west-1:111122223333:table/fixture' } }); fireEvent.click(scope.getByRole('button', { name: 'Grant resource access' })); await waitFor(() => expect(requests.at(-1)?.body.kind).toBe('ddb_table'));
    });
    it('surfaces errors and refuses automatic secret replay', async () => {
        await mount(); denied = true; await fill('Add a secret', { 'Secret name': 'Fixture token', 'Secret value': 'private-fixture-value' }, 'Save secret'); expect(await screen.findByRole('alert')).toHaveTextContent('Confirm a new secret write.');
        expect(requests).toHaveLength(1); denied = false; const buttons = screen.getAllByRole('button', { name: 'Disable' }); fireEvent.click(buttons[0]); await waitFor(() => expect(requests).toHaveLength(2));
    });
    it('handles empty scopes, missing runtime administration and failed background reads', async () => {
        empty = true; const view = await mount(true, true, false); expect(screen.getByText('No source checks yet.')).toBeInTheDocument(); expect(screen.getByText('Add a source connection and worker pool to begin.')).toBeInTheDocument(); expect(screen.queryByRole('heading', { name: 'Approve a worker runtime' })).not.toBeInTheDocument(); view.unmount(); broken = true; await mount(false, false, false); expect(screen.getByRole('alert')).toHaveTextContent('Synthetic service unavailable.');
    });
    it('polls without issuing activity renewal or a mutation and cancels on unmount', async () => {
        vi.useFakeTimers();
        const view = renderPage(<Settings org={ids.org} projects={[project]} manage={false} installationAdmin={false} configuration={false}/>);
        await act(async () => {});
        expect(screen.getByRole('heading', { name: 'Stacks' })).toBeInTheDocument();
        const fetch = vi.mocked(globalThis.fetch);
        expect(fetch).toHaveBeenCalledTimes(2);
        const initial = fetch.mock.calls.map(([, options]) => options!.signal!);
        await act(async () => { await vi.advanceTimersByTimeAsync(4999); });
        expect(fetch).toHaveBeenCalledTimes(2);
        const pending: AbortSignal[] = [];
        fetch.mockImplementation((_url, options) => new Promise<Response>((_resolve, reject) => {
            const signal = options!.signal!; pending.push(signal);
            signal.addEventListener('abort', () => reject(new DOMException('Fixture aborted', 'AbortError')), { once: true });
        }));
        await act(async () => { await vi.advanceTimersByTimeAsync(1); });
        expect(fetch).toHaveBeenCalledTimes(4);
        expect(fetch.mock.calls.slice(2).map(([url]) => url)).toEqual([`/api/v1/organizations/${ids.org}/stacks`, `/api/v1/organizations/${ids.org}/preflights`]);
        expect(initial.every(signal => signal.aborted)).toBe(true);
        expect(pending).toHaveLength(2);
        expect(pending.every(signal => !signal.aborted)).toBe(true);
        await act(async () => { view.unmount(); });
        expect(pending.every(signal => signal.aborted)).toBe(true);
        await act(async () => { await vi.advanceTimersByTimeAsync(15000); });
        expect(fetch).toHaveBeenCalledTimes(4);
        expect(requests).toHaveLength(0);
        expect(fetch.mock.calls.some(([url]) => String(url).includes('/session/activity'))).toBe(false);
    });
});
