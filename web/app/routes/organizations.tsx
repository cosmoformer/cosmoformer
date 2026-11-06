import { useCallback, useEffect, useRef, useState, type FormEvent } from 'react';
import { Navigate, useSearchParams } from 'react-router';
import { Button } from '@astryxdesign/core/Button';
import { api, type Schema } from '../lib/api';
import { useSession } from '../lib/session';
type Project = Schema['Project'];
type Account = Schema['AccountPublic'];
type Access = Schema['AccessState'];
const roles = [['viewer', 'Viewer'], ['planner', 'Planner'], ['deployer', 'Deployer'], ['administrator', 'Administrator'], ['approver', 'Approver'], ['raw_evidence_read', 'Raw evidence access']];
function data(form: HTMLFormElement) { return Object.fromEntries(new FormData(form)); }
export default function Organizations() {
    const { session, loading, error: sessionError, refresh } = useSession();
    const [search, setSearch] = useSearchParams();
    const org = search.get('org') ?? session?.organizations?.[0]?.id ?? '';
    const page = search.get('page') ?? 'projects';
    const [projects, setProjects] = useState<Project[]>([]), [accounts, setAccounts] = useState<Account[]>([]), [access, setAccess] = useState<Access | null>(null), [canManage, setCanManage] = useState(false), [error, setError] = useState(''), [notice, setNotice] = useState(''), [busy, setBusy] = useState(false), [fetching, setFetching] = useState(true), [code, setCode] = useState<{
        code: string;
        purpose: string;
    } | null>(null);
    const [reload, setReload] = useState(0);
    const proofGeneration = useRef(0);
    const pending = useRef<AbortController | null>(null), scope = useRef('');
    const currentOrg = session?.organizations?.find(o => o.id === org);
    const admin = !!session?.actor.installation_admin;
    const orgAdmin = admin || !!currentOrg?.org_admin;
    const load = useCallback(async () => {
        pending.current?.abort();
        const controller = new AbortController();
        pending.current = controller;
        const key = page + '/' + org;
        if (scope.current !== key) {
            scope.current = key;
            setFetching(true);
            setProjects([]);
            setAccess(null);
            setAccounts([]);
            setCanManage(false);
        }
        setError('');
        let nextAccounts: Account[] = [], nextProjects: Project[] = [], nextAccess: Access | null = null, nextManage = false;
        try {
            if (page === 'accounts' && admin) {
                const a = await api('/admin/accounts', 'AccountsOutputBody', { signal: controller.signal });
                nextAccounts = a.accounts ?? [];
            }
            if (org) {
                const p = await api(`/organizations/${org}/projects`, 'ProjectsOutputBody', { signal: controller.signal });
                nextProjects = p.projects ?? [];
                nextManage = p.can_manage;
                if (page === 'access' && orgAdmin)
                    nextAccess = await api(`/organizations/${org}/access`, 'AccessState', { signal: controller.signal });
            }
            if (!controller.signal.aborted) {
                setAccounts(nextAccounts);
                setProjects(nextProjects);
                setAccess(nextAccess);
                setCanManage(nextManage);
            }
        }
        catch (e) {
            if (!controller.signal.aborted) {
                setProjects([]);
                setAccess(null);
                setAccounts([]);
                setCanManage(false);
                setError(e instanceof Error ? e.message : 'Unable to load your workspace.');
            }
        }
        finally {
            if (!controller.signal.aborted)
                setFetching(false);
        }
    }, [page, org, admin, orgAdmin]);
    useEffect(() => { if (session?.actor.id)
        void load(); return () => pending.current?.abort(); }, [session?.actor.id, load, reload]);
    useEffect(() => {
        setCode(null); setNotice(''); setBusy(false);
        return () => { proofGeneration.current++; };
    }, [org, page, session?.actor.id]);
    if (loading)
        return <main className="standalone">Opening your workspace…</main>;
    if (sessionError)
        return <main className="standalone"><h1>Unable to connect</h1><p role="alert">{sessionError}</p><Button label="Try again" onClick={() => void refresh()}/></main>;
    if (!session)
        return <Navigate to="/login" replace/>;
    async function perform(fn: () => Promise<unknown>, message: string) { setBusy(true); setError(''); setNotice(''); try {
        await fn();
        setNotice(message);
        await refresh();
        setReload(v => v + 1);
    }
    catch (e) {
        setError(e instanceof Error ? e.message : 'The request could not be completed.');
    }
    finally {
        setBusy(false);
    } }
    function form(fn: (values: Record<string, FormDataEntryValue>) => Promise<unknown>, message: string) { return (e: FormEvent<HTMLFormElement>) => { e.preventDefault(); const f = e.currentTarget; void perform(async () => { await fn(data(f)); f.reset(); }, message); }; }
    function navigate(page: string, nextOrg = org) { setSearch({ ...(nextOrg ? { org: nextOrg } : {}), page }); }
    const base = `/organizations/${org}`;
    async function issue(account: Account) {
        const current = ++proofGeneration.current;
        setCode(null); setBusy(true); setError(''); try {
        const c = await api(`/admin/accounts/${account.id}/${account.pending ? 'setup' : 'reset'}-code`, 'CodeOutputBody', { method: 'POST' });
        if (current === proofGeneration.current) setCode({ code: c.code, purpose: c.purpose });
    }
    catch (e) {
        if (current === proofGeneration.current) setError(e instanceof Error ? e.message : 'Unable to issue code.');
    }
    finally {
        if (current === proofGeneration.current) setBusy(false);
    } }
    return <div className="workspace"><aside className="sidebar"><a className="brand" href="/organizations"><img src="/brand/logo-horizontal-reversed.svg" alt="Cosmoformer" width={1954} height={427}/></a><label className="org-picker">ORGANIZATION<select aria-label="Organization" value={org} onChange={e => navigate('projects', e.target.value)}>{!session.organizations?.length && <option value="">No organizations</option>}{session.organizations?.map(o => <option key={o.id} value={o.id}>{o.name}</option>)}</select></label><nav aria-label="Workspace"><button aria-current={page === 'projects' ? 'page' : undefined} onClick={() => navigate('projects')}><span aria-hidden="true">▦</span><span>Projects</span></button>{orgAdmin && org && <button aria-current={page === 'access' ? 'page' : undefined} onClick={() => navigate('access')}><span aria-hidden="true">♧</span><span>Teams & access</span></button>}{admin && <button aria-current={page === 'accounts' ? 'page' : undefined} onClick={() => navigate('accounts')}><span aria-hidden="true">◎</span><span>Local accounts</span></button>}</nav><div className="sidebar-footer"><span className="avatar">{session.actor.display_name.slice(0, 1).toUpperCase()}</span><div><strong>{session.actor.display_name}</strong><small>{admin ? 'Installation administrator' : 'Team member'}</small></div><button aria-label="Sign out" title="Sign out" onClick={() => void perform(() => api('/session/logout', 'LoginOutputBody', { method: 'POST' }), '')}>↪</button></div></aside>
 <main className="workspace-main"><header className="topbar"><span>{currentOrg?.name ?? 'Your workspace'} <span className="separator">/</span> {page === 'accounts' ? 'Local accounts' : page === 'access' ? 'Teams & access' : 'Projects'}</span><span className="local-badge">LOCAL WORKSPACE</span></header><div className="content"><div className="page-heading"><div><span className="eyebrow">{page === 'accounts' ? 'INSTALLATION' : currentOrg?.name ?? 'WELCOME'}</span><h1>{page === 'accounts' ? 'Local accounts' : page === 'access' ? 'Teams & access' : 'Your projects'}</h1><p className="muted">{page === 'accounts' ? 'Invite people into Cosmoformer. Assign their workspace access separately.' : page === 'access' ? 'Give teams the right access, in the right environment.' : 'Your assigned projects and environments, in one place.'}</p></div><span className="count-badge">{page === 'projects' ? `${projects.length} ${projects.length === 1 ? 'project' : 'projects'}` : page === 'accounts' ? `${accounts.length} ${accounts.length === 1 ? 'account' : 'accounts'}` : `${access?.teams?.length ?? 0} teams`}</span></div>
 {error && <div className="error" role="alert">{error}</div>}{notice && <div className="success" role="status">{notice}</div>}
 {!!session.invitations?.length && <section className="panel"><h2>Your invitations</h2>{session.invitations.map(i => <div key={i.id} className="row"><span><strong>{i.name}</strong><small>Accept to join. Project access is assigned separately.</small></span><Button label="Accept invitation" isDisabled={busy} onClick={() => void perform(() => api(`/organizations/${i.org_id}/invitations/${i.id}/accept`, 'OKBody', { method: 'POST' }), 'Invitation accepted.')}/></div>)}</section>}
 {fetching ? <p role="status">Loading workspace…</p> : page === 'projects' ? <>
 {!projects.length && <section className="empty-state"><span className="empty-symbol">▦</span><h2>{canManage ? 'Create your first project' : 'No projects assigned yet'}</h2><p>{canManage ? 'Organize your team’s work by project, then add its environments.' : 'Ask your organization administrator to add you to a team with project access.'}</p></section>}
 <div className="project-grid">{projects.map(p => <article key={p.id} className="project-card"><div className="project-icon">{p.name.slice(0, 1)}</div><h2>{p.name}</h2><p className="muted">{p.environments?.length ?? 0} visible {(p.environments?.length ?? 0) === 1 ? 'environment' : 'environments'}</p><div className="environment-list">{p.environments?.map(e => <span key={e.id} className={'environment ' + (/prod/i.test(e.name) ? 'production' : '')}>{e.name}</span>)}</div>{p.can_manage && <form className="inline-form" onSubmit={form(v => api(`${base}/projects/${p.id}/environments`, 'IDResultBody', { method: 'POST', body: { name: v.name } }), 'Environment created.')}><input aria-label={`New environment for ${p.name}`} name="name" placeholder="New environment" required maxLength={100}/><Button label="Add" type="submit" isDisabled={busy}/></form>}</article>)}</div>
 {canManage && <section className="panel"><h2>Create a project</h2><form className="inline-form" onSubmit={form(v => api(`${base}/projects`, 'IDResultBody', { method: 'POST', body: v }), 'Project created.')}><label>Project name<input name="name" placeholder="e.g. Payments" required maxLength={100}/></label><Button label="Create project" type="submit" isDisabled={busy}/></form></section>}
 {admin && !org && <section className="panel"><h2>Create an organization</h2><form className="inline-form" onSubmit={form(async (v) => { const result = await api('/organizations', 'IDResultBody', { method: 'POST', body: v }); navigate('projects', result.id); }, 'Organization created.')}><label>Organization name<input name="name" required maxLength={100}/></label><Button label="Create organization" type="submit" isDisabled={busy}/></form></section>}
 </> : page === 'accounts' && admin ? <>
 {code && <section className="panel once-visible" role="status"><h2>{code.purpose === 'setup' ? 'Setup' : 'Reset'} code · shown once</h2><p>Share privately with the account owner. It will disappear when you leave this page.</p><code>{code.code}</code><p><button className="text-button" onClick={() => setCode(null)}>Dismiss code</button></p></section>}
 <section className="panel table-panel"><table><thead><tr><th>Person</th><th>Status</th><th>Installation role</th><th>Actions</th></tr></thead><tbody>{accounts.map(a => <tr key={a.id}><td><strong>{a.display_name}</strong><small>{a.username}</small></td><td><span className="status-badge">{!a.enabled ? 'Disabled' : a.pending ? 'Awaiting setup' : 'Active'}</span></td><td>{a.installation_admin ? 'Administrator' : 'Member'}</td><td><div className="actions"><button className="text-button" disabled={busy || !a.enabled} onClick={() => void issue(a)}>{a.pending ? 'Setup code' : 'Reset code'}</button><button className="text-button" disabled={busy || a.id === session.actor.id} onClick={() => { if (confirm(`${a.enabled ? 'Disable' : 'Enable'} ${a.display_name}?`))
            void perform(() => api(`/admin/accounts/${a.id}`, 'OKBody', { method: 'PATCH', body: { revision: a.revision, display_name: a.display_name, enabled: !a.enabled, installation_admin: a.installation_admin } }), 'Account updated.'); }}>{a.enabled ? 'Disable' : 'Enable'}</button><button className="text-button" disabled={busy || a.id === session.actor.id} onClick={() => { if (confirm(`Change installation administrator access for ${a.display_name}?`))
            void perform(() => api(`/admin/accounts/${a.id}`, 'OKBody', { method: 'PATCH', body: { revision: a.revision, enabled: a.enabled, display_name: a.display_name, installation_admin: !a.installation_admin } }), 'Administrator access updated.'); }}>{a.installation_admin ? 'Remove admin' : 'Make admin'}</button></div></td></tr>)}</tbody></table></section>
 <section className="panel"><h2>Create a local account</h2><p className="muted">New accounts start with no organization or project access.</p><form className="inline-form" onSubmit={form(v => api('/admin/accounts', 'IDResultBody', { method: 'POST', body: v }), 'Account created. Issue a setup code to share privately.')}><label>Username<input name="username" required pattern="[A-Za-z][A-Za-z0-9._-]{2,63}" autoComplete="off"/></label><label>Display name<input name="display_name" required maxLength={100}/></label><Button label="Create account" type="submit" isDisabled={busy}/></form></section>
 <section className="panel"><h2>Add an organization</h2><form className="inline-form" onSubmit={form(v => api('/organizations', 'IDResultBody', { method: 'POST', body: v }), 'Organization created.')}><label>Name<input name="name" required maxLength={100}/></label><Button label="Create organization" type="submit" isDisabled={busy}/></form></section>
 </> : page === 'access' && orgAdmin ? <>
 <section className="notice"><strong>Administration and deployment are separate.</strong><p>Administrator manages configuration. Deployer can apply and stop runs. Approver and raw evidence access are separate grants. Project grants include all environments; environment grants add access.</p></section>
 <div className="two-columns"><section className="panel"><h2>Invite a member</h2><form onSubmit={form(v => api(`${base}/invitations`, 'IDResultBody', { method: 'POST', body: { username: v.username, org_admin: v.org_admin === 'on' } }), 'Invitation sent in-app. The account owner must accept it.')}><label>Local username<input name="username" required/></label><label className="checkbox"><input type="checkbox" name="org_admin"/>Organization administrator</label><Button label="Create invitation" type="submit" isDisabled={busy}/></form></section><section className="panel"><h2>Create a team</h2><form onSubmit={form(v => api(`${base}/teams`, 'IDResultBody', { method: 'POST', body: v }), 'Team created.')}><label>Team name<input name="name" required maxLength={100}/></label><Button label="Create team" type="submit" isDisabled={busy}/></form></section></div>
 <section className="panel"><h2>Organization members</h2>{access?.members?.map(m => <div className="row" key={m.id}><div><strong>{m.display_name}</strong><small>{m.username} · {m.org_admin ? 'Organization administrator' : 'Member'}</small></div><div className="actions"><button className="text-button" disabled={busy || m.id === session.actor.id} onClick={() => { if (confirm(`Change organization administrator access for ${m.display_name}?`))
            void perform(() => api(`${base}/members/${m.id}`, 'OKBody', { method: 'PATCH', body: { revision: m.revision, org_admin: !m.org_admin } }), 'Organization role updated.'); }}>{m.org_admin ? 'Remove admin' : 'Make admin'}</button><button className="text-button" disabled={busy || m.id === session.actor.id} onClick={() => { if (confirm(`Remove ${m.display_name} from this organization and its teams?`))
            void perform(() => api(`${base}/members/${m.id}`, 'OKBody', { method: 'DELETE' }), 'Member removed from the organization.'); }}>Remove member</button></div></div>)}</section><section className="panel"><h2>Teams</h2>{!access?.teams?.length && <p className="muted">Create a team to start assigning project access.</p>}{access?.teams?.map(t => <div className="team-row" key={t.id}><h3>{t.name}</h3><div className="member-chips">{t.members?.map(id => <span key={id} className="member-chip">{access.members?.find(m => m.id === id)?.display_name ?? 'Member'}<button aria-label={`Remove ${access.members?.find(m => m.id === id)?.display_name ?? 'member'} from ${t.name}`} disabled={busy} onClick={() => void perform(() => api(`${base}/teams/${t.id}/members/${id}`, 'OKBody', { method: 'DELETE' }), 'Team member removed.')}>×</button></span>)}</div><form className="inline-form" onSubmit={form(v => api(`${base}/teams/${t.id}/members`, 'OKBody', { method: 'POST', body: v }), 'Team member added.')}><select name="account_id" aria-label={`Member to add to ${t.name}`} required defaultValue=""><option value="" disabled>Select a member</option>{access.members?.filter(m => !t.members?.includes(m.id)).map(m => <option key={m.id} value={m.id}>{m.display_name}</option>)}</select><Button label="Add member" type="submit" isDisabled={busy}/></form></div>)}</section>
 <section className="panel"><h2>Project access</h2>{access?.grants?.map(g => <div key={g.id} className="row"><div><strong>{access.teams?.find(t => t.id === g.team_id)?.name ?? 'Team'} · {roles.find(r => r[0] === g.role)?.[1]}</strong><small>{projects.find(p => p.id === g.project_id)?.name ?? 'Project'} / {g.environment_id ? projects.flatMap(p => p.environments ?? []).find(e => e.id === g.environment_id)?.name ?? 'Environment' : 'All environments'}</small></div><button className="text-button" disabled={busy} onClick={() => void perform(() => api(`${base}/grants/${g.id}`, 'OKBody', { method: 'DELETE' }), 'Access revoked.')}>Revoke</button></div>)}<GrantForm teams={access?.teams ?? []} projects={projects} busy={busy} submit={v => perform(() => api(`${base}/grants`, 'IDResultBody', { method: 'POST', body: v }), 'Access granted.')}/></section>
 </> : <section className="empty-state"><h2>This page is unavailable</h2><p>Choose a workspace you have access to.</p></section>}
 </div></main></div>;
}
function GrantForm({ teams, projects, busy, submit }: {
    teams: NonNullable<Access['teams']>;
    projects: Project[];
    busy: boolean;
    submit: (v: Record<string, string>) => Promise<void>;
}) {
    const [project, setProject] = useState(''), [environment, setEnvironment] = useState('');
    return <form className="grant-form" onSubmit={e => { e.preventDefault(); const v = Object.fromEntries(new FormData(e.currentTarget)) as Record<string, string>; void submit(v); }}><label>Team<select aria-label="Team" name="team_id" required defaultValue=""><option value="" disabled>Select team</option>{teams.map(t => <option key={t.id} value={t.id}>{t.name}</option>)}</select></label><label>Project<select aria-label="Project" name="project_id" required value={project} onChange={e => { setProject(e.target.value); setEnvironment(''); }}><option value="" disabled>Select project</option>{projects.map(p => <option key={p.id} value={p.id}>{p.name}</option>)}</select></label><label>Environment<select aria-label="Environment" name="environment_id" value={environment} onChange={e => setEnvironment(e.target.value)}><option value="">All environments</option>{projects.find(p => p.id === project)?.environments?.map(e => <option key={e.id} value={e.id}>{e.name}</option>)}</select></label><label>Role<select aria-label="Role" name="role" defaultValue="viewer">{roles.map(([v, label]) => <option key={v} value={v}>{label}</option>)}</select></label><Button label="Grant access" type="submit" isDisabled={busy || !project}/></form>;
}
