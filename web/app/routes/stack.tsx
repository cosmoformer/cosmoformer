import { useParams, useSearchParams } from 'react-router';
import { ProjectContext, ReadState, Workspace } from '../components/workspace';
import { useRead } from '../lib/polling';
import { PlanningActions } from '../components/action-dialog';
import { StackDependencies } from '../components/stack-dependencies';
import { ProjectRuns } from './project';
export default function Stack() { return <Workspace><ProjectContext>{c => <StackBody key={c.environment} {...c}/>}</ProjectContext></Workspace>; }
function StackBody({ org, project, environment }: { org: string; project: string; environment: string }) {
    const { stack = '' } = useParams();
    const [search, setSearch] = useSearchParams();
    const query = new URLSearchParams({ project_id: project, stack_id: stack, ...(environment ? { environment_id: environment } : {}) });
    const read = useRead(`/organizations/${org}/stacks?${query}`, 'StacksOutputBody', 10000);
    if (!read.data) return <ReadState error={read.error} retry={read.reload}/>;
    const s = read.data.stacks[0];
    if (!s) return <section className="empty-state"><h1>Stack unavailable</h1><p>This stack is outside the selected project, environment, or current access.</p></section>;
    return <><div className="page-heading"><div><span className="eyebrow">STACK · {s.environment_name}</span><h1>{s.name}</h1><p className="muted">{s.archived ? 'Archived stack' : 'Current stack source'}</p></div><span className="environment">{s.assessment.replaceAll('_', ' ')}</span></div><section className="panel"><h2>Current source</h2><p>{s.source_url}</p><p>{s.tracked_branch} · <code>{s.commit}</code></p><p>Root: <code>{s.root || '.'}</code></p><small>Each historical run retains its own source identity and commit.</small></section><PlanningActions key={`${s.id}/${s.can_plan}/${s.can_native_plan}`} org={org} stack={s}/><StackDependencies org={org} stack={s.id}/><h2>Runs</h2>{s.can_review ? <ProjectRuns org={org} project={project} query={new URLSearchParams({ ...Object.fromEntries(query), ...(search.get('before') ? { before: search.get('before')! } : {}) }).toString()} next={cursor => { const p = new URLSearchParams(search); p.set('before', cursor); setSearch(p); }}/> : <p className="notice">A Viewer grant is required to read execution evidence. Administration alone does not grant it.</p>}</>;
}
