import type { ReactNode } from 'react';
import { render } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { Theme } from '@astryxdesign/core/theme';
import { neutralTheme } from '@astryxdesign/theme-neutral/built';
import type { Schema } from '../../app/lib/api';

export const ids = {
    actor: '11111111-1111-4111-8111-111111111111',
    org: '22222222-2222-4222-8222-222222222222',
    otherOrg: '33333333-3333-4333-8333-333333333333',
    project: '44444444-4444-4444-8444-444444444444',
    env: '55555555-5555-4555-8555-555555555555',
    team: '66666666-6666-4666-8666-666666666666',
    member: '77777777-7777-4777-8777-777777777777',
};
export function session(overrides: Partial<Schema['SessionOutputBody']> = {}): Schema['SessionOutputBody'] {
    return {
        actor: { id: ids.actor, username: 'engineer', display_name: 'Engineer', installation_admin: false },
        csrf_token: 'fictional-csrf',
        expires_at: new Date(Date.now() + 3600000).toISOString(),
        organizations: [{ id: ids.org, name: 'Engineering', org_admin: false }],
        invitations: [],
        ...overrides,
    };
}
export function reply(value: unknown, status = 200) {
    return new Response(JSON.stringify(value), { status, headers: { 'Content-Type': 'application/json' } });
}
export function deferred<T>() {
    let resolve!: (value: T) => void;
    let reject!: (reason: unknown) => void;
    const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
    return { promise, resolve, reject };
}
export function renderPage(element: ReactNode, path = '/') {
    return render(<Theme theme={neutralTheme} mode="light"><MemoryRouter initialEntries={[path]}>{element}</MemoryRouter></Theme>);
}
