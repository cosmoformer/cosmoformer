import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { Routes, Route } from 'react-router';
import { beforeEach, expect, it, vi } from 'vitest';
import { renderPage, reply } from './helpers';
import ModulePage, { Modules, ModuleUsage, ModuleForm } from '../../app/routes/modules';
import { StackDependencies } from '../../app/components/stack-dependencies';
import { org, project, env, stack, reviewID, reviewSession, projects } from '../fixtures/review';
import { SessionProvider } from '../../app/lib/session';
import { AppearanceProvider } from '../../app/lib/appearance';
import type { Schema } from '../../app/lib/api';

import { moduleFixture } from '../fixtures/modules';

let data: Schema['ModulesOutputBody'], revoked:boolean;
beforeEach(()=>{
 revoked=false;data={modules:[structuredClone(moduleFixture)],repositories:[],can_register:false,can_register_shared:false,next_cursor:reviewID(61)};
 vi.stubGlobal('fetch',vi.fn(async(p)=>{
  if(revoked) return reply({code:'FORBIDDEN',message:'Catalog access changed.'},403);
  return reply(String(p).includes('usage-instructions')? moduleFixture : data);
 }));
});
function list(){renderPage(<Routes><Route path="/catalog" element={<Modules org={org} project={project} query={'project_id='+project}/>}/></Routes>,'/catalog?env='+env);}
it('filters by search, pages within scope and links to the exact module context',async()=>{
 list();const user=userEvent.setup();
 expect(await screen.findByRole('link',{name:'Synthetic instance'})).toHaveAttribute('href','/o/'+org+'/p/'+project+'/modules/'+moduleFixture.id+'?env='+env);
 await user.type(screen.getByRole('textbox',{name:'Search modules'}),'instance');await user.click(screen.getByRole('button',{name:'Search'}));
 await waitFor(()=>expect(vi.mocked(fetch).mock.calls.some(([p])=>String(p).includes('query=instance'))).toBe(true));
 await user.click(screen.getByRole('button',{name:'More modules'}));
 await waitFor(()=>expect(vi.mocked(fetch).mock.calls.some(([p])=>String(p).includes('before='+reviewID(61)))).toBe(true));
 expect(screen.queryByText('Register a module')).not.toBeInTheDocument();
});
it('clears protected catalog metadata after a denied page request',async()=>{
 list();await screen.findByText('Synthetic instance');revoked=true;
 await userEvent.setup().click(screen.getByRole('button',{name:'More modules'}));
 expect(await screen.findByRole('alert')).toHaveTextContent('Catalog access changed.');
 expect(screen.queryByText('Synthetic instance')).not.toBeInTheDocument();
});
it('shows an empty catalog without borrowing execution rights',async()=>{
 data.modules=[];list();expect(await screen.findByRole('heading',{name:'No visible modules'})).toBeVisible();
 expect(screen.queryByRole('button',{name:'Apply'})).not.toBeInTheDocument();
});
it('renders plain instructions, an exact source snippet and pinned examples',async()=>{
 renderPage(<ModuleUsage org={org} module={moduleFixture}/>);
 expect(await screen.findByRole('textbox',{name:'Commit-pinned module snippet'})).toHaveValue(moduleFixture.snippet);
 expect(screen.getByText(moduleFixture.instructions)).toBeVisible();expect(document.querySelector('img')).toBeNull();
 expect(screen.getByRole('link',{name:'examples/basic'})).toHaveAttribute('href',moduleFixture.examples[0].url);
});
it('blocks a mismatched catalog revision',async()=>{
 vi.mocked(fetch).mockResolvedValue(reply({...moduleFixture,revision:2}));
 renderPage(<ModuleUsage org={org} module={moduleFixture}/>);
 expect(await screen.findByRole('alert')).toHaveTextContent('catalog revision changed');
 expect(screen.queryByRole('textbox')).not.toBeInTheDocument();
});
it('submits an explicit revision edit and surfaces failure without automatic retry',async()=>{
 const done=vi.fn(),denied=vi.fn();let calls=0;
 vi.mocked(fetch).mockImplementation(async()=>{calls++;return reply({code:'CONFLICT',message:'Refresh this catalog revision.'},409)});
 renderPage(<ModuleForm org={org} project={project} repositories={[{id:moduleFixture.repository_id,url:moduleFixture.repository_url}]} module={moduleFixture} onDone={done} onDenied={denied}/>);
 const user=userEvent.setup();await user.click(screen.getByRole('button',{name:'Save catalog revision'}));
 expect(await screen.findByRole('alert')).toHaveTextContent('Refresh this catalog revision.');
 expect(calls).toBe(1);expect(done).not.toHaveBeenCalled();expect(denied).not.toHaveBeenCalled();
 const options=vi.mocked(fetch).mock.calls[0][1] as RequestInit;
 expect(options.method).toBe('PATCH');expect(JSON.parse(options.body as string).expected_revision).toBe(1);
});
it('registers a shared entry only through an explicit administrator form',async()=>{
 const done=vi.fn();vi.mocked(fetch).mockResolvedValue(reply({id:moduleFixture.id}));
 renderPage(<ModuleForm org={org} project={project} repositories={[{id:moduleFixture.repository_id,url:moduleFixture.repository_url}]} shared onDone={done} onDenied={vi.fn()}/>);
 const user=userEvent.setup();await user.selectOptions(screen.getByLabelText('Catalog scope'),'shared');
 await user.type(screen.getByLabelText('Module name'),'Shared example');await user.selectOptions(screen.getByLabelText('Registered repository'),moduleFixture.repository_id);
 await user.type(screen.getByLabelText('Full commit'),moduleFixture.commit);await user.type(screen.getByLabelText('Usage instructions'),'Instructions');
 await user.click(screen.getByRole('button',{name:'Register module'}));await waitFor(()=>expect(done).toHaveBeenCalledWith(moduleFixture.id));
 expect(JSON.parse((vi.mocked(fetch).mock.calls[0][1] as RequestInit).body as string)).not.toHaveProperty('project_id');
});
it.each([0,100])('retains the current source when it is absent from %i replacement options',async(count)=>{
 const done=vi.fn();vi.mocked(fetch).mockResolvedValue(reply({id:moduleFixture.id}));
 const repositories=Array.from({length:count},(_,i)=>({id:reviewID(100+i),url:'https://bitbucket.org/synthetic/repo-'+i+'.git'}));
 renderPage(<ModuleForm org={org} project={project} repositories={repositories} module={{...moduleFixture,available:false}} onDone={done} onDenied={vi.fn()}/>);
 expect(screen.getByRole('combobox',{name:'Registered repository'})).toHaveValue(moduleFixture.repository_id);
 expect(screen.getByRole('button',{name:'Save catalog revision'})).toBeEnabled();
 const user=userEvent.setup();await user.click(screen.getByRole('checkbox',{name:'Archive this entry'}));await user.click(screen.getByRole('button',{name:'Save catalog revision'}));
 await waitFor(()=>expect(done).toHaveBeenCalledWith(moduleFixture.id));
 expect(JSON.parse((vi.mocked(fetch).mock.calls[0][1] as RequestInit).body as string)).toMatchObject({repository_id:moduleFixture.repository_id,commit:moduleFixture.commit,subdirectory:moduleFixture.subdirectory,archived:true,expected_revision:moduleFixture.revision});
});
it('provides dependency links for manual coordination without run controls',async()=>{
 vi.mocked(fetch).mockResolvedValue(reply({stack_id:stack,stack_revision_id:reviewID(62),dependencies:[{id:reviewID(63),name:'Network foundation',project_id:project,environment_id:env,archived:true}]}));
 renderPage(<StackDependencies org={org} stack={stack}/>);
 expect(await screen.findByRole('link',{name:'Network foundation'})).toHaveAttribute('href','/o/'+org+'/p/'+project+'/stacks/'+reviewID(63)+'?env='+env);
 expect(screen.getByText(/do not start or order runs/)).toBeVisible();expect(screen.queryByRole('button')).not.toBeInTheDocument();
});


function detail(){return renderPage(<AppearanceProvider><SessionProvider><Routes><Route path="/o/:org/p/:project/modules/:module" element={<ModulePage/>}/></Routes></SessionProvider></AppearanceProvider>,'/o/'+org+'/p/'+project+'/modules/'+moduleFixture.id+'?env='+env);}
function detailReplies(m:Schema['ModulePublic'],writeStatus=200){
 vi.mocked(fetch).mockImplementation(async(p,options)=>{
  const path=String(p);
  if(options?.method!=='GET')return writeStatus===200?reply({id:m.id}):reply({code:'FORBIDDEN',message:'Current catalog permission was revoked.'},writeStatus);
  if(path.endsWith('/session'))return reply(reviewSession);
  if(path.endsWith('/projects'))return reply(projects);
  if(path.includes('/modules?'))return reply({...data,repositories:[{id:m.repository_id,url:m.repository_url}]});
  return reply(m);
 });
}
it('opens the shared archived catalog context and its administrative edit form',async()=>{
 const m={...moduleFixture,project_id:undefined,available:false,archived:true,can_manage:true};detailReplies(m);detail();
 expect(await screen.findByRole('heading',{name:m.name})).toBeVisible();
 expect(screen.getByText('Shared organization catalog')).toBeVisible();expect(screen.getByText('Archived',{exact:true})).toBeVisible();
 expect(screen.getByText(/Usage is unavailable/)).toBeVisible();expect(screen.queryByRole('textbox',{name:'Commit-pinned module snippet'})).not.toBeInTheDocument();
 await userEvent.setup().click(screen.getByText('Edit catalog entry',{exact:true}));
 expect(screen.getByRole('checkbox',{name:'Archive this entry'})).toBeChecked();
 expect(screen.getByRole('link',{name:'Modules'})).toHaveAttribute('href','/o/'+org+'/p/'+project+'?view=modules&env='+env);
});
it('rejects a detail link in the wrong selected project',async()=>{
 detailReplies({...moduleFixture,project_id:reviewID(99)});detail();
 expect(await screen.findByRole('alert')).toHaveTextContent('outside the selected project');
 expect(screen.queryByText(moduleFixture.instructions)).not.toBeInTheDocument();
});
it('clears detail and the pending editor when current administrative authority is lost',async()=>{
 detailReplies({...moduleFixture,can_manage:true},403);detail();
 await screen.findByRole('heading',{name:moduleFixture.name});const user=userEvent.setup();
 await user.click(screen.getByText('Edit catalog entry',{exact:true}));await user.click(screen.getByRole('button',{name:'Save catalog revision'}));
 expect(await screen.findByRole('alert')).toHaveTextContent('Catalog access changed');
 expect(screen.queryByRole('heading',{name:moduleFixture.name})).not.toBeInTheDocument();
 expect(vi.mocked(fetch).mock.calls.filter(([,o])=>o?.method==='PATCH')).toHaveLength(1);
});
it('keeps an empty example list distinct from unavailable instructions',async()=>{
 vi.mocked(fetch).mockResolvedValue(reply({...moduleFixture,examples:[]}));renderPage(<ModuleUsage org={org} module={moduleFixture}/>);
 expect(await screen.findByText('No examples registered.')).toBeVisible();expect(screen.getByRole('textbox')).toHaveValue(moduleFixture.snippet);
});
