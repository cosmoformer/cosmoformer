import { approvalSummary } from '../fixtures/review';
import { act, fireEvent, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Route, Routes } from 'react-router';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import Project from '../../app/routes/project';
import Stack from '../../app/routes/stack';
import Run from '../../app/routes/run';
import { ChangeList, displayValue, riskOrder } from '../../app/components/change-list';
import { LogView, appendWindow, LOG_SEGMENTS, LOG_WINDOW } from '../../app/components/log-view';
import { RunRows, planLabel, reason } from '../../app/components/run-context';
import { SessionProvider } from '../../app/lib/session';
import { AppearanceProvider } from '../../app/lib/appearance';
import { ReadState } from '../../app/components/workspace';
import { deferred, renderPage, reply } from './helpers';
import { changes, changesResponse, reviewSession, projects, stackFixture, runFixture, runResponse, org, project, env, stack, run, attempt, resource, plan, reviewID } from '../fixtures/review';
const base = `/o/${org}/p/${project}`;
let mutations: Record<string, unknown>[], stacks = [stackFixture], runs = [runFixture], response = runResponse, change = changesResponse, projectData = projects;
let fail = '', status = 503, next = '', logBatch = { cursor: 'cursor', reset: false, segments: [{ sequence: 0, byte_start: 0, byte_end: 5, text: 'hello' }], archive_complete: true, has_more: false } as any;
beforeEach(() => {
    mutations = []; stacks = [stackFixture]; runs = [runFixture]; response = structuredClone(runResponse); change = structuredClone(changesResponse); projectData = projects; fail = ''; status = 503; next = '';
    logBatch = { cursor: 'cursor', reset: false, segments: [{ sequence: 0, byte_start: 0, byte_end: 5, text: 'hello' }], archive_complete: true, has_more: false };
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, options?: RequestInit) => {
        const path = String(input);
        if (options?.method !== 'GET') { mutations.push(JSON.parse(String(options?.body))); if (fail === 'mutation') return reply({ code: 'CONFLICT', message: 'Fresh action required.' }, status); return reply({ request_id: 'request', subject_id: run, revision: 1, status: 'queued', operation_id: reviewID(9) }); }
        if (fail && path.includes(fail)) return reply({ code: 'FORBIDDEN', message: 'Current access unavailable.' }, status);
        if (path.endsWith('/dependencies')) return reply({ stack_id: stack, stack_revision_id: reviewID(70), dependencies: [] });
        if (path.endsWith('/approvals')) return reply(approvalSummary);
        if (path.endsWith('/session')) return reply(reviewSession);
        if (path.endsWith('/projects')) return reply(projectData);
        if (path.includes('/stacks?')) return reply({ stacks, ...(next ? { next_cursor: next } : {}) });
        if (path.includes('/runs?')) return reply({ runs, ...(next ? { next_cursor: next } : {}) });
        if (path.includes('/changes?')) return reply(change);
        if (path.includes('/logs?')) return reply(logBatch);
        return reply(response);
    }));
    Object.defineProperty(HTMLDialogElement.prototype, 'showModal', { configurable: true, value: function (this: HTMLDialogElement) { this.setAttribute('open', ''); } });
    Object.defineProperty(HTMLDialogElement.prototype, 'close', { configurable: true, value: function (this: HTMLDialogElement) { this.removeAttribute('open'); } });
});
afterEach(() => vi.restoreAllMocks());
function mount(path = base) { return renderPage(<AppearanceProvider><SessionProvider><Routes><Route path="/o/:org/p/:project" element={<Project/>}/><Route path="/o/:org/p/:project/stacks/:stack" element={<Stack/>}/><Route path="/o/:org/p/:project/runs/:run" element={<Run/>}/><Route path="/login" element={<h1>Sign in again</h1>}/><Route path="/organizations" element={<h1>Organizations home</h1>}/></Routes></SessionProvider></AppearanceProvider>, path); }
it('opens a project, filters environments, changes theme, navigates stack context and exact returned run', async () => {
    mount(); const user = userEvent.setup();
    await user.selectOptions(await screen.findByLabelText('Environment filter'), env);
    await user.click(await screen.findByRole('link', { name: 'Network edge' }));
    expect(await screen.findByRole('heading', { name: 'Current source' })).toBeVisible();
    expect(screen.getByText('b'.repeat(40))).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Dark theme' }));
    expect(screen.getByRole('button', { name: 'Light theme' })).toBeVisible();
    await user.click(screen.getByRole('button', { name: 'Light theme' }));
    await user.click(screen.getByRole('button', { name: 'Request plan' }));
    expect(screen.getByRole('dialog')).toBeVisible();
    await user.selectOptions(screen.getByLabelText('Source', { exact: true }), 'pr');
    await user.type(screen.getByLabelText('PR number'), '42');
    await user.selectOptions(screen.getByLabelText('Priority'), 'high');
    await user.click(screen.getByRole('button', { name: 'Confirm request' }));
    expect(await screen.findByText('Pull request #42 · exact run source')).toBeVisible();
    expect(mutations).toEqual([{ intent: 'native_plan', source: { kind: 'pr', pr_number: 42 }, priority: 'high' }]);
    expect(screen.getByText('a'.repeat(40))).toBeInTheDocument();
    expect(screen.queryByText('b'.repeat(40))).not.toBeInTheDocument();
});
it('keeps source checks explicit, handles request errors/cancel, and signs out without replay', async () => {
    mount(`${base}/stacks/${stack}`); const user = userEvent.setup();
    await user.click(await screen.findByRole('button', { name: 'Check source' }));
    fireEvent(screen.getByRole('dialog'), new Event('cancel', { cancelable: true }));
    await user.click(screen.getByRole('button', { name: 'Check source' }));
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    await user.click(screen.getByRole('button', { name: 'Check source' }));
    fail = 'mutation'; status = 409;
    await user.click(screen.getByRole('button', { name: 'Confirm request' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Fresh action required.');
    expect(mutations[0]).toEqual({ intent: 'source_check', source: { kind: 'branch' }, priority: 'medium' });
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    await user.click(screen.getByRole('button', { name: 'Sign out' }));
    expect(await screen.findByRole('heading', { name: 'Sign in again' })).toBeVisible();
});
it('reviews risks, all lossless value states, safe siblings, outputs and separate drift as text', async () => {
    mount(`${base}/runs/${run}?view=changes&env=${env}`); const user = userEvent.setup();
    expect(await screen.findByRole('heading', { name: 'Coverage & limitations' })).toBeVisible();
    expect(screen.getByText(/Partial coverage/)).toBeVisible();
    await user.click(screen.getAllByText('aws_route.edge', { selector: 'strong' })[0]);
    expect(screen.getAllByText('9007199254740993')[0]).toBeVisible();
    expect(screen.getAllByText('[absent]')[0]).toBeVisible();
    expect(screen.getAllByText('null')[0]).toBeVisible();
    expect(screen.getAllByText('"safe sibling"')[0]).toBeVisible();
    expect(document.querySelector('img[src="x"]')).toBeNull();
    await user.type(screen.getByLabelText('Search Changes'), 'hidden');
    expect(screen.queryByText('aws_route.edge', { selector: 'strong' })).not.toBeInTheDocument();
    await user.clear(screen.getByLabelText('Search Changes'));
    await user.selectOptions(screen.getByLabelText('Action', { exact: true }), 'delete');
    expect(screen.queryByText(/Hidden resource/, { selector: 'strong' })).not.toBeInTheDocument();
});
it.each(['expired', 'invalidated'])('allows retained %s review while showing ineligibility', async status => {
    response.run = { ...runFixture, plan: { ...plan, status, reason: 'SOURCE_CHANGED' } };
    mount(`${base}/runs/${run}?view=changes`);
    expect(await screen.findByRole('heading', { name: 'Coverage & limitations' })).toBeVisible();
    expect(screen.getByText(/Historical review only/)).toHaveTextContent('ineligible');
});
it('shows unavailable evidence and rejects a stale identity without a fallback', async () => {
    fail = '/changes?'; status = 409;
    const mounted = mount(`${base}/runs/${run}?view=changes`);
    expect(await screen.findByRole('alert')).toBeVisible();
    expect(screen.getByText(/Exact retained Changes are unavailable/)).toBeVisible();
    fail = ''; change = { ...change, manifest_digest: 'c'.repeat(64) };
    fireEvent.click(screen.getByRole('button', { name: 'Try again' }));
    expect(await screen.findByText(/Review identity changed/)).toBeVisible();
    mounted.unmount();
});
it('shows source-only, pending and empty attempts without fabricated plan evidence', async () => {
    response = { ...runResponse, run: { ...runFixture, plan: undefined, native_planning: false }, attempts: [] };
    const a = mount(`${base}/runs/${run}?view=changes`);
    expect(await screen.findByText(/This run is a source check/)).toBeVisible();a.unmount();
    response.run.native_planning = true;
    const b = mount(`${base}/runs/${run}?view=changes`);
    expect(await screen.findByText(/no finalized review evidence/)).toBeVisible();b.unmount();
    const c = mount(`${base}/runs/${run}?view=logs`);
    expect(await screen.findByText(/No authorized attempt selected/)).toBeVisible();c.unmount();
    mount(`${base}/runs/${run}`);
    expect(await screen.findByText('Waiting for a worker claim.')).toBeVisible();
});
it('shows exact attempts, raw downloads only with separate capability, and selected-attempt logs', async () => {
    response = { ...runResponse, can_read_raw: true, raw_artifacts: [{ id: reviewID(30), kind: 'raw_plan_json', status: 'finalized' }, { id: reviewID(31), kind: 'manifest', status: 'unavailable' }] };
    mount(`${base}/runs/${run}`);const user = userEvent.setup();
    expect(await screen.findByRole('link', { name: 'Download raw plan json' })).toHaveAttribute('href', `/api/v1/organizations/${org}/artifacts/${reviewID(30)}/content`);
    expect(screen.queryByRole('link', { name: 'Download manifest' })).not.toBeInTheDocument();
    await user.click(screen.getByRole('link', { name: 'View logs' }));
    expect(await screen.findByText('hello')).toBeVisible();
    fireEvent.change(screen.getByLabelText('Attempt'), { target: { value: attempt } });
});
it('clears protected data immediately on expiry and ignores late responses', async () => {
    const late = deferred<Response>();
    vi.mocked(fetch).mockImplementation(async (input, options) => String(input).includes('/changes?') ? late.promise : reply(String(input).endsWith('/session') ? reviewSession : String(input).endsWith('/projects') ? projects : runResponse));
    mount(`${base}/runs/${run}?view=changes`);
    await screen.findByLabelText('Exact run identity');
    act(() => window.dispatchEvent(new Event('cosmoformer:expired')));
    await screen.findByRole('heading', { name: 'Sign in again' });
    await act(async () => late.resolve(reply(changesResponse)));
    expect(screen.queryByText('Change summary')).not.toBeInTheDocument();
    expect(screen.queryByText(run)).not.toBeInTheDocument();
});
it('handles project, environment, stack, run scope, access and transport failures', async () => {
    projectData = { can_manage: false, projects: [] };let m = mount();expect(await screen.findByText('No project access')).toBeVisible();m.unmount();projectData = projects;
    m=mount(`${base}?env=${reviewID(99)}`);expect(await screen.findByText('Environment unavailable')).toBeVisible();m.unmount();
    stacks=[];m=mount(`${base}/stacks/${stack}`);expect(await screen.findByText('Stack unavailable')).toBeVisible();m.unmount();
    response.run={...runFixture, project_id:reviewID(99)};m=mount(`${base}/runs/${run}`);expect(await screen.findByText('Run outside this context')).toBeVisible();m.unmount();
    fail='/projects';m=mount();expect(await screen.findByRole('alert')).toBeVisible();m.unmount();
    fail='';m=mount(base.replace(org,reviewID(99)));expect(await screen.findByText('Workspace unavailable')).toBeVisible();m.unmount();
});
it('paginates visible stacks/runs and handles empty administrative stack access', async () => {
    next=reviewID(50);const user=userEvent.setup();let m=mount();await user.click(await screen.findByRole('button',{name:'Next stacks'}));expect(vi.mocked(fetch).mock.calls.some(([p])=>String(p).includes('before='))).toBe(true);
    await user.click(screen.getByRole('link',{name:'Runs'}));await user.click(await screen.findByRole('button',{name:'Older runs'}));m.unmount();
    stacks=[];m=mount();expect(await screen.findByText('No visible stacks')).toBeVisible();m.unmount();
    runs=[];m=mount(`${base}?view=runs`);expect(await screen.findByText('No visible runs')).toBeVisible();m.unmount();
    stacks=[{...stackFixture,can_review:false,can_plan:false,can_native_plan:false,archived:true,assessment:'not_assessed',check_reason:'Archived',plan_reason:'Archived'}];mount(`${base}/stacks/${stack}`);
    expect(await screen.findByText(/Viewer grant is required/)).toBeVisible();expect(screen.getByRole('button',{name:'Request plan'})).toBeDisabled();
});
it('renders no-change/output-only and generic/unknown coverage notices and deterministic risk order', () => {
    const no={...changes,resources:[],outputs:[],drift:[],findings:[],coverage:[],summary:{actions:{},findings:{}}};const a=renderPage(<ChangeList changes={no}/>);expect(screen.getByText('No planned resource or output changes.')).toBeVisible();a.unmount();
    renderPage(<ChangeList changes={{...changes,coverage:[{...changes.coverage[0],status:'generic_only'},{...changes.coverage[0],provider_identity:'another',status:'valid_with_unknown_hidden'},{...changes.coverage[0],provider_identity:'third',status:'supported'}]}}/>);
    expect(screen.getByText(/Generic actions only/)).toBeVisible();expect(screen.getByText(/Coverage includes unknown/)).toBeVisible();
    expect(displayValue(changes.outputs[0].paths[0].before)).toBe('{"enabled": false, "zones": [3]}');expect(displayValue({state:'known',type:'string',value:''})).toBe('""');expect(displayValue({state:'known',type:'boolean',value:true})).toBe('true');
    expect(riskOrder(resource,{...resource,findings:[]})).toBeLessThan(0);expect(riskOrder({...resource,findings:[],actions:['delete']},{...resource,findings:[],actions:['update']})).toBeLessThan(0);
    expect(reason('UNKNOWN_REASON')).toContain('unknown reason');expect(planLabel()).toBe('No native plan');expect(planLabel({...plan,status:'assembling'})).toBe('Collecting plan evidence');
});
it('bounds browser log memory independently of durable evidence', () => {
    const s={sequence:0,byte_start:0,byte_end:1,text:'x'};expect(appendWindow(Array(LOG_SEGMENTS).fill(s),[s]).segments).toHaveLength(LOG_SEGMENTS);
    expect(appendWindow([],[{...s,text:'x'.repeat(LOG_WINDOW+1)}])).toEqual({segments:[],removed:true});
});
it('follows, pauses, resumes cursor polling, resets preview and distinguishes known/unknown gaps', async () => {
    vi.useFakeTimers();const denied=vi.fn();const m=renderPage(<LogView org={org} run={run} attempt={attempt} onDenied={denied}/>);await act(async()=>{});
    expect(screen.getByText('hello')).toBeVisible();fireEvent.click(screen.getByRole('button',{name:'Pause'}));await act(async()=>{vi.advanceTimersByTime(10000)});expect(vi.mocked(fetch)).toHaveBeenCalledTimes(1);
    logBatch={...logBatch,reset:true,reset_reason:'epoch_changed',segments:[{sequence:1,byte_start:5,byte_end:9,gap:{reason:'archive_unavailable',unknown_extent:false}},{sequence:2,byte_start:9,byte_end:9,gap:{reason:'worker_final_unknown',unknown_extent:true}}],archive_complete:false,has_more:true};
    fireEvent.click(screen.getByRole('button',{name:'Follow'}));await act(async()=>{});expect(screen.queryByText('hello')).not.toBeInTheDocument();expect(screen.getByText(/5–9 redacted bytes/)).toBeVisible();expect(screen.getByText(/Gap: worker final unknown/)).toHaveTextContent('unknown extent');
    expect(String(vi.mocked(fetch).mock.calls[1][0])).toContain('cursor=cursor');m.unmount();
});
it('clears logs on revocation, fences in-flight pause responses, and retries transient failures', async () => {
    const late=deferred<Response>();vi.mocked(fetch).mockReturnValueOnce(late.promise);const denied=vi.fn();renderPage(<LogView org={org} run={run} attempt={attempt} onDenied={denied}/>);
    fireEvent.click(screen.getByRole('button',{name:'Pause'}));await act(async()=>late.resolve(reply(logBatch)));expect(screen.queryByText('hello')).not.toBeInTheDocument();
    fail='/logs?';status=503;fireEvent.click(screen.getByRole('button',{name:'Follow'}));await screen.findByRole('alert');expect(denied).not.toHaveBeenCalled();
    status=403;fireEvent.click(screen.getByRole('button',{name:'Retry logs'}));await waitFor(()=>expect(denied).toHaveBeenCalledOnce());
});

it('clears the entire review when Changes permission is revoked', async () => {
    fail='/changes?';status=403;mount(`${base}/runs/${run}?view=changes`);
    expect(await screen.findByText('Review access changed')).toBeVisible();
    expect(screen.queryByLabelText('Exact run identity')).not.toBeInTheDocument();
    expect(screen.queryByText('Change summary')).not.toBeInTheDocument();
});
it('does not repopulate protected Changes after navigating to another scope', async () => {
    const late=deferred<Response>();
    const original=vi.mocked(fetch).getMockImplementation()!;
    vi.mocked(fetch).mockImplementation((input,options)=>String(input).includes('/changes?')?late.promise:original(input,options));
    mount(`${base}/runs/${run}?view=changes`);
    await screen.findByLabelText('Exact run identity');
    fireEvent.click(screen.getAllByRole('link',{name:'Orbital platform'})[0]);
    await screen.findByRole('heading',{name:'Orbital platform'});
    await act(async()=>late.resolve(reply(changesResponse)));
    expect(screen.queryByText('Change summary')).not.toBeInTheDocument();
    expect(screen.queryByLabelText('Exact run identity')).not.toBeInTheDocument();
});
it('labels a log display window limit separately and allows an explicit archive restart', async () => {
    logBatch={...logBatch,segments:Array(501).fill({sequence:0,byte_start:0,byte_end:1,text:'x'})};
    renderPage(<LogView org={org} run={run} attempt={attempt} onDenied={()=>{}}/>);
    expect(await screen.findByText(/Earlier text was removed from browser memory/)).toBeVisible();
    logBatch={...logBatch,segments:[]};fireEvent.click(screen.getByRole('button',{name:'Read archive from start'}));
    await waitFor(()=>expect(vi.mocked(fetch)).toHaveBeenCalledTimes(2));
    expect(String(vi.mocked(fetch).mock.calls[1][0])).not.toContain('cursor=');
});

it('pages historical attempts and raw metadata explicitly in run context', async () => {
    response={...runResponse,attempts_next:reviewID(50),raw_artifacts_next:reviewID(51),can_read_raw:true,raw_artifacts:[]};
    const user=userEvent.setup();mount(`${base}/runs/${run}`);
    await user.click(await screen.findByRole('button',{name:'Older attempts'}));
    expect(await screen.findByRole('link',{name:'View logs'})).toHaveAttribute('href',expect.stringContaining('attempt_before='));
    await user.click(await screen.findByRole('button',{name:'Newest attempts'}));
    await user.click(await screen.findByRole('button',{name:'Older raw artifacts'}));
    await user.click(await screen.findByRole('button',{name:'Newest raw artifacts'}));
    expect(vi.mocked(fetch).mock.calls.some(([p])=>String(p).includes('attempt_before='))).toBe(true);
    expect(vi.mocked(fetch).mock.calls.some(([p])=>String(p).includes('artifact_before='))).toBe(true);
});
