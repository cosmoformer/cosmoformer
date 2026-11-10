import { act, fireEvent, screen } from '@testing-library/react';
import { Route, Routes, useLocation } from 'react-router';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { LogView } from '../../app/components/log-view';
import { useRead } from '../../app/lib/polling';
import { SessionProvider } from '../../app/lib/session';
import Project from '../../app/routes/project';
import Run from '../../app/routes/run';
import { deferred, renderPage, reply } from './helpers';
import { attempt, env, org, project, projects, reviewSession, run, runFixture, runResponse, stackFixture } from '../fixtures/review';

let visibility: DocumentVisibilityState, online: boolean;
beforeEach(() => {
    visibility = 'visible'; online = true;
    vi.spyOn(document, 'visibilityState', 'get').mockImplementation(() => visibility);
    vi.spyOn(navigator, 'onLine', 'get').mockImplementation(() => online);
});
afterEach(() => vi.restoreAllMocks());
function suspend(kind: 'hidden' | 'offline') {
    if (kind === 'hidden') { visibility = 'hidden'; document.dispatchEvent(new Event('visibilitychange')); }
    else { online = false; window.dispatchEvent(new Event('offline')); }
}
function resume(kind: 'hidden' | 'offline') {
    if (kind === 'hidden') { visibility = 'visible'; document.dispatchEvent(new Event('visibilitychange')); }
    else { online = true; window.dispatchEvent(new Event('online')); }
}
const batch = { cursor: 'retained-cursor', reset: false, segments: [{ sequence: 0, byte_start: 0, byte_end: 8, text: 'retained' }], archive_complete: true, has_more: false };
const continuation = { ...batch, cursor: 'next-cursor', segments: [] };

it('retains displayed archive text and its cursor across a transient error and empty retry continuation', async () => {
    vi.useFakeTimers();
    vi.mocked(fetch).mockResolvedValueOnce(reply(batch)).mockResolvedValueOnce(reply({ code: 'UNAVAILABLE', message: 'Try again.' }, 503)).mockResolvedValueOnce(reply(continuation));
    const denied = vi.fn();
    renderPage(<LogView org={org} run={run} attempt={attempt} onDenied={denied}/>);
    await act(async () => {});
    expect(screen.getByText('retained')).toBeVisible();
    await act(async () => { vi.advanceTimersByTime(2000); });
    expect(screen.getByRole('alert')).toHaveTextContent('Try again.');
    expect(screen.getByText('retained')).toBeVisible();
    await act(async () => { fireEvent.click(screen.getByRole('button', { name: 'Retry logs' })); });
    expect(String(vi.mocked(fetch).mock.calls[2][0])).toContain('cursor=retained-cursor');
    expect(screen.getByText('retained')).toBeVisible();
    expect(screen.queryByRole('alert')).not.toBeInTheDocument();
    expect(denied).not.toHaveBeenCalled();
});

it.each([401, 403, 404])('clears retained logs when current authorization fails with %s', async status => {
    vi.useFakeTimers();
    vi.mocked(fetch).mockResolvedValueOnce(reply(batch)).mockResolvedValueOnce(reply({ code: 'FORBIDDEN', message: 'Access changed.' }, status));
    const denied = vi.fn();
    renderPage(<LogView org={org} run={run} attempt={attempt} onDenied={denied}/>);
    await act(async () => {});
    expect(screen.getByText('retained')).toBeVisible();
    await act(async () => { vi.advanceTimersByTime(2000); });
    expect(screen.queryByText('retained')).not.toBeInTheDocument();
    expect(denied).toHaveBeenCalledOnce();
});

it.each(['hidden', 'offline'] as const)('suspends log requests while %s, fences late batches, and resumes from the retained cursor', async kind => {
    vi.useFakeTimers();
    const late = deferred<Response>(), denied = vi.fn();
    vi.mocked(fetch).mockResolvedValueOnce(reply(batch)).mockReturnValueOnce(late.promise).mockResolvedValueOnce(reply(continuation));
    renderPage(<LogView org={org} run={run} attempt={attempt} onDenied={denied}/>);
    await act(async () => {});
    await act(async () => { vi.advanceTimersByTime(2000); });
    const signal = vi.mocked(fetch).mock.calls[1][1]?.signal;
    act(() => suspend(kind));
    expect(signal?.aborted).toBe(true);
    await act(async () => { late.resolve(reply({ ...batch, cursor: 'stale-cursor', segments: [{ sequence: 1, byte_start: 8, byte_end: 13, text: 'stale' }] })); });
    expect(screen.queryByText('stale')).not.toBeInTheDocument();
    expect(screen.getByText('retained')).toBeVisible();
    await act(async () => { vi.advanceTimersByTime(10000); });
    expect(fetch).toHaveBeenCalledTimes(2);
    await act(async () => resume(kind));
    expect(fetch).toHaveBeenCalledTimes(3);
    expect(String(vi.mocked(fetch).mock.calls[2][0])).toContain('cursor=retained-cursor');
    expect(screen.getByText('retained')).toBeVisible();
    expect(denied).not.toHaveBeenCalled();
});

function ReadProbe() {
    const read = useRead('/probe', 'StackPublic', 1000);
    return <p>{read.data?.name ?? 'Loading'}</p>;
}
it.each(['hidden', 'offline'] as const)('suspends scoped reads while %s and revalidates on resume without accepting a late response', async kind => {
    vi.useFakeTimers();
    const late = deferred<Response>();
    vi.mocked(fetch).mockResolvedValueOnce(reply(stackFixture)).mockReturnValueOnce(late.promise).mockResolvedValueOnce(reply({ ...stackFixture, name: 'Fresh scope' }));
    renderPage(<ReadProbe/>);
    await act(async () => {});
    expect(screen.getByText(stackFixture.name)).toBeVisible();
    await act(async () => { vi.advanceTimersByTime(1000); });
    const signal = vi.mocked(fetch).mock.calls[1][1]?.signal;
    act(() => suspend(kind));
    expect(signal?.aborted).toBe(true);
    await act(async () => { late.resolve(reply({ ...stackFixture, name: 'Stale scope' })); vi.advanceTimersByTime(10000); });
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(screen.queryByText('Stale scope')).not.toBeInTheDocument();
    expect(screen.getByText(stackFixture.name)).toBeVisible();
    await act(async () => resume(kind));
    expect(fetch).toHaveBeenCalledTimes(3);
    expect(screen.getByText('Fresh scope')).toBeVisible();
});

it('waits for both visibility and connectivity before issuing initial reads', async () => {
    visibility = 'hidden'; online = false;
    vi.mocked(fetch).mockImplementation(async input => reply(String(input).includes('/logs?') ? batch : stackFixture));
    const denied = vi.fn();
    renderPage(<><ReadProbe/><LogView org={org} run={run} attempt={attempt} onDenied={denied}/></>);
    await act(async () => {});
    expect(fetch).not.toHaveBeenCalled();
    await act(async () => resume('hidden'));
    expect(fetch).not.toHaveBeenCalled();
    await act(async () => resume('offline'));
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(screen.getByText(stackFixture.name)).toBeVisible();
    expect(screen.getByText('retained')).toBeVisible();
});

it('fences an in-flight read across a hide/show transition in the same event turn', async () => {
    const late = deferred<Response>();
    vi.mocked(fetch).mockReturnValueOnce(late.promise).mockResolvedValueOnce(reply({ ...stackFixture, name: 'Fresh scope' }));
    renderPage(<ReadProbe/>);
    const signal = vi.mocked(fetch).mock.calls[0][1]?.signal;
    await act(async () => {
        suspend('hidden'); resume('hidden');
        expect(signal?.aborted).toBe(true);
        late.resolve(reply({ ...stackFixture, name: 'Stale scope' }));
    });
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(screen.queryByText('Stale scope')).not.toBeInTheDocument();
    expect(screen.getByText('Fresh scope')).toBeVisible();
});

function LocationProbe() { const location = useLocation(); return <output data-testid="location">{location.pathname + location.search}</output>; }
const base = `/o/${org}/p/${project}`;
function mountWorkspace(path: string) {
    return renderPage(<SessionProvider><LocationProbe/><Routes><Route path="/o/:org/p/:project" element={<Project/>}/><Route path="/o/:org/p/:project/runs/:run" element={<Run/>}/></Routes></SessionProvider>, path);
}
it.each(['sidebar', 'project context', 'run breadcrumb'])('preserves environment through the %s project link and a refreshed project route', async link => {
    vi.mocked(fetch).mockImplementation(async input => {
        const path = String(input);
        if (path.endsWith('/session')) return reply(reviewSession);
        if (path.endsWith('/projects')) return reply(projects);
        if (path.includes('/stacks?')) return reply({ stacks: [stackFixture] });
        return reply(runResponse);
    });
    const mounted = mountWorkspace(`${base}/runs/${run}?env=${env}`);
    await screen.findByLabelText('Exact run identity');
    const target = link === 'sidebar' ? screen.getByRole('link', { name: 'Project workspace' }) : screen.getAllByRole('link', { name: runFixture.project_name })[link === 'project context' ? 0 : 1];
    expect(target).toHaveAttribute('href', `${base}?env=${env}`);
    fireEvent.click(target);
    await screen.findByRole('heading', { name: runFixture.project_name });
    expect(screen.getByLabelText('Environment filter')).toHaveValue(env);
    const refreshPath = screen.getByTestId('location').textContent!;
    mounted.unmount();
    mountWorkspace(refreshPath);
    await screen.findByRole('heading', { name: runFixture.project_name });
    expect(screen.getByLabelText('Environment filter')).toHaveValue(env);
    expect(vi.mocked(fetch).mock.calls.some(([path]) => String(path).includes(`/stacks?project_id=${project}&environment_id=${env}`))).toBe(true);
    expect(vi.mocked(fetch).mock.calls.every(([, options]) => options?.method === 'GET')).toBe(true);
});
