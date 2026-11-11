import { useEffect, useRef, useState, type FormEvent } from 'react';
import { Link } from 'react-router';
import { projectPath } from './workspace';
import { api, APIError, type Schema } from '../lib/api';

type Props = { org: string; run: Schema['RunPublic']; attempts: Schema['AttemptPublic'][]; recovery?: Schema['RecoveryPublic']; reload: () => void; onDenied: () => void };
export function ExecutionControls({ org, run, attempts, recovery, reload, onDenied }: Props) {
    const [intent, setIntent] = useState<{ attempt: Schema['AttemptPublic']; kind: 'cancel' | 'force-stop' | 'recovery' } | null>(null);
    const [busy, setBusy] = useState(false), [error, setError] = useState(''), [message, setMessage] = useState('');
    const dialog = useRef<HTMLDialogElement>(null), request = useRef<AbortController | null>(null);
    useEffect(() => () => request.current?.abort(), []);
    useEffect(() => { if (intent) dialog.current?.showModal(); else dialog.current?.close(); }, [intent]);
    useEffect(() => { if (intent && !attempts.some(a => a.id === intent.attempt.id && a.can_control && a.revision === intent.attempt.revision)) setIntent(null); }, [attempts, intent]);
    async function control(e: FormEvent) {
        e.preventDefault(); if (!intent || busy) return;
        const controller = new AbortController(); request.current = controller;
        setBusy(true); setError(''); setMessage('');
        try {
            const path = `/organizations/${org}/attempts/${intent.attempt.id}/${intent.kind}`;
            const body = { expected_attempt_revision: intent.attempt.revision, ...(intent.kind === 'force-stop' ? { confirm: true, confirmed_context: { stack_id: run.stack_id, environment_id: run.environment_id, attempt_id: intent.attempt.id } } : {}) };
            if (intent.kind === 'recovery') await api(path, 'OKBody', { method: 'POST', body, signal: controller.signal });
            else await api(path, 'ControlOutputBody', { method: 'POST', body, signal: controller.signal });
            if (!controller.signal.aborted) { setIntent(null); setMessage('Request recorded. Delivery and verified stop are shown separately below.'); reload(); }
        } catch (e) { if (!controller.signal.aborted) { setIntent(null); if (e instanceof APIError && [401, 403, 404].includes(e.status)) onDenied(); else { setError(e instanceof Error ? e.message : 'Request failed. Refresh before taking another action.'); reload(); } } }
        finally { if (!controller.signal.aborted) setBusy(false); }
    }
    const active = attempts.filter(a => !a.closed_at);
    if (!active.length && !recovery && !attempts.some(a => a.controls?.length)) return null;
    return <><section className="panel" aria-label="Execution controls"><h2>Execution controls</h2>
        <p>Cancel requests one graceful interrupt to the direct native parent. Force Stop separately requests a process group kill. A signal, elapsed deadline or missing contact does not prove stop or rollback.</p>
        {attempts.filter(a => !a.closed_at || a.controls?.length).map(a => <div className="control-attempt" key={a.id}><h3>{a.phase} · {a.outcome || a.state}</h3><p><code>{a.id}</code> · command / preparation {a.command_ordinal ?? 0}</p>
            {a.contact_warning && <p className="notice">Contact missing — showing last known facts. Ownership is retained; missing contact alone does not open recovery.</p>}
            <p>{a.closed_at ? 'Execution positively closed.' : a.state === 'uncertain' ? 'Stop is unverified. Ownership remains reserved.' : 'No final stop proof accepted yet.'}</p>
            {a.controls?.map(c => <p key={c.control_id}>{c.kind.replaceAll('_', ' ')} · {c.state} · ordinal {c.command_ordinal}{c.signal_sent ? ` · ${c.signal_sent} delivered` : ' · no signal delivery recorded'}. Observed does not mean stopped.</p>)}
            {a.can_control && <div className="actions"><button disabled={busy} onClick={() => setIntent({ attempt: a, kind: 'cancel' })}>Cancel execution</button><button disabled={busy} onClick={() => setIntent({ attempt: a, kind: 'force-stop' })}>Force Stop</button>{a.state !== 'uncertain' && <button disabled={busy} onClick={() => setIntent({ attempt: a, kind: 'recovery' })}>Escalate to recovery</button>}</div>}
            {a.can_control && a.state === 'uncertain' && (a.phase === 'preflight' || recovery?.origin_attempt_id !== a.id) && <AttemptProof org={org} attempt={a} reload={reload} onDenied={onDenied}/>}
        </div>)}
        {error && <p className="error" role="alert">{error}</p>}{message && <p role="status">{message}</p>}
        <dialog ref={dialog} aria-labelledby="control-title" onCancel={e => { if (busy) e.preventDefault(); else setIntent(null); }} onClose={() => setIntent(null)}>{intent && <form onSubmit={control}>
            <h2 id="control-title">{intent.kind === 'force-stop' ? 'Confirm Force Stop' : intent.kind === 'recovery' ? 'Open formal recovery' : 'Cancel this execution'}</h2>
            <dl className="approval-context"><div><dt>Stack</dt><dd>{run.stack_name}<small><code>{run.stack_id}</code></small></dd></div><div><dt>Environment</dt><dd>{run.environment_name}<small><code>{run.environment_id}</code></small></dd></div><div><dt>Exact attempt / revision</dt><dd><code>{intent.attempt.id}</code> · {intent.attempt.revision}</dd></div><div><dt>Command / preparation ordinal</dt><dd>{intent.attempt.command_ordinal ?? 0}</dd></div></dl>
            <p>{intent.kind === 'force-stop' ? 'SIGKILL targets only the recorded process group. Infrastructure may be partially changed. Independent stop proof and reconciliation may be required.' : intent.kind === 'recovery' ? 'Formal recovery retains ownership until independent stop proof. Clearance then needs reconciliation and current distinct approvals.' : 'One SIGINT is requested for the recorded direct parent. Cancellation prevents the next command from starting.'}</p>
            <div className="actions"><button type="submit" disabled={busy}>{busy ? 'Requesting…' : intent.kind === 'force-stop' ? 'Confirm Force Stop for this attempt' : 'Confirm request'}</button><button type="button" disabled={busy} onClick={() => setIntent(null)}>Go back</button></div>
        </form>}</dialog>
    </section>{recovery && <Recovery key={`${recovery.id}/${recovery.revision}/${recovery.evidence_revision}`} org={org} run={run} recovery={recovery} reload={reload} onDenied={onDenied}/>}</>;
}
function Recovery({ org, run, recovery: r, reload, onDenied }: Omit<Props, 'attempts'> & { recovery: Schema['RecoveryPublic'] }) {
    const [diagnostic, setDiagnostic] = useState('');
    const [busy, setBusy] = useState(false), [error, setError] = useState(''), [action, setAction] = useState<'approve' | 'clear' | 'plan' | null>(null);
    const request = useRef<AbortController | null>(null), dialog = useRef<HTMLDialogElement>(null);
    useEffect(() => () => request.current?.abort(), []);
    useEffect(() => { if (action) dialog.current?.showModal(); else dialog.current?.close(); }, [action]);
    async function mutate(kind: 'evidence' | 'approve' | 'clear' | 'plan', body: unknown) {
        if (busy) return;
        const controller = new AbortController(); request.current = controller; setBusy(true); setError('');
        try {
            const path = `/organizations/${org}/recoveries/${r.id}`;
            if (kind === 'plan') { const receipt = await api(`/organizations/${org}/stacks/${r.stack_id}/plans`, 'ReceiptOutputBody', { method: 'POST', body: { intent: 'recovery_plan', recovery_case_id: r.id, expected_evidence_revision: r.evidence_revision, source: { kind: 'branch' }, priority: 'medium' }, signal: controller.signal }); if (!controller.signal.aborted) setDiagnostic(receipt.subject_id); }
            else await api(path + (kind === 'approve' ? '/approvals' : '/' + kind), 'RecoveryPublic', { method: kind === 'evidence' ? 'PATCH' : 'POST', body, signal: controller.signal });
            if (!controller.signal.aborted) { setAction(null); reload(); }
        } catch (e) { if (!controller.signal.aborted) { setAction(null); if (e instanceof APIError && [401, 403, 404].includes(e.status)) onDenied(); else { setError(e instanceof Error ? e.message : 'Recovery request failed.'); reload(); } } }
        finally { if (!controller.signal.aborted) setBusy(false); }
    }
    function evidence(e: FormEvent<HTMLFormElement>) {
        e.preventDefault(); const f = new FormData(e.currentTarget);
        void mutate('evidence', { expected_revision: r.revision, stop_proof: { method: f.get('method'), statement: f.get('stop'), protected_reference: f.get('reference') }, reconciliation: { complete: f.get('complete') === 'on', statement: f.get('reconciliation'), protected_references: String(f.get('references') ?? '').split('\n').map(x => x.trim()).filter(Boolean) } });
    }
    return <section className="panel" aria-label="Recovery case"><h2>Recovery · {r.status}</h2><p><code>{r.id}</code> · evidence revision {r.evidence_revision}</p>
        <dl className="outcome-grid"><div><dt>Stop verification</dt><dd>{r.stop_verified ? 'Verified' : 'Unverified'}</dd></div><div><dt>Stop authority</dt><dd>{r.stop_authority.replaceAll('_', ' ')}</dd></div><div><dt>Reconciliation</dt><dd>{r.reconciliation_complete ? 'Complete' : 'Incomplete'}</dd></div><div><dt>Infrastructure outcome</dt><dd>{r.infrastructure_outcome}</dd></div></dl>
        <p>{r.counted_approvals} of {r.minimum} distinct eligible recovery approvals. {r.own_counted ? 'Your vote counts.' : ''} Evidence edits invalidate counting votes. Recovery approval and clearance do not read Bitbucket.</p>
        {r.evidence?.stop_proof.statement && <><h3>Stop statement</h3><p className="evidence-statement">{r.evidence.stop_proof.statement}</p><h3>Reconciliation statement</h3><p className="evidence-statement">{r.evidence.reconciliation.statement}</p>{r.evidence.stop_proof.protected_reference && <p>Protected stop reference: <code>{r.evidence.stop_proof.protected_reference}</code></p>}{r.evidence.reconciliation.protected_references?.map(ref => <p key={ref}>Protected reference: <code>{ref}</code></p>)}</>}
        {r.can_submit_evidence && <details><summary>Submit or revise recovery evidence</summary><form onSubmit={evidence} className="recovery-form"><p>Use bounded, non-secret statements. Keep raw evidence in protected records. Host and operator attestations are manual authority; agent proof must match accepted process facts.</p>
            <label>Stop authority<select name="method" defaultValue="operator"><option value="operator">Operator attestation (manual)</option><option value="host">Host attestation (manual)</option><option value="agent">Accepted agent protocol proof</option></select></label>
            <label>Stop statement<textarea name="stop" required maxLength={2048} defaultValue={r.evidence?.stop_proof.statement}/></label>
            <label>Protected stop reference<input name="reference" required maxLength={512} placeholder="Protected record ID or credential-free HTTPS location"/></label><p className="muted">Agent proof reference must be the exact originating attempt ID: <code>{r.origin_attempt_id}</code></p>
            <label>Reconciliation statement<textarea name="reconciliation" required maxLength={2048} defaultValue={r.evidence?.reconciliation.statement}/></label>
            <label>Protected reconciliation references (one per line, up to 16)<textarea name="references" maxLength={8208}/></label>
            <label className="checkbox"><input type="checkbox" name="complete"/>Infrastructure and state reconciliation is complete</label><button type="submit" disabled={busy}>Save evidence revision</button>
        </form></details>}
        <div className="actions"><button disabled={!r.can_approve || r.own_counted || busy} onClick={() => setAction('approve')}>Approve recovery evidence</button><button disabled={!r.can_clear || busy} onClick={() => setAction('clear')}>Clear recovery</button><button disabled={!r.can_plan || busy} onClick={() => setAction('plan')}>Request diagnostic plan</button></div>
        {diagnostic && <p role="status">Diagnostic request accepted. <Link to={`${projectPath(org,run.project_id)}/runs/${diagnostic}?env=${run.environment_id}`}>Open diagnostic run</Link></p>}
        <p className="muted">Diagnostic plans are recovery-only and never deployable. After clearance, request a fresh normal plan and new normal approvals. Apply remains unavailable.</p>
        {error && <p role="alert" className="error">{error}</p>}
        <dialog ref={dialog} aria-labelledby="recovery-action-title" onCancel={e => { if (busy) e.preventDefault(); else setAction(null); }} onClose={() => setAction(null)}>{action && <form onSubmit={e => { e.preventDefault(); void mutate(action, { expected_evidence_revision: r.evidence_revision }); }}><h2 id="recovery-action-title">{action === 'approve' ? 'Approve this recovery evidence' : action === 'clear' ? 'Clear this recovery case' : 'Request a diagnostic plan'}</h2><p>{run.stack_name} · {run.environment_name}</p><p>Case <code>{r.id}</code> · evidence revision {r.evidence_revision}</p><p>{action === 'plan' ? 'The platform verifies the current tracked branch. The resulting plan retains this case and evidence revision and cannot authorize deployment.' : action === 'clear' ? 'Clearance requires independently verified stop, complete reconciliation and the current minimum eligible approvals. A new normal plan and approvals are required afterward.' : 'Your vote binds only this exact evidence revision. An eligible self-approval counts once.'}</p><div className="actions"><button type="submit" disabled={busy}>{busy ? 'Requesting…' : 'Confirm recovery action'}</button><button type="button" disabled={busy} onClick={() => setAction(null)}>Go back</button></div></form>}</dialog>
    </section>;
}

function AttemptProof({org,attempt:a,reload,onDenied}:{org:string;attempt:Schema['AttemptPublic'];reload:()=>void;onDenied:()=>void}){
 const [busy,setBusy]=useState(false),[error,setError]=useState('');const request=useRef<AbortController|null>(null);
 useEffect(()=>()=>request.current?.abort(),[]);
 async function submit(e:FormEvent<HTMLFormElement>){
  e.preventDefault();if(busy)return;const f=new FormData(e.currentTarget),controller=new AbortController();request.current=controller;setBusy(true);setError('');
  try{await api(`/organizations/${org}/attempts/${a.id}/${a.phase==='preflight'?'source-stop':'stop-proof'}`,'OKBody',{method:'PATCH',signal:controller.signal,body:{expected_revision:a.revision,stop_proof:{method:f.get('method'),statement:f.get('statement'),protected_reference:f.get('reference')},reconciliation:{complete:false,statement:a.phase==='preflight'?'Source-only preparation; no backend recovery case.':'Attempt stop only; case reconciliation remains separate.',protected_references:[]}}});if(!controller.signal.aborted)reload();}
  catch(e){if(!controller.signal.aborted){if(e instanceof APIError && [401,403,404].includes(e.status))onDenied();else setError(e instanceof Error?e.message:'Stop proof submission failed.');}}
  finally{if(!controller.signal.aborted)setBusy(false);}
 }
 return <details><summary>Submit independent {a.phase==='preflight'?'source process':'attempt'} stop proof</summary><form className="recovery-form" onSubmit={submit}><p>This proof accounts only for attempt <code>{a.id}</code>. {a.phase==='preflight'?'Source preparation has no backend case. After proof, request a new plan.':'The recovery case and its reconciliation approvals remain separate.'}</p><label>Independent stop authority<select name="method"><option value="operator">Operator attestation (manual)</option><option value="host">Host attestation (manual)</option>{a.phase==='execution'&&<option value="agent">Accepted agent protocol proof</option>}</select></label><label>Independent stop statement<textarea name="statement" required maxLength={2048}/></label><label>Protected independent stop reference<input name="reference" required maxLength={512}/></label><button type="submit" disabled={busy}>Record independent stop proof</button>{error&&<p className="error" role="alert">{error}</p>}</form></details>;
}
