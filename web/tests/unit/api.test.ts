import { describe, expect, it, vi } from 'vitest';
import { api, APIError, setCSRF, type Schema } from '../../app/lib/api';
import { reply, session } from './helpers';

describe('API requests and the Go-owned response contract', () => {
    it('uses private same-origin GETs without mutation headers or a body', async () => {
        const value = session();
        vi.mocked(fetch).mockResolvedValue(reply(value));
        const controller = new AbortController();
        setCSRF('fixture');
        await expect(api('/session', 'SessionOutputBody', { signal: controller.signal })).resolves.toEqual(value);
        expect(fetch).toHaveBeenCalledWith('/api/v1/session', {
            method: 'GET', headers: { Accept: 'application/json' }, credentials: 'same-origin',
            cache: 'no-store', body: undefined, signal: controller.signal,
        });
    });
    it.each(['POST', 'PATCH', 'DELETE'])('protects %s mutations and gives each attempt a distinct key', async method => {
        vi.mocked(fetch).mockImplementation(async () => reply({ ok: true }));
        setCSRF('fixture-csrf');
        await api('/example', 'OKBody', { method, body: { name: 'Example' } });
        await api('/example', 'OKBody', { method });
        const first = vi.mocked(fetch).mock.calls[0][1]!;
        const second = vi.mocked(fetch).mock.calls[1][1]!;
        expect(first).toMatchObject({ method, credentials: 'same-origin', cache: 'no-store', body: '{"name":"Example"}' });
        expect(first.headers).toMatchObject({ 'Content-Type': 'application/json', 'X-Cosmoformer-Request': 'browser', 'X-CSRF-Token': 'fixture-csrf' });
        expect((first.headers as Record<string, string>)['Idempotency-Key']).toMatch(/^[a-f0-9]{32}$/);
        expect((first.headers as Record<string, string>)['Idempotency-Key']).not.toBe((second.headers as Record<string, string>)['Idempotency-Key']);
        expect(second.body).toBe('{}');
    });
    it('omits the CSRF header before sign-in and after clearing it', async () => {
        vi.mocked(fetch).mockImplementation(async () => reply({ ok: true }));
        setCSRF('old'); setCSRF(null);
        await api('/session/login', 'LoginOutputBody', { method: 'POST' });
        expect(fetch).toHaveBeenCalledTimes(1);
        expect(fetch).toHaveBeenLastCalledWith('/api/v1/session/login', expect.objectContaining({ headers: expect.not.objectContaining({ 'X-CSRF-Token': expect.anything() }) }));
    });
    it.each([{}, { ok: 'yes' }, { ok: true, extra: 'unexpected' }, null, []])('rejects a successful response with an invalid shape: %j', async value => {
        vi.mocked(fetch).mockResolvedValue(reply(value));
        await expect(api('/session/activity', 'OKBody', { method: 'POST' })).rejects.toMatchObject({ status: 503, code: 'UNAVAILABLE' });
    });
    it('validates nested session data and date/UUID formats with real generated validators', async () => {
        const value = session();
        vi.mocked(fetch).mockResolvedValue(reply({ ...value, actor: { ...value.actor, id: 'not-a-uuid' } }));
        await expect(api('/session', 'SessionOutputBody')).rejects.toBeInstanceOf(APIError);
        vi.mocked(fetch).mockResolvedValue(reply({ ...value, expires_at: 'tomorrow' }));
        await expect(api('/session', 'SessionOutputBody')).rejects.toBeInstanceOf(APIError);
    });
    it('fails safely when a validator is unavailable', async () => {
        vi.mocked(fetch).mockResolvedValue(reply({}));
        await expect(api('/example', 'MissingSchema' as keyof Schema)).rejects.toMatchObject({ status: 503 });
    });
    it.each([400, 403, 404, 409, 422, 429, 503])('preserves a known public error at HTTP %i without retrying', async status => {
        vi.mocked(fetch).mockResolvedValue(reply({ code: 'CONFLICT', message: 'Refresh and try again.' }, status));
        await expect(api('/example', 'OKBody', { method: 'POST' })).rejects.toMatchObject({ status, code: 'CONFLICT', message: 'Refresh and try again.' });
        expect(fetch).toHaveBeenCalledTimes(1);
    });
    it.each([{}, { code: 'INTERNAL_DETAIL', message: 'private database detail' }, { code: 'CONFLICT', message: 42 }, null, 'private detail'])('sanitizes malformed or unknown errors: %j', async value => {
        vi.mocked(fetch).mockResolvedValue(reply(value, 500));
        await expect(api('/example', 'OKBody')).rejects.toMatchObject({ status: 500, message: 'The request could not be completed.' });
    });
    it.each([200, 401, 502])('handles non-JSON HTTP %i responses', async status => {
        vi.mocked(fetch).mockResolvedValue(new Response('<html>private proxy error</html>', { status }));
        await expect(api('/example', 'OKBody')).rejects.toMatchObject({ status, code: 'UNAVAILABLE', message: 'The service returned an unexpected response.' });
    });
    it('expires the session and clears CSRF on 401 before parsing its response', async () => {
        const expired = vi.fn();
        window.addEventListener('cosmoformer:expired', expired);
        try {
            setCSRF('old-session-token');
            vi.mocked(fetch).mockResolvedValue(reply({ code: 'SIGN_IN_REQUIRED', message: 'Sign in.' }, 401));
            await expect(api('/example', 'OKBody')).rejects.toMatchObject({ status: 401 });
            expect(expired).toHaveBeenCalledTimes(1);
            vi.mocked(fetch).mockResolvedValue(reply({ ok: true }));
            await api('/session/login', 'LoginOutputBody', { method: 'POST' });
            expect(vi.mocked(fetch).mock.lastCall![1]!.headers).not.toHaveProperty('X-CSRF-Token');
        } finally { window.removeEventListener('cosmoformer:expired', expired); }
    });
    it.each([new TypeError('offline'), new DOMException('Aborted', 'AbortError')])('propagates network/cancellation failures without replaying a mutation', async error => {
        vi.mocked(fetch).mockRejectedValue(error);
        await expect(api('/example', 'OKBody', { method: 'POST' })).rejects.toBe(error);
        expect(fetch).toHaveBeenCalledTimes(1);
    });
});
