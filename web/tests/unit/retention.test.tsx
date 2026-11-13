import { act, fireEvent, screen, waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import Retention from '../../app/components/retention';
import { ids, renderPage, reply } from './helpers';

const settings = { cleanup: { enabled: false, interval_seconds: 86400, retention_days: 30, batch: 2 }, maintenance: false, offline: false, storage_configured: true, plan_seconds: 1200, apply_seconds: 2400, preflight_seconds: 300, plan_ttl_hours: 168 };
let mode = '', calls: { path: string; method: string; body: unknown; signal?: AbortSignal | null }[];
beforeEach(() => {
    mode = ''; calls = [];
    vi.stubGlobal('fetch', vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
        const path = String(input), method = init?.method ?? 'GET';
        calls.push({ path, method, body: init?.body ? JSON.parse(String(init.body)) : undefined, signal: init?.signal });
        if (mode === 'read-fail' && method === 'GET') return reply({ code: 'FORBIDDEN', message: 'Current administration permission is required.' }, 403);
        if (method === 'POST') {
            if (mode === 'write-fail') return reply({ code: 'UNAVAILABLE', message: 'Refresh to inspect durable outcomes.' }, 503);
            if (mode === 'write-wait') return new Promise<Response>(() => {});
            return reply({ claim_ids: [ids.project] });
        }
        if (path.endsWith('/settings')) return reply({ ...settings, ...(mode === 'paused' ? { maintenance: true, offline: true, storage_configured: false } : {}), ...(mode === 'enabled' ? { cleanup: { ...settings.cleanup, enabled: true } } : {}) });
        const cursor = new URL(path, 'https://localhost:5173').searchParams.get('cursor');
        return reply(mode === 'empty' ? { items: [], next_cursor: '' } : { items: [{ artifact_id: cursor ? ids.env : ids.project, kind: 'manifest', outcome: cursor ? 'completed' : 'pending', reason: cursor ? '' : 'STORAGE_DELETE_UNCONFIRMED' }], next_cursor: cursor ? '' : ids.project });
    }));
});
async function mount() { const view = renderPage(<Retention org={ids.org}/>); await screen.findByText('30 days'); return view; }

describe('Retention administration', () => {
    it('shows safe read-only settings and explicit outcomes, with filtered pagination', async () => {
        await mount(); expect(screen.getByText('Disabled')).toBeInTheDocument(); expect(screen.getByText('86400 seconds')).toBeInTheDocument();
        expect(screen.getByText('pending', { selector: 'td' })).toBeInTheDocument(); expect(screen.getByText('storage delete unconfirmed')).toBeInTheDocument();
        fireEvent.click(screen.getByText('Execution limits')); expect(screen.getByText(/Plan: 1200 seconds/)).toBeInTheDocument();
        fireEvent.click(screen.getByText('Next page')); await screen.findByText('completed', { selector: 'td' }); expect(calls.some(c => c.path.includes('cursor='))).toBe(true);
        fireEvent.change(screen.getByLabelText('Outcome'), { target: { value: 'failed' } }); await screen.findByText('pending', { selector: 'td' });
        expect(calls.at(-1)?.path).toContain('outcome=failed'); expect(calls.at(-1)?.path).not.toContain('cursor=');
        fireEvent.click(screen.getByText('Refresh preview')); await waitFor(() => expect(calls.filter(c => c.path.includes('cleanup-preview'))).toHaveLength(4));
        expect(calls.every(c => c.method === 'GET')).toBe(true);
    });
    it('bounds explicit actions and refreshes durable outcomes without changing settings', async () => {
        await mount(); expect(screen.getByLabelText('Batch size')).toHaveValue(2);
        fireEvent.change(screen.getByLabelText('Batch size'), { target: { value: '1' } }); fireEvent.click(screen.getByText('Run bounded cleanup'));
        await screen.findByText(/1 cleanup claims selected/);
        expect(calls.filter(c => c.method === 'POST')).toEqual([expect.objectContaining({ path: '/api/v1/admin/cleanup', body: { org_id: ids.org, limit: 1 } })]);
        expect(screen.getByText('Run bounded cleanup')).not.toBeDisabled();
    });
    it('keeps cleanup disabled during maintenance and shows empty previews honestly', async () => {
        mode = 'paused'; const view = await mount(); expect(screen.getByText('Active · cleanup paused')).toBeInTheDocument(); expect(screen.getByText('Unavailable')).toBeInTheDocument(); expect(screen.getByText('Run bounded cleanup')).toBeDisabled();
        view.unmount(); mode = 'empty'; await mount(); expect(screen.getByText('No artifacts match this preview.')).toBeInTheDocument(); expect(screen.queryByText('Next page')).not.toBeInTheDocument();
    });
    it('surfaces authorization loss and storage errors without automatic mutation replay', async () => {
        mode = 'enabled'; await mount(); expect(screen.getByText('Enabled')).toBeInTheDocument();
        mode = 'write-fail'; fireEvent.click(screen.getByText('Run bounded cleanup')); await screen.findByRole('alert'); expect(screen.getByRole('alert')).toHaveTextContent('Refresh to inspect durable outcomes.');
        expect(calls.filter(c => c.method === 'POST')).toHaveLength(1);
        mode = 'read-fail'; fireEvent.click(screen.getByText('Refresh preview')); await waitFor(() => expect(screen.getByRole('alert')).toHaveTextContent('Current administration permission is required.'));
        expect(screen.queryByText('30 days')).not.toBeInTheDocument(); expect(screen.queryByText('pending', { selector: 'td' })).not.toBeInTheDocument(); expect(screen.getByText('Run bounded cleanup')).toBeDisabled();
    });
    it('aborts reads and accepted requests when the organization view is left', async () => {
        const view = await mount(); mode = 'write-wait'; fireEvent.click(screen.getByText('Run bounded cleanup')); expect(screen.getByText('Cleaning…')).toBeDisabled();
        const pending = calls.at(-1)!.signal!; view.unmount(); expect(pending.aborted).toBe(true);
        await act(async () => {});
    });
});
