import { useEffect, useRef, useState, type FormEvent } from 'react';
import { Link, useNavigate, useParams, useSearchParams } from 'react-router';
import { api, APIError, type Schema } from '../lib/api';
import { useRead } from '../lib/polling';
import { ProjectContext, projectPath, ReadState, Workspace } from '../components/workspace';

function modulePath(org: string, project: string, id: string, env: string) {
 return projectPath(org, project) + '/modules/' + id + (env ? '?' + new URLSearchParams({ env }) : '');
}
export function Modules({ org, project, query }: { org: string; project: string; query: string }) {
 const [search, setSearch] = useSearchParams();
 const navigate = useNavigate();
 const filters = new URLSearchParams(query);
 const term = search.get('query') ?? '';
 if (search.get('before')) filters.set('before', search.get('before')!);
 if (term) filters.set('query', term);
 if (search.get('include_archived') === 'true') filters.set('include_archived', 'true');
 const read = useRead('/organizations/' + org + '/modules?' + filters, 'ModulesOutputBody', 10000);
 const [denied, setDenied] = useState(false);
 if (denied) return <p role="alert">Catalog access changed. Reopen this project to check current access.</p>;
 if (!read.data) return <ReadState error={read.error} retry={read.reload}/>;
 const data = read.data, env = search.get('env') ?? '';
 function filter(e: FormEvent<HTMLFormElement>) {
  e.preventDefault(); const f = new FormData(e.currentTarget), next = new URLSearchParams(search);
  const value = String(f.get('query') ?? '').trim();
  if (value) next.set('query', value); else next.delete('query');
  if (f.get('archived')) next.set('include_archived', 'true'); else next.delete('include_archived');
  next.delete('before'); setSearch(next);
 }
 return <><section className="panel"><h2>Module catalog</h2><p>Find an exact source revision and its usage instructions. Repository access is granted separately.</p>
 <form className="catalog-search" onSubmit={filter}><label>Search modules<input name="query" defaultValue={term} maxLength={100}/></label>{data.can_register && <label className="checkbox"><input type="checkbox" name="archived" defaultChecked={search.get('include_archived') === 'true'}/>Include archived entries</label>}<button type="submit">Search</button></form></section>
 {!data.modules.length ? <section className="empty-state"><h2>No visible modules</h2><p>Registered modules in your current scope will appear here.</p></section> : <div className="stack-grid">{data.modules.map(m => <article className="panel" key={m.id}><h2><Link to={modulePath(org, project, m.id, env)}>{m.name}</Link></h2><p>{m.project_id ? 'Project catalog' : 'Shared organization catalog'} · {m.available ? 'Registered source' : m.archived ? 'Archived entry' : 'Source unavailable'}</p><p className="catalog-code">{m.repository_url}<br/><code>{m.commit}</code><br/>{m.subdirectory}</p></article>)}</div>}
 {data.next_cursor && <button onClick={() => {const next = new URLSearchParams(search); next.set('before', data.next_cursor!); setSearch(next);}}>More modules</button>}
 {search.get('before') && <Link to={projectPath(org, project) + '?' + new URLSearchParams({ view: 'modules', ...(env ? { env } : {}), ...(term ? { query: term } : {}) })}>First modules</Link>}
 {data.can_register && <details className="panel"><summary>Register a module</summary><ModuleForm org={org} project={project} repositories={data.repositories} shared={data.can_register_shared} onDenied={() => setDenied(true)} onDone={id => navigate(modulePath(org, project, id, env))}/></details>}</>;
}
export default function ModulePage() {
 return <Workspace><ProjectContext>{c => <ModuleDetail key={c.project + '/' + c.environment} {...c}/>}</ProjectContext></Workspace>;
}
function ModuleDetail({ org, project, environment }: { org: string; project: string; environment: string }) {
 const { module = '' } = useParams();
 const read = useRead('/organizations/' + org + '/modules/' + module, 'ModulePublic', 10000);
 const options = useRead('/organizations/' + org + '/modules?project_id=' + project, 'ModulesOutputBody', 10000);
 const [denied, setDenied] = useState(false);
 if (denied) return <p role="alert">Catalog access changed. Reopen this project to check current access.</p>;
 if (!read.data) return <ReadState error={read.error} retry={read.reload}/>;
 const m = read.data;
 if (m.project_id && m.project_id !== project) return <p role="alert">This module is outside the selected project.</p>;
 return <><nav className="breadcrumbs" aria-label="Module context"><Link to={projectPath(org,project) + '?' + new URLSearchParams({view:'modules',...(environment ? {env:environment} : {})})}>Modules</Link><span>{m.name}</span></nav>
 <div className="page-heading"><div><span className="eyebrow">MODULE CATALOG</span><h1>{m.name}</h1><p>{m.project_id ? 'Project catalog' : 'Shared organization catalog'}</p></div><span>{m.archived ? 'Archived' : m.available ? 'Registered' : 'Unavailable'}</span></div>
 <section className="panel catalog-code"><h2>Exact source revision</h2><p>{m.repository_url}</p><p><code>{m.commit}</code> · {m.subdirectory}</p><a href={m.source_url} target="_blank" rel="noopener noreferrer">Open source at this commit</a><p>Registration records a source revision. Consumption still requires approved repository access and source validation.</p></section>
 {m.available ? <ModuleUsage key={m.id + '/' + m.revision} org={org} module={m}/> : <p className="notice">Usage is unavailable while this entry or its repository connection is disabled.</p>}
 {m.can_manage && options.data && <details className="panel"><summary>Edit catalog entry</summary><ModuleForm key={m.id + '/' + m.revision} org={org} project={project} repositories={options.data.repositories} module={m} onDenied={() => setDenied(true)} onDone={read.reload}/></details>}</>;
}
export function ModuleUsage({ org, module: m }: { org: string; module: Schema['ModulePublic'] }) {
 const read = useRead('/organizations/' + org + '/modules/' + m.id + '/usage-instructions?expected_revision=' + m.revision, 'ModulePublic', 10000);
 if (!read.data) return <ReadState error={read.error} retry={read.reload}/>;
 if (read.data.id !== m.id || read.data.revision !== m.revision || read.data.commit !== m.commit) return <p role="alert">The catalog revision changed. Refresh before copying the source.</p>;
 const data = read.data;
 return <section className="panel"><h2>Usage at this revision</h2><label>Commit-pinned module snippet<textarea className="catalog-snippet" readOnly rows={4} value={data.snippet}/></label><p className="evidence-statement">{data.instructions}</p><h3>Examples at this commit</h3>{data.examples.length ? <ul>{data.examples.map(x => <li key={x.path}><a href={x.url} target="_blank" rel="noopener noreferrer">{x.path}</a></li>)}</ul> : <p>No examples registered.</p>}</section>;
}
export function ModuleForm({ org, project, repositories, shared, module: m, onDone, onDenied }: {
 org: string; project: string; repositories: Schema['ModuleRepository'][]; shared?: boolean; module?: Schema['ModulePublic']; onDone: (id: string) => void; onDenied: () => void;
}) {
 const [busy,setBusy] = useState(false), [error,setError] = useState('');
 const request = useRef<AbortController|null>(null); useEffect(()=>()=>request.current?.abort(),[]);
 async function submit(e: FormEvent<HTMLFormElement>) {
  e.preventDefault(); if (busy) return;
  const f = new FormData(e.currentTarget), examples = String(f.get('examples') ?? '').split('\n').map(s=>s.trim()).filter(Boolean);
  const controller = new AbortController(); request.current = controller; setBusy(true);setError('');
  try {
   const body = { name: f.get('name'), repository_id: f.get('repository'), commit: f.get('commit'), subdirectory: f.get('directory'), instructions: f.get('instructions'), example_paths: examples, archived: !!f.get('archived'), ...(m ? {expected_revision:m.revision} : f.get('scope') === 'shared' ? {} : {project_id:project}) };
   const result = await api('/organizations/' + org + '/modules' + (m ? '/' + m.id : ''), 'IDResultBody', {method:m ? 'PATCH' : 'POST',body,signal:controller.signal});
   if(!controller.signal.aborted) onDone(result.id);
  } catch(e) {
   if(controller.signal.aborted) return;
   if(e instanceof APIError && [401,403,404].includes(e.status)) onDenied();
   else setError(e instanceof Error ? e.message : 'Catalog update failed. Refresh before explicitly trying again.');
  } finally {if(!controller.signal.aborted) setBusy(false);}
 }
 return <form className="recovery-form" onSubmit={submit}><p>Use plain, non-secret instructions and repository-relative example paths.</p>
 {!m && shared && <label>Catalog scope<select name="scope"><option value="project">This project</option><option value="shared">Shared across the organization</option></select></label>}
 <label>Module name<input name="name" required maxLength={100} defaultValue={m?.name}/></label>
 <label>Registered repository<select name="repository" required defaultValue={m?.repository_id ?? ''}><option value="">Choose a repository</option>{m && !repositories.some(r=>r.id===m.repository_id) && <option value={m.repository_id}>{m.repository_url} (current source)</option>}{repositories.map(r=><option key={r.id} value={r.id}>{r.url}</option>)}</select></label>
 <label>Full commit<input name="commit" required pattern="[0-9a-f]{40}" maxLength={40} defaultValue={m?.commit}/></label>
 <label>Module directory<input name="directory" required maxLength={4096} defaultValue={m?.subdirectory ?? '.'}/></label>
 <label>Usage instructions<textarea name="instructions" required maxLength={16000} defaultValue={m?.instructions}/></label>
 <label>Example paths (one per line, up to 16)<textarea name="examples" maxLength={16000} defaultValue={m?.example_paths.join('\n')}/></label>
 <label className="checkbox"><input name="archived" type="checkbox" defaultChecked={m?.archived}/>Archive this entry</label>
 {error && <p className="error" role="alert">{error}</p>}<button type="submit" disabled={busy || !m && repositories.length === 0}>{busy ? 'Saving…' : m ? 'Save catalog revision' : 'Register module'}</button></form>;
}
