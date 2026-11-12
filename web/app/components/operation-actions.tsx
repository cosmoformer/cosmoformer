import {useEffect,useRef,useState} from 'react';
import {api,APIError,type Schema} from '../lib/api';
export function OperationActions({org,operation:o,reload,onDenied}:{org:string;operation:Schema['OperationPublic'];reload:()=>void;onDenied:()=>void}){
 const [busy,setBusy]=useState(false),[error,setError]=useState('');const request=useRef<AbortController|null>(null);
 useEffect(()=>()=>request.current?.abort(),[]);
 async function change(kind:'retry'|'priority',priority?:string){
  if(busy)return;const controller=new AbortController();request.current=controller;setBusy(true);setError('');
  try{await api(`/organizations/${org}/operations/${o.id}/${kind}`,'OKBody',{method:'POST',signal:controller.signal,body:{expected_operation_revision:o.revision,...(priority?{priority}:{})}});if(!controller.signal.aborted)reload();}
  catch(e){if(!controller.signal.aborted){if(e instanceof APIError && [403,404].includes(e.status))onDenied();else{setError(e instanceof Error?e.message:'Operation request failed.');reload();}}}
  finally{if(!controller.signal.aborted)setBusy(false);}
 }
 return <div>{o.can_retry&&<button disabled={busy} onClick={()=>void change('retry')}>Retry {o.kind} preparation</button>}{o.can_prioritize&&<label>Waiting {o.kind} priority<select disabled={busy} value={o.priority} onChange={e=>void change('priority',e.target.value)}>{['low','medium','high'].map(p=><option key={p}>{p}</option>)}</select></label>}{error&&<p role="alert" className="error">{error}</p>}</div>;
}
