import { type ReactNode } from 'react';
import { Link, Navigate, useLocation, useNavigate, useParams, useSearchParams } from 'react-router';
import { api } from '../lib/api';
import { useSession } from '../lib/session';
import { useRead } from '../lib/polling';
import { useAppearance } from '../lib/appearance';

export function projectPath(org: string, project: string) { return `/o/${org}/p/${project}`; }
export function Workspace({ children }: { children: ReactNode }) {
    const { session, loading, error, refresh } = useSession();
    const { org = '', project = '' } = useParams();
    const location = useLocation(), navigate = useNavigate();
    const { mode, toggle } = useAppearance();
    if (loading) return <main className="standalone" role="status">Opening your workspace…</main>;
    if (error) return <main className="standalone"><p role="alert">{error}</p><button onClick={() => void refresh()}>Try again</button></main>;
    if (!session) return <Navigate to={`/login?returnTo=${encodeURIComponent(location.pathname + location.search)}`} replace/>;
    const organization = session.organizations?.find(o => o.id === org);
    if (!organization) return <main className="standalone"><h1>Workspace unavailable</h1><p>Your current account has no access to this organization.</p><Link to="/organizations">Your organizations</Link></main>;
    const environment = new URLSearchParams(location.search).get('env');
    const base = projectPath(org, project) + (environment ? `?${new URLSearchParams({ env: environment })}` : '');
    return <div className="workspace review-workspace"><a className="skip-link" href="#workspace-content">Skip to content</a><aside className="sidebar"><Link className="brand" to="/organizations"><img src="/brand/logo-horizontal-reversed.svg" alt="Cosmoformer" width={1954} height={427}/></Link><label className="org-picker">ORGANIZATION<select aria-label="Organization" value={org} onChange={e => navigate(`/organizations?org=${e.target.value}`)}>{session.organizations?.map(o => <option key={o.id} value={o.id}>{o.name}</option>)}</select></label><nav aria-label="Workspace"><Link to={`/organizations?org=${org}`}>All projects</Link><Link to={base} aria-current="page">Project workspace</Link>{(organization.org_admin || session.actor.installation_admin) && <><Link to={`/organizations?org=${org}&page=access`}>Teams & access</Link><Link to={`/organizations?org=${org}&page=settings`}>Sources & workers</Link></>}</nav><div className="sidebar-footer"><span className="avatar">{session.actor.display_name.slice(0, 1)}</span><strong>{session.actor.display_name}</strong><button aria-label="Sign out" onClick={() => { const logout = api('/session/logout', 'LoginOutputBody', { method: 'POST' }); window.dispatchEvent(new Event('cosmoformer:expired')); void logout.then(() => refresh()).catch(() => {}); }}>↪</button></div></aside><main className="workspace-main" id="workspace-content"><header className="topbar"><Link to={`/organizations?org=${org}`}>{organization.name}</Link><button className="theme-button" onClick={toggle}>{mode === 'light' ? 'Dark theme' : 'Light theme'}</button></header><div className="content" key={session.actor.id + '/' + location.pathname}>{children}</div></main></div>;
}
export function ProjectContext({ children }: { children: (context: { org: string; project: string; environment: string; name: string }) => ReactNode }) {
    const { org = '', project = '' } = useParams();
    const [search, setSearch] = useSearchParams();
    const read = useRead(`/organizations/${org}/projects`, 'ProjectsOutputBody', 10000);
    if (!read.data) return <ReadState error={read.error} retry={read.reload}/>;
    const selected = read.data.projects?.find(p => p.id === project);
    if (!selected) return <section className="empty-state"><h1>No project access</h1><p>Ask your organization administrator for access to this project.</p></section>;
    const environment = search.get('env') ?? '';
    if (environment && !selected.environments?.some(e => e.id === environment)) return <section className="empty-state"><h1>Environment unavailable</h1><p>The selected environment is outside your current access.</p><Link to={projectPath(org, project)}>Open visible environments</Link></section>;
    return <><div className="project-context"><Link to={projectPath(org, project) + (environment ? `?${new URLSearchParams({ env: environment })}` : '')}>{selected.name}</Link><label>Environment<select aria-label="Environment filter" value={environment} onChange={e => { const next = new URLSearchParams(search); if (e.target.value) next.set('env', e.target.value); else next.delete('env'); next.delete('before'); setSearch(next); }}><option value="">All visible environments</option>{selected.environments?.map(e => <option key={e.id} value={e.id}>{e.name}</option>)}</select></label></div>{children({ org, project, environment, name: selected.name })}</>;
}
export function ReadState({ error, retry }: { error: string; retry: () => void }) { return error ? <section className="error"><p role="alert">{error}</p><button onClick={retry}>Try again</button></section> : <p role="status">Loading workspace…</p>; }
export function time(value?: string) { return value ? new Date(value).toLocaleString() : 'Not recorded'; }
