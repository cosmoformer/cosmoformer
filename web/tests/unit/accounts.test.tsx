import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Route, Routes } from 'react-router';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import Account from '../../app/routes/account';
import Login from '../../app/routes/login';
import { useSession } from '../../app/lib/session';
import { deferred, renderPage, reply, session } from './helpers';

vi.mock('../../app/lib/session', () => ({ useSession: vi.fn() }));
const password = 'A fictional testing password';
const refresh = vi.fn<() => Promise<void>>();
beforeEach(() => {
    refresh.mockReset().mockResolvedValue();
    vi.mocked(useSession).mockReturnValue({ session: null, loading: false, error: '', refresh });
});
function login() {
    return renderPage(<Routes><Route path="/login" element={<Login />} /><Route path="/organizations" element={<h1>Workspace destination</h1>} /></Routes>, '/login');
}
function account(purpose = 'setup') {
    return renderPage(<Routes><Route path="/account/:purpose" element={<Account />} /><Route path="/login" element={<h1>Sign-in destination</h1>} /></Routes>, '/account/' + purpose);
}
async function fillLogin() {
    const user = userEvent.setup();
    await user.type(screen.getByLabelText('Username'), 'engineer');
    await user.type(screen.getByLabelText('Password', { exact: true }), password);
    return user;
}
async function fillAccount(purpose = 'setup', confirmation = password) {
    const user = userEvent.setup();
    await user.type(screen.getByLabelText(purpose === 'setup' ? 'Setup code' : 'Reset code'), 'a'.repeat(43));
    await user.type(screen.getByLabelText('New password'), password);
    await user.type(screen.getByLabelText('Confirm password'), confirmation);
    return user;
}

describe('sign-in form', () => {
    it('redirects an authenticated account without submitting credentials', async () => {
        vi.mocked(useSession).mockReturnValue({ session: session(), loading: false, error: '', refresh });
        login();
        expect(await screen.findByRole('heading', { name: 'Workspace destination' })).toBeVisible();
        expect(fetch).not.toHaveBeenCalled();
    });
    it('disables submission while bootstrap is loading', () => {
        vi.mocked(useSession).mockReturnValue({ session: null, loading: true, error: '', refresh });
        login(); expect(screen.getByRole('button', { name: 'Sign in' })).toBeDisabled();
    });
    it('requires credentials and keeps account setup/reset links available', async () => {
        login();
        await userEvent.setup().click(screen.getByRole('button', { name: 'Sign in' }));
        expect(fetch).not.toHaveBeenCalled();
        expect(screen.getByRole('link', { name: 'Set up your account' })).toHaveAttribute('href', '/account/setup');
        expect(screen.getByRole('link', { name: 'Use a password reset code' })).toHaveAttribute('href', '/account/reset');
    });
    it('submits once, disables the pending form, refreshes session and navigates', async () => {
        const pending = deferred<Response>(); vi.mocked(fetch).mockReturnValueOnce(pending.promise);
        login(); const user = await fillLogin();
        await user.click(screen.getByRole('button', { name: 'Sign in' }));
        expect(screen.getByRole('button', { name: 'Signing in…' })).toBeDisabled();
        expect(fetch).toHaveBeenCalledWith('/api/v1/session/login', expect.objectContaining({ method: 'POST', body: JSON.stringify({ username: 'engineer', password }) }));
        pending.resolve(reply({ ok: true }));
        expect(await screen.findByRole('heading', { name: 'Workspace destination' })).toBeVisible();
        expect(refresh).toHaveBeenCalledTimes(1); expect(fetch).toHaveBeenCalledTimes(1);
    });
    it.each([new Error('offline'), 'unknown'])('shows failures and allows an explicit retry', async error => {
        vi.mocked(fetch).mockRejectedValueOnce(error).mockResolvedValueOnce(reply({ ok: true }));
        login(); const user = await fillLogin();
        await user.click(screen.getByRole('button', { name: 'Sign in' }));
        expect(await screen.findByRole('alert')).toHaveTextContent(error instanceof Error ? 'offline' : 'Unable to sign in.');
        expect(refresh).not.toHaveBeenCalled(); expect(fetch).toHaveBeenCalledTimes(1);
        await user.click(screen.getByRole('button', { name: 'Sign in' }));
        expect(await screen.findByRole('heading', { name: 'Workspace destination' })).toBeVisible();
    });
});

describe('one-use setup/reset form', () => {
    it('does not offer a form for an unsupported purpose', () => {
        account('other'); expect(screen.getByRole('heading', { name: 'Page unavailable' })).toBeVisible();
        expect(screen.queryByRole('button')).not.toBeInTheDocument();
    });
    it('rejects mismatched passwords without sending or consuming the proof', async () => {
        account(); const user = await fillAccount('setup', 'A different testing password');
        await user.click(screen.getByRole('button', { name: 'Save password' }));
        expect(screen.getByRole('alert')).toHaveTextContent('The passwords don’t match.');
        expect(fetch).not.toHaveBeenCalled();
    });
    it.each(['setup', 'reset'])('consumes a %s proof once and requires explicit sign-in afterwards', async purpose => {
        const pending = deferred<Response>(); vi.mocked(fetch).mockReturnValueOnce(pending.promise);
        account(purpose); const user = await fillAccount(purpose);
        await user.click(screen.getByRole('button', { name: 'Save password' }));
        expect(screen.getByRole('button', { name: 'Saving…' })).toBeDisabled();
        expect(fetch).toHaveBeenCalledWith('/api/v1/account/' + purpose, expect.objectContaining({ method: 'POST', body: JSON.stringify({ code: 'a'.repeat(43), password }) }));
        pending.resolve(reply({ ok: true }));
        expect(await screen.findByRole('heading', { name: 'Password saved' })).toBeVisible();
        expect(screen.queryByLabelText('New password')).not.toBeInTheDocument();
        expect(fetch).toHaveBeenCalledTimes(1); expect(refresh).not.toHaveBeenCalled();
        await user.click(screen.getByRole('link', { name: 'Go to sign in →' }));
        expect(screen.getByRole('heading', { name: 'Sign-in destination' })).toBeVisible();
    });
    it.each([new Error('The code is invalid or expired.'), 'unknown'])('keeps the form usable after a failure without automatic replay', async error => {
        vi.mocked(fetch).mockRejectedValueOnce(error);
        account(); const user = await fillAccount();
        await user.click(screen.getByRole('button', { name: 'Save password' }));
        expect(await screen.findByRole('alert')).toHaveTextContent(error instanceof Error ? error.message : 'Unable to complete setup.');
        expect(screen.getByRole('button', { name: 'Save password' })).toBeEnabled();
        expect(fetch).toHaveBeenCalledTimes(1);
    });
});
