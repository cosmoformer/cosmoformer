import { act, fireEvent, renderHook, waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import { api } from '../../app/lib/api';
import { SessionProvider, useSession } from '../../app/lib/session';
import { deferred, reply, session } from './helpers';

const mount = () => renderHook(() => useSession(), { wrapper: SessionProvider });

describe('session bootstrap and expiry', () => {
    it('starts loading, then exposes the validated account and keeps CSRF in memory', async () => {
        const pending = deferred<Response>();
        vi.mocked(fetch).mockReturnValueOnce(pending.promise);
        const { result } = mount();
        expect(result.current.loading).toBe(true);
        const value = session();
        await act(async () => { pending.resolve(reply(value)); });
        expect(result.current).toMatchObject({ loading: false, session: value, error: '' });
        vi.mocked(fetch).mockResolvedValue(reply({ ok: true }));
        await api('/session/activity', 'OKBody', { method: 'POST' });
        expect(vi.mocked(fetch).mock.lastCall![1]!.headers).toHaveProperty('X-CSRF-Token', value.csrf_token);
        expect(localStorage.length).toBe(0);
        expect(sessionStorage.length).toBe(0);
    });
    it.each([
        [reply({ code: 'SIGN_IN_REQUIRED', message: 'Sign in.' }, 401), ''],
        [reply({ code: 'UNAVAILABLE', message: 'Temporarily unavailable.' }, 503), 'Temporarily unavailable.'],
    ])('distinguishes signed-out state from service errors', async (response, error) => {
        vi.mocked(fetch).mockResolvedValue(response);
        const { result } = mount();
        await waitFor(() => expect(result.current.loading).toBe(false));
        expect(result.current.session).toBeNull();
        expect(result.current.error).toBe(error);
    });
    it.each([new Error('offline'), 'unknown failure'])('reports connection failures and recovers on explicit refresh', async error => {
        vi.mocked(fetch).mockRejectedValueOnce(error);
        const { result } = mount();
        await waitFor(() => expect(result.current.loading).toBe(false));
        expect(result.current.error).toBe(error instanceof Error ? error.message : 'Unable to connect.');
        const value = session();
        vi.mocked(fetch).mockResolvedValueOnce(reply(value));
        await act(async () => { await result.current.refresh(); });
        expect(result.current).toMatchObject({ session: value, error: '' });
    });
    it('clears protected state and CSRF when another request reports expiry', async () => {
        vi.mocked(fetch).mockResolvedValueOnce(reply(session()));
        const { result } = mount();
        await waitFor(() => expect(result.current.session).not.toBeNull());
        act(() => { window.dispatchEvent(new Event('cosmoformer:expired')); });
        expect(result.current.session).toBeNull();
        vi.mocked(fetch).mockResolvedValue(reply({ ok: true }));
        await api('/session/login', 'LoginOutputBody', { method: 'POST' });
        expect(vi.mocked(fetch).mock.lastCall![1]!.headers).not.toHaveProperty('X-CSRF-Token');
    });
    it('clears an earlier connection error when reauthentication becomes necessary', async () => {
        vi.mocked(fetch).mockRejectedValueOnce(new Error('offline'));
        const { result } = mount();
        await waitFor(() => expect(result.current.error).toBe('offline'));
        vi.mocked(fetch).mockResolvedValueOnce(reply({ code: 'SIGN_IN_REQUIRED', message: 'Sign in.' }, 401));
        await act(async () => { await result.current.refresh(); });
        expect(result.current.session).toBeNull();
        expect(result.current.error).toBe('');
        expect(result.current.loading).toBe(false);
    });
    it('does not restore a session from a bootstrap response arriving after expiry', async () => {
        const pending = deferred<Response>();
        vi.mocked(fetch).mockReturnValueOnce(pending.promise);
        const { result } = mount();
        act(() => { window.dispatchEvent(new Event('cosmoformer:expired')); });
        await act(async () => { pending.resolve(reply(session())); });
        expect(result.current.session).toBeNull();
        expect(result.current.loading).toBe(false);
    });
    it('ignores an old refresh failure after a newer refresh succeeds', async () => {
        vi.mocked(fetch).mockResolvedValueOnce(reply(session()));
        const { result } = mount();
        await waitFor(() => expect(result.current.loading).toBe(false));
        const older = deferred<Response>();
        vi.mocked(fetch).mockReturnValueOnce(older.promise).mockResolvedValueOnce(reply(session()));
        let first!: Promise<void>;
        act(() => { first = result.current.refresh(); });
        await act(async () => { await result.current.refresh(); });
        await act(async () => { older.reject(new Error('stale connection failure')); await first; });
        expect(result.current.session).not.toBeNull();
        expect(result.current.error).toBe('');
    });
    it('keeps the most recent refresh when responses arrive out of order', async () => {
        vi.mocked(fetch).mockResolvedValueOnce(reply(session()));
        const { result } = mount();
        await waitFor(() => expect(result.current.loading).toBe(false));
        const older = deferred<Response>();
        const newer = deferred<Response>();
        vi.mocked(fetch).mockReturnValueOnce(older.promise).mockReturnValueOnce(newer.promise);
        let first!: Promise<void>, second!: Promise<void>;
        act(() => { first = result.current.refresh(); second = result.current.refresh(); });
        const current = session({ csrf_token: 'newer-session' });
        await act(async () => { newer.resolve(reply(current)); await second; });
        await act(async () => { older.resolve(reply(session({ csrf_token: 'stale-session' }))); await first; });
        expect(result.current.session?.csrf_token).toBe('newer-session');
    });
});

describe('session activity and timer cleanup', () => {
    beforeEach(() => { vi.useFakeTimers(); vi.setSystemTime(new Date('2026-11-06T12:00:00Z')); });
    it('expires at the server deadline without background polling', async () => {
        vi.mocked(fetch).mockResolvedValueOnce(reply(session({ expires_at: new Date(Date.now() + 1000).toISOString() })));
        const { result } = mount();
        await act(async () => {});
        expect(result.current.session).not.toBeNull();
        await act(async () => { await vi.advanceTimersByTimeAsync(999); });
        expect(result.current.session).not.toBeNull();
        await act(async () => { await vi.advanceTimersByTimeAsync(1); });
        expect(result.current.session).toBeNull();
        expect(fetch).toHaveBeenCalledTimes(1);
    });
    it('immediately clears an already expired session', async () => {
        vi.mocked(fetch).mockResolvedValueOnce(reply(session({ expires_at: new Date(Date.now() - 1).toISOString() })));
        const { result } = mount();
        await act(async () => {});
        await act(async () => { await vi.advanceTimersByTimeAsync(0); });
        expect(result.current.session).toBeNull();
    });
    it('ignores synthetic activity rather than extending idle time', async () => {
        vi.mocked(fetch).mockResolvedValueOnce(reply(session()));
        mount(); await act(async () => {});
        fireEvent.pointerDown(window); fireEvent.keyDown(window, { key: 'a' });
        expect(fetch).toHaveBeenCalledTimes(1);
    });
    it('bounds trusted activity reports to one per minute and refreshes the deadline', async () => {
        const listeners = vi.spyOn(window, 'addEventListener');
        vi.mocked(fetch).mockImplementation(async (url) => reply(String(url).endsWith('/activity') ? { ok: true } : session()));
        mount(); await act(async () => {});
        const activity = listeners.mock.calls.find(([name]) => name === 'pointerdown')![1] as EventListener;
        // Test the listener boundary directly; jsdom cannot produce trusted native input.
        await act(async () => { activity({ isTrusted: true } as Event); });
        expect(vi.mocked(fetch).mock.calls.filter(([url]) => String(url).endsWith('/activity'))).toHaveLength(1);
        await act(async () => { activity({ isTrusted: true } as Event); });
        expect(fetch).toHaveBeenCalledTimes(3);
        await act(async () => { await vi.advanceTimersByTimeAsync(60000); activity({ isTrusted: true } as Event); });
        expect(vi.mocked(fetch).mock.calls.filter(([url]) => String(url).endsWith('/activity'))).toHaveLength(2);
    });
    it('does not retry a failed activity mutation', async () => {
        const listeners = vi.spyOn(window, 'addEventListener');
        vi.mocked(fetch).mockResolvedValueOnce(reply(session())).mockRejectedValueOnce(new Error('offline'));
        mount(); await act(async () => {});
        const activity = listeners.mock.calls.find(([name]) => name === 'keydown')![1] as EventListener;
        await act(async () => { activity({ isTrusted: true } as Event); });
        expect(fetch).toHaveBeenCalledTimes(2);
    });
    it('does not bootstrap again when an activity response arrives after expiry', async () => {
        const listeners = vi.spyOn(window, 'addEventListener');
        const pending = deferred<Response>();
        vi.mocked(fetch).mockResolvedValueOnce(reply(session())).mockReturnValueOnce(pending.promise);
        const { result } = mount(); await act(async () => {});
        const activity = listeners.mock.calls.find(([name]) => name === 'pointerdown')![1] as EventListener;
        act(() => { activity({ isTrusted: true } as Event); });
        act(() => { window.dispatchEvent(new Event('cosmoformer:expired')); });
        await act(async () => { pending.resolve(reply({ ok: true })); });
        expect(result.current.session).toBeNull();
        expect(fetch).toHaveBeenCalledTimes(2);
    });
    it('removes listeners and expiry timers on unmount', async () => {
        const remove = vi.spyOn(window, 'removeEventListener');
        vi.mocked(fetch).mockResolvedValueOnce(reply(session()));
        const { unmount } = mount(); await act(async () => {});
        unmount();
        expect(vi.getTimerCount()).toBe(0);
        for (const event of ['pointerdown', 'keydown', 'cosmoformer:expired']) expect(remove).toHaveBeenCalledWith(event, expect.any(Function));
    });
});
