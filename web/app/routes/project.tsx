import { Link, useSearchParams } from 'react-router';
import { ProjectContext, projectPath, ReadState, Workspace } from '../components/workspace';
import { useRead } from '../lib/polling';
import { PullRequests } from './pull-requests';
import { Modules } from './modules';
import { RunRows } from '../components/run-context';
export default function Project() { return <Workspace><ProjectContext>{c => <ProjectBody key={c.environment} {...c}/>}</ProjectContext></Workspace>; }
function ProjectBody({ org, project, environment, name }: { org: string; project: string; environment: string; name: string }) {
    const [search, setSearch] = useSearchParams();
    const view = search.get('view') === 'modules' ? 'modules' : search.get('view') === 'pull-requests' ? 'pull-requests' : search.get('view') === 'runs' ? 'runs' : 'stacks';
    const query = new URLSearchParams({ project_id: project, ...(environment ? { environment_id: environment } : {}), ...(search.get('before') ? { before: search.get('before')! } : {}) });
    return <><div className="page-heading"><div><span className="eyebrow">PROJECT WORKSPACE</span><h1>{name}</h1><p className="muted">Source, planning, and retained review evidence.</p></div></div><nav className="view-nav" aria-label="Project views">{['stacks', 'runs', 'pull-requests', 'modules'].map(v => <Link key={v} aria-current={view === v ? 'page' : undefined} to={`?${new URLSearchParams({ ...(environment ? { env: environment } : {}), view: v })}`}>{v === 'stacks' ? 'Stacks' : v === 'runs' ? 'Runs' : v === 'modules' ? 'Modules' : 'Pull requests'}</Link>)}</nav>{view === 'modules' ? <Modules org={org} project={project} query={query.toString()}/> : view === 'pull-requests' ? <PullRequests org={org} project={project} query={query.toString()}/> : view === 'stacks' ? <StackRows org={org} project={project} query={query.toString()} next={cursor => { const p = new URLSearchParams(search); p.set('before', cursor); setSearch(p); }}/> : <ProjectRuns org={org} project={project} query={query.toString()} next={cursor => { const p = new URLSearchParams(search); p.set('before', cursor); setSearch(p); }}/>}</>;
}
function StackRows({ org, project, query, next }: { org: string; project: string; query: string; next: (cursor: string) => void }) {
    const read = useRead(`/organizations/${org}/stacks?${query}`, 'StacksOutputBody', 10000);
    if (!read.data) return <ReadState error={read.error} retry={read.reload}/>;
    return <>{!read.data.stacks.length ? <section className="empty-state"><h2>No visible stacks</h2><p>Import a repository inventory through Sources & workers to make authorized stacks available.</p></section> : <div className="stack-grid">{read.data.stacks.map(s => <article className="panel stack-card" key={s.id}><div className="row"><span className={'environment ' + (/prod/i.test(s.environment_name) ? 'production' : '')}>{s.environment_name}</span><span className="muted">{s.archived ? 'Archived' : s.assessment === 'compatible' ? 'Assessed compatible' : 'Compatibility ' + s.assessment.replaceAll('_', ' ')}</span></div><h2><Link to={`${projectPath(org, project)}/stacks/${s.id}?env=${s.environment_id}`}>{s.name}</Link></h2><p className="muted">{s.tracked_branch} · <code>{s.commit.slice(0, 12)}</code></p><small>{s.source_url}</small><p>{s.can_native_plan ? 'Native planning available' : s.plan_reason}</p></article>)}</div>}{read.data.next_cursor && <button onClick={() => next(read.data!.next_cursor!)}>Next stacks</button>}</>;
}
export function ProjectRuns({ org, project, query, next }: { org: string; project: string; query: string; next: (cursor: string) => void }) {
    const read = useRead(`/organizations/${org}/runs?${query}`, 'RunsOutputBody', 5000);
    if (!read.data) return <ReadState error={read.error} retry={read.reload}/>;
    return <><RunRows org={org} project={project} runs={read.data.runs}/>{read.data.next_cursor && <button onClick={() => next(read.data!.next_cursor!)}>Older runs</button>}</>;
}
