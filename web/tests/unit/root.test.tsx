import { screen } from '@testing-library/react';
import { Route, Routes, useRouteError } from 'react-router';
import { expect, it, vi } from 'vitest';
import App, { ErrorBoundary } from '../../app/root';
import { renderPage, reply, session } from './helpers';

vi.mock('react-router', async importOriginal => ({ ...await importOriginal<typeof import('react-router')>(), useRouteError: vi.fn() }));

it('renders nested application content with the real session and theme providers', async () => {
    vi.mocked(fetch).mockResolvedValueOnce(reply(session()));
    renderPage(<Routes><Route element={<App />}><Route path="/" element={<h1>Child route</h1>} /></Route></Routes>);
    expect(await screen.findByRole('heading', { name: 'Child route' })).toBeVisible();
});
it.each([
    [{ status: 404, statusText: 'Not Found', internal: false, data: 'private route detail' }, 'Page unavailable'],
    [{ status: 500, statusText: 'Error', internal: false, data: 'private route detail' }, 'Unable to open this page'],
    [new Error('private route detail'), 'Unable to open this page'],
])('shows a safe route error without exposing internal details', (error, heading) => {
    vi.mocked(useRouteError).mockReturnValue(error);
    renderPage(<ErrorBoundary />);
    expect(screen.getByRole('heading', { name: heading })).toBeVisible();
    expect(screen.queryByText(/private route detail/)).not.toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'Return to your workspace' })).toHaveAttribute('href', '/organizations');
});
