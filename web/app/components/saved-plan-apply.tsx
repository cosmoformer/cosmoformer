import { useEffect, useRef, useState, type FormEvent } from 'react';
import { api, APIError, type Schema } from '../lib/api';
import { useRead } from '../lib/polling';
import { ReadState, time } from './workspace';
const blockers: Record<string,string> = {
 APPROVALS_REQUIRED: 'More current eligible approvals are required. No Apply is queued.',
 APPLY_ALREADY_PENDING: 'An Apply for this exact plan is already pending.',
 NEW_PLAN_REQUIRED: 'This plan has been consumed. Request a new normal plan and approvals.',
 PR_APPLY_DISABLED: 'This stack does not allow Apply from an open pull request.',
 PLAN_EXPIRED: 'This plan has expired. Request a new plan.', PLAN_INVALIDATED: 'This plan was invalidated. Request a new plan.',
 EVIDENCE_UNAVAILABLE: 'Exact retained evidence is unavailable.', INPUTS_CHANGED: 'Reviewed inputs or policy changed.',
 STATE_OR_RECOVERY_CHANGED: 'State or recovery context changed. Request a fresh normal plan after recovery.',
 CLOCK_UNRELIABLE: 'Trusted server time is unavailable.', MAINTENANCE: 'Apply is blocked during maintenance.',
 RECOVERY_REQUIRED: 'Recovery evidence cannot be deployed.', PROFILE_UNSUPPORTED: 'The reviewed runtime or profile is unavailable.',
};
export function SavedPlanApply({org,run,onDenied,reload}:{org:string;run:Schema['RunPublic'];onDenied:()=>void;reload:()=>void}) {
 const plan=run.plan!;
 const read=useRead(`/organizations/${org}/plans/${plan.id}/approvals`,'ApprovalSummary',5000);
 const [confirm,setConfirm]=useState(false),[busy,setBusy]=useState(false),[priority,setPriority]=useState('medium'),[error,setError]=useState(''),[message,setMessage]=useState('');
 const dialog=useRef<HTMLDialogElement>(null),request=useRef<AbortController|null>(null);
 const summary=read.data;
 const matches=summary?.plan_id===plan.id && summary.revision===plan.revision && summary.manifest_digest===plan.manifest_digest && summary.source_commit===run.commit;
 const canApply=matches && !!summary?.can_apply;
 const hasAuthority=summary?.apply_blocked_reasons && !summary.apply_blocked_reasons.includes('APPLY_AUTHORITY_REQUIRED') || canApply;
 useEffect(()=>()=>request.current?.abort(),[]);
 useEffect(()=>{if(read.denied)onDenied();},[read.denied,onDenied]);
 useEffect(()=>{if(!canApply)setConfirm(false);},[canApply]);
 useEffect(()=>{if(confirm)dialog.current?.showModal();else dialog.current?.close();},[confirm]);
 async function submit(e:FormEvent){
  e.preventDefault();if(busy||!canApply)return;
  const controller=new AbortController();request.current=controller;setBusy(true);setError('');setMessage('');
  try{
   await api(`/organizations/${org}/plans/${plan.id}/apply`,'ApplyOutputBody',{method:'POST',signal:controller.signal,body:{expected_plan_revision:plan.revision,manifest_digest:plan.manifest_digest,priority,confirmed_context:{stack_id:run.stack_id,environment_id:run.environment_id,source_commit:run.commit}}});
   if(!controller.signal.aborted){setConfirm(false);setMessage('Apply accepted for this exact saved plan.');read.reload();reload();}
  }catch(e){if(!controller.signal.aborted){setConfirm(false);if(e instanceof APIError && [403,404].includes(e.status))onDenied();else{setError(e instanceof Error?e.message:'Apply request failed. Check operation history before explicitly trying again.');read.reload();reload();}}}
  finally{if(!controller.signal.aborted)setBusy(false);}
 }
 if(!summary)return <ReadState error={read.error} retry={read.reload}/>;
 if(!hasAuthority)return null;
 return <section className="panel" aria-label="Saved-plan Apply"><h2>Saved-plan Apply</h2>
 <p>Platform checks cover exact source, reviewed inputs, expiry, approvals, retained evidence and recovery. The native engine validates state lineage and serial under its state lock when it applies the saved binary.</p>
 <p>{summary.counted_approvals} of {summary.minimum} current approvals · Expires {time(plan.expires_at)}</p>
 <ul>{summary.apply_blocked_reasons?.map(code=><li key={code}>{blockers[code]??'Current plan eligibility needs attention.'}</li>)}</ul>
 <button disabled={!canApply||busy} onClick={()=>{setError('');setPriority('medium');setConfirm(true);}}>Apply</button>
 {error&&<p role="alert" className="error">{error}</p>}{message&&<p role="status">{message}</p>}
 <dialog ref={dialog} aria-labelledby="apply-title" onCancel={e=>{if(busy)e.preventDefault();else setConfirm(false);}} onClose={()=>setConfirm(false)}>{confirm&&<form onSubmit={submit}><h2 id="apply-title">Apply this exact saved plan</h2>
 <dl className="approval-context"><div><dt>Stack</dt><dd>{run.stack_name}<small><code>{run.stack_id}</code></small></dd></div><div><dt>Environment</dt><dd>{run.environment_name}<small><code>{run.environment_id}</code></small></dd></div><div><dt>Source</dt><dd>{run.source_kind} {run.source_key}<br/><code>{run.commit}</code></dd></div><div><dt>Plan / revision</dt><dd><code>{plan.id}</code> · {plan.revision}</dd></div><div><dt>Manifest</dt><dd><code>{plan.manifest_digest}</code></dd></div><div><dt>Expires</dt><dd>{time(plan.expires_at)}</dd></div><div><dt>Current approvals</dt><dd>{summary.counted_approvals} of {summary.minimum}</dd></div></dl>
 <label>Apply priority<select value={priority} onChange={e=>setPriority(e.target.value)}><option value="low">Low</option><option value="medium">Medium</option><option value="high">High</option></select></label>
 <p>This action may change infrastructure. The server performs a new Bitbucket observation and repeats eligibility checks before accepting work and issuing command authority.</p>
 <div className="actions"><button type="submit" disabled={!canApply||busy}>{busy?'Checking…':'Confirm Apply'}</button><button type="button" disabled={busy} onClick={()=>setConfirm(false)}>Cancel</button></div>
 </form>}</dialog></section>;
}
