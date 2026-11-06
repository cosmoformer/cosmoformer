import { act, screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Route, Routes } from 'react-router';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import Organizations from '../../app/routes/organizations';
import { useSession } from '../../app/lib/session';
import type { Schema } from '../../app/lib/api';
import { deferred, ids, renderPage, reply, session } from './helpers';

vi.mock('../../app/lib/session', () => ({ useSession: vi.fn() }));
const refresh = vi.fn<() => Promise<void>>();
let current: ReturnType<typeof useSession>;
let projects: Schema['Project'][];
let accounts: Schema['AccountPublic'][];
let access: Schema['AccessState'];
const base = '/api/v1/organizations/' + ids.org;
const proof = 'fictional-setup-code-'.padEnd(43, 'x');
function installAPI() {
    vi.mocked(fetch).mockImplementation(async (url, options) => {
        const path = String(url);
        if (options?.method !== 'GET') {
            if (path.endsWith('-code')) return reply({ code: proof, purpose: path.endsWith('setup-code') ? 'setup' : 'reset', expires_in_seconds: 1800 });
            if (options?.method === 'POST' && !path.endsWith('/logout') && !path.includes('/members') && !path.endsWith('/accept')) return reply({ id: ids.project });
            return reply({ ok: true });
        }
        if (path === '/api/v1/admin/accounts') return reply({ accounts });
        if (path.endsWith('/projects')) return reply({ projects, can_manage: current.session?.actor.installation_admin || current.session?.organizations?.some(o => o.org_admin) || false });
        if (path.endsWith('/access')) return reply(access);
        throw new Error('Unexpected fixture endpoint: ' + path);
    });
}
function mount(query = '') {
    return renderPage(<Routes><Route path="/organizations" element={<Organizations />} /><Route path="/login" element={<h1>Sign-in destination</h1>} /></Routes>, '/organizations' + query);
}
function admin() {
    current.session = session({ actor: { ...session().actor, installation_admin: true } });
}
function sent(path: string, method: string, body?: unknown) {
    expect(fetch).toHaveBeenCalledWith(path, expect.objectContaining({ method, ...(body === undefined ? {} : { body: JSON.stringify(body) }) }));
}
beforeEach(() => {
    refresh.mockReset().mockResolvedValue();
    current = { session: session(), loading: false, error: '', refresh };
    vi.mocked(useSession).mockImplementation(() => current);
    projects = [{ id: ids.project, name: 'Payments', can_manage: false, environments: [{ id: ids.env, name: 'Production' }] }];
    accounts = [
        { ...session().actor, enabled: true, pending: false, revision: 1 },
        { id: ids.member, username: 'colleague', display_name: 'Colleague', installation_admin: false, enabled: true, pending: true, revision: 3 },
    ];
    access = {
        members: [
            { id: ids.actor, username: 'engineer', display_name: 'Engineer', org_admin: true, revision: 1 },
            { id: ids.member, username: 'colleague', display_name: 'Colleague', org_admin: false, revision: 3 },
        ],
        teams: [{ id: ids.team, name: 'Engineers', members: [ids.actor] }],
        grants: [{ id: ids.env, team_id: ids.team, project_id: ids.project, environment_id: ids.env, role: 'viewer' }],
    };
    installAPI();
});

describe('workspace visibility and loading', () => {
    it('shows bootstrap progress without fetching protected data', () => {
        current.loading = true; current.session = null;
        mount(); expect(screen.getByText('Opening your workspace…')).toBeVisible(); expect(fetch).not.toHaveBeenCalled();
    });
    it('offers an explicit retry after a session failure', async () => {
        current.error = 'offline'; current.session = null;
        mount(); expect(screen.getByRole('alert')).toHaveTextContent('offline');
        await userEvent.setup().click(screen.getByRole('button', { name: 'Try again' }));
        expect(refresh).toHaveBeenCalledTimes(1); expect(fetch).not.toHaveBeenCalled();
    });
    it('redirects signed-out visitors without fetching or replaying mutations', async () => {
        current.session = null; mount();
        expect(await screen.findByRole('heading', { name: 'Sign-in destination' })).toBeVisible(); expect(fetch).not.toHaveBeenCalled();
    });
    it('shows only returned projects and environments, with no administration controls for members', async () => {
        mount(); expect(await screen.findByRole('heading', { name: 'Payments' })).toBeVisible();
        expect(screen.getByText('Production')).toBeVisible();
        for (const name of ['Local accounts', 'Teams & access', 'Create project']) expect(screen.queryByRole('button', { name })).not.toBeInTheDocument();
        expect(screen.queryByLabelText('New environment for Payments')).not.toBeInTheDocument();
    });
    it('shows a useful empty-access state for a new account', async () => {
        current.session = session({ organizations: [] }); projects = [];
        mount(); expect(await screen.findByRole('heading', { name: 'No projects assigned yet' })).toBeVisible();
        expect(screen.getByRole('combobox', { name: 'Organization' })).toHaveTextContent('No organizations');
        expect(fetch).not.toHaveBeenCalled();
    });
    it('does not render privileged forms when a member guesses an administration page URL', async () => {
        mount('?page=accounts');
        expect(await screen.findByRole('heading', { name: 'This page is unavailable' })).toBeVisible();
        expect(vi.mocked(fetch).mock.calls.some(([url]) => String(url).includes('/admin/'))).toBe(false);
    });
    it.each([new Error('Access was revoked.'), 'unknown'])('clears project data after a failed load', async error => {
        vi.mocked(fetch).mockRejectedValue(error);
        mount(); expect(await screen.findByRole('alert')).toHaveTextContent(error instanceof Error ? error.message : 'Unable to load your workspace.');
        expect(screen.queryByRole('heading', { name: 'Payments' })).not.toBeInTheDocument();
    });
    it('aborts an old organization request and ignores its late response', async () => {
        current.session = session({ organizations: [...session().organizations!, { id: ids.otherOrg, name: 'Other organization', org_admin: false }] });
        const old = deferred<Response>(); let oldSignal: AbortSignal | undefined;
        vi.mocked(fetch).mockImplementation(async (url, options) => {
            if (String(url).includes(ids.org)) { oldSignal = options?.signal as AbortSignal; return old.promise; }
            return reply({ projects: [{ ...projects[0], name: 'Other project' }], can_manage: false });
        });
        mount(); await userEvent.setup().selectOptions(screen.getByRole('combobox', { name: 'Organization' }), ids.otherOrg);
        expect(await screen.findByRole('heading', { name: 'Other project' })).toBeVisible();
        expect(oldSignal?.aborted).toBe(true);
        await act(async () => { old.resolve(reply({ projects, can_manage: false })); });
        expect(screen.queryByRole('heading', { name: 'Payments' })).not.toBeInTheDocument();
        expect(screen.getByRole('heading', { name: 'Other project' })).toBeVisible();
    });
});

describe('workspace mutations', () => {
    it('creates projects and environments with the selected organization scope', async () => {
        admin(); projects[0].can_manage = true; mount();
        await screen.findByRole('heading', { name: 'Payments' }); const user = userEvent.setup();
        await user.type(screen.getByLabelText('Project name'), 'New project');
        await user.click(screen.getByRole('button', { name: 'Create project' }));
        await screen.findByText('Project created.'); sent(base + '/projects', 'POST', { name: 'New project' });
        expect(screen.getByLabelText('Project name')).toHaveValue('');
        await user.type(screen.getByLabelText('New environment for Payments'), 'Staging');
        await user.click(screen.getByRole('button', { name: 'Add' }));
        await screen.findByText('Environment created.'); sent(base + '/projects/' + ids.project + '/environments', 'POST', { name: 'Staging' });
    });
    it('offers organization creation for an installation administrator with no organizations', async () => {
        admin(); current.session!.organizations = []; projects = [];
        mount(); const user = userEvent.setup();
        await user.type(await screen.findByLabelText('Organization name'), 'First organization');
        await user.click(screen.getByRole('button', { name: 'Create organization' }));
        await waitFor(() => sent('/api/v1/organizations', 'POST', { name: 'First organization' }));
        await waitFor(() => expect(refresh).toHaveBeenCalledTimes(1));
    });
    it('accepts an invitation with its own organization and invitation identifiers', async () => {
        current.session!.invitations = [{ id: ids.env, org_id: ids.otherOrg, name: 'Invitation organization' }];
        mount(); await screen.findByRole('heading', { name: 'Payments' });
        await userEvent.setup().click(screen.getByRole('button', { name: 'Accept invitation' }));
        await screen.findByText('Invitation accepted.'); sent('/api/v1/organizations/' + ids.otherOrg + '/invitations/' + ids.env + '/accept', 'POST');
    });
    it.each([new Error('Conflict. Refresh first.'), 'unknown'])('preserves form input on failure and never retries automatically', async error => {
        admin(); mount(); await screen.findByRole('heading', { name: 'Payments' });
        vi.mocked(fetch).mockRejectedValueOnce(error);
        const user = userEvent.setup(); await user.type(screen.getByLabelText('Project name'), 'Keep this name');
        await user.click(screen.getByRole('button', { name: 'Create project' }));
        expect(await screen.findByRole('alert')).toHaveTextContent(error instanceof Error ? error.message : 'The request could not be completed.');
        expect(screen.getByLabelText('Project name')).toHaveValue('Keep this name'); expect(refresh).not.toHaveBeenCalled();
        expect(vi.mocked(fetch).mock.calls.filter(([, options]) => options?.method === 'POST')).toHaveLength(1);
    });
    it('disables duplicate submissions while a mutation is pending', async () => {
        admin(); mount(); await screen.findByRole('heading', { name: 'Payments' });
        const pending = deferred<Response>(); vi.mocked(fetch).mockReturnValueOnce(pending.promise);
        const user = userEvent.setup(); await user.type(screen.getByLabelText('Project name'), 'New');
        await user.click(screen.getByRole('button', { name: 'Create project' }));
        expect(screen.getByRole('button', { name: 'Create project' })).toBeDisabled();
        await user.click(screen.getByRole('button', { name: 'Create project' }));
        expect(vi.mocked(fetch).mock.calls.filter(([, options]) => options?.method === 'POST')).toHaveLength(1);
        await act(async () => { pending.resolve(reply({ id: ids.project })); });
        await screen.findByText('Project created.');
    });
});

describe('local account administration', () => {
    it('creates an account without granting permissions', async () => {
        admin(); mount('?page=accounts'); await screen.findByRole('heading', { name: 'Create a local account' });
        const user = userEvent.setup(); await user.type(screen.getByLabelText('Username'), 'new.engineer'); await user.type(screen.getByLabelText('Display name'), 'New engineer');
        await user.click(screen.getByRole('button', { name: 'Create account' }));
        await screen.findByText('Account created. Issue a setup code to share privately.');
        sent('/api/v1/admin/accounts', 'POST', { username: 'new.engineer', display_name: 'New engineer' });
    });
    it.each([true, false])('issues the correct proof for pending=%s and allows dismissing it', async pending => {
        admin(); accounts[1].pending = pending; mount('?page=accounts'); const user = userEvent.setup();
        const row = (await screen.findByText('colleague')).closest('tr')!;
        await user.click(within(row).getByRole('button', { name: pending ? 'Setup code' : 'Reset code' }));
        expect(await screen.findByText(proof)).toBeVisible();
        sent('/api/v1/admin/accounts/' + ids.member + '/' + (pending ? 'setup' : 'reset') + '-code', 'POST');
        await user.click(screen.getByRole('button', { name: 'Dismiss code' })); expect(screen.queryByText(proof)).not.toBeInTheDocument();
    });
    it('clears a one-use code when navigating away and does not restore it on return', async () => {
        admin(); mount('?page=accounts'); const user = userEvent.setup();
        await user.click(await screen.findByRole('button', { name: 'Setup code' })); await screen.findByText(proof);
        await user.click(screen.getByRole('button', { name: 'Projects' }));
        expect(screen.queryByText(proof)).not.toBeInTheDocument();
        await user.click(screen.getByRole('button', { name: 'Local accounts' })); await screen.findByRole('heading', { name: 'Create a local account' });
        expect(screen.queryByText(proof)).not.toBeInTheDocument();
    });
    it('ignores a proof response arriving after leaving the accounts page', async () => {
        admin(); mount('?page=accounts'); const user = userEvent.setup();
        await screen.findByRole('button', { name: 'Setup code' });
        const pending = deferred<Response>(); vi.mocked(fetch).mockReturnValueOnce(pending.promise);
        await user.click(screen.getByRole('button', { name: 'Setup code' }));
        await user.click(screen.getByRole('button', { name: 'Projects' }));
        await screen.findByRole('heading', { name: 'Payments' });
        await user.click(screen.getByRole('button', { name: 'Local accounts' }));
        await screen.findByRole('heading', { name: 'Create a local account' });
        await act(async () => { pending.resolve(reply({ code: proof, purpose: 'setup', expires_in_seconds: 86400 })); });
        expect(screen.queryByText(proof)).not.toBeInTheDocument();
    });
    it.each([new Error('Proof unavailable.'), 'unknown'])('clears an earlier code when issuing a replacement fails', async error => {
        admin(); mount('?page=accounts'); const user = userEvent.setup();
        await user.click(await screen.findByRole('button', { name: 'Setup code' })); await screen.findByText(proof);
        vi.mocked(fetch).mockRejectedValueOnce(error); await user.click(screen.getByRole('button', { name: 'Setup code' }));
        expect(await screen.findByRole('alert')).toHaveTextContent(error instanceof Error ? error.message : 'Unable to issue code.');
        expect(screen.queryByText(proof)).not.toBeInTheDocument();
    });
    it.each(['Disable', 'Make admin'])('requires confirmation before %s and submits the current revision', async action => {
        admin(); mount('?page=accounts'); const user = userEvent.setup();
        const row = (await screen.findByText('colleague')).closest('tr')!;
        const confirm = vi.spyOn(window, 'confirm').mockReturnValueOnce(false).mockReturnValueOnce(true);
        await user.click(within(row).getByRole('button', { name: action }));
        expect(vi.mocked(fetch).mock.calls.filter(([, opts]) => opts?.method === 'PATCH')).toHaveLength(0);
        await user.click(within(row).getByRole('button', { name: action }));
        await waitFor(() => expect(refresh).toHaveBeenCalled());
        expect(confirm).toHaveBeenCalledTimes(2);
        sent('/api/v1/admin/accounts/' + ids.member, 'PATCH', action === 'Disable'
            ? { revision: 3, display_name: 'Colleague', enabled: false, installation_admin: false }
            : { revision: 3, enabled: true, display_name: 'Colleague', installation_admin: true });
        const self = screen.getByText('engineer', { selector: 'small' }).closest('tr')!;
        expect(within(self).getByRole('button', { name: 'Disable' })).toBeDisabled();
    });
});

describe('organization teams and scoped grants', () => {
    it('lets an organization administrator manage access without installation account controls', async () => {
        current.session!.organizations![0].org_admin = true; mount('?page=access');
        expect(await screen.findByRole('heading', { name: 'Invite a member' })).toBeVisible();
        expect(screen.queryByRole('button', { name: 'Local accounts' })).not.toBeInTheDocument();
    });
    it('invites members and creates teams in the current organization', async () => {
        admin(); mount('?page=access'); const user = userEvent.setup(); await screen.findByRole('heading', { name: 'Invite a member' });
        await user.type(screen.getByLabelText('Local username'), 'colleague'); await user.click(screen.getByLabelText('Organization administrator'));
        await user.click(screen.getByRole('button', { name: 'Create invitation' }));
        await screen.findByText('Invitation sent in-app. The account owner must accept it.');
        sent(base + '/invitations', 'POST', { username: 'colleague', org_admin: true });
        await user.type(screen.getByLabelText('Team name'), 'New team'); await user.click(screen.getByRole('button', { name: 'Create team' }));
        await screen.findByText('Team created.'); sent(base + '/teams', 'POST', { name: 'New team' });
    });
    it('adds and removes team members and excludes existing members from the picker', async () => {
        admin(); mount('?page=access'); const user = userEvent.setup();
        const picker = await screen.findByRole('combobox', { name: 'Member to add to Engineers' });
        expect(within(picker).queryByRole('option', { name: 'Engineer' })).not.toBeInTheDocument();
        await user.selectOptions(picker, ids.member); await user.click(screen.getByRole('button', { name: 'Add member' }));
        await screen.findByText('Team member added.'); sent(base + '/teams/' + ids.team + '/members', 'POST', { account_id: ids.member });
        await user.click(screen.getByRole('button', { name: 'Remove Engineer from Engineers' }));
        await screen.findByText('Team member removed.'); sent(base + '/teams/' + ids.team + '/members/' + ids.actor, 'DELETE');
    });
    it('grants an exact environment and resets that selection when the project changes', async () => {
        admin(); projects.push({ id: ids.otherOrg, name: 'Other project', can_manage: true, environments: [] });
        mount('?page=access'); const user = userEvent.setup();
        await screen.findByRole('heading', { name: 'Project access' });
        expect(screen.getByRole('button', { name: 'Grant access' })).toBeDisabled();
        await user.selectOptions(screen.getByRole('combobox', { name: 'Team' }), ids.team);
        await user.selectOptions(screen.getByRole('combobox', { name: 'Project' }), ids.project);
        await user.selectOptions(screen.getByRole('combobox', { name: 'Environment' }), ids.env);
        await user.selectOptions(screen.getByRole('combobox', { name: 'Role' }), 'deployer');
        await user.click(screen.getByRole('button', { name: 'Grant access' }));
        await screen.findByText('Access granted.');
        sent(base + '/grants', 'POST', { team_id: ids.team, project_id: ids.project, environment_id: ids.env, role: 'deployer' });
        await user.selectOptions(screen.getByRole('combobox', { name: 'Project' }), ids.otherOrg);
        expect(screen.getByRole('combobox', { name: 'Environment' })).toHaveValue('');
        expect(screen.queryByRole('option', { name: 'Production' })).not.toBeInTheDocument();
    });
    it('revokes the selected grant and refreshes current authority', async () => {
        admin(); mount('?page=access'); await userEvent.setup().click(await screen.findByRole('button', { name: 'Revoke' }));
        await screen.findByText('Access revoked.'); sent(base + '/grants/' + ids.env, 'DELETE'); expect(refresh).toHaveBeenCalledTimes(1);
    });
    it.each(['Make admin', 'Remove member'])('confirms membership action %s and protects self membership', async action => {
        admin(); mount('?page=access'); const user = userEvent.setup(); await screen.findByRole('heading', { name: 'Organization members' });
        const section = screen.getByRole('heading', { name: 'Organization members' }).closest('section')!;
        const memberRow = within(section).getByText('Colleague').closest('.row') as HTMLElement;
        const confirm = vi.spyOn(window, 'confirm').mockReturnValueOnce(false).mockReturnValueOnce(true);
        await user.click(within(memberRow).getByRole('button', { name: action }));
        expect(refresh).not.toHaveBeenCalled();
        await user.click(within(memberRow).getByRole('button', { name: action })); await waitFor(() => expect(refresh).toHaveBeenCalledTimes(1));
        expect(confirm).toHaveBeenCalledTimes(2);
        sent(base + '/members/' + ids.member, action === 'Make admin' ? 'PATCH' : 'DELETE', action === 'Make admin' ? { revision: 3, org_admin: true } : undefined);
        expect(within(section).getByRole('button', { name: 'Remove admin' })).toBeDisabled();
    });
});
