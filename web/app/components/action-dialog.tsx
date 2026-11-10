import { useEffect, useRef, useState, type FormEvent } from 'react';
import { useNavigate } from 'react-router';
import { Button } from '@astryxdesign/core/Button';
import { api, type Schema } from '../lib/api';
import { projectPath } from './workspace';
export function PlanningActions({ org, stack }: { org: string; stack: Schema['StackPublic'] }) {
    const navigate = useNavigate();
    const [intent, setIntent] = useState<'source_check' | 'native_plan' | null>(null), [busy, setBusy] = useState(false), [error, setError] = useState('');
    const dialog = useRef<HTMLDialogElement>(null), request = useRef<AbortController | null>(null);
    useEffect(() => () => request.current?.abort(), []);
    useEffect(() => { if (intent) dialog.current?.showModal(); else dialog.current?.close(); }, [intent]);
    async function submit(e: FormEvent<HTMLFormElement>) {
        e.preventDefault(); if (busy || !intent) return;
        const fields = new FormData(e.currentTarget), controller = new AbortController(); request.current = controller;
        setBusy(true); setError('');
        try {
            const r = await api(`/organizations/${org}/stacks/${stack.id}/plans`, 'ReceiptOutputBody', { method: 'POST', signal: controller.signal, body: { intent, source: fields.get('kind') === 'pr' ? { kind: 'pr', pr_number: Number(fields.get('pr')) } : { kind: 'branch' }, priority: fields.get('priority') } });
            if (!controller.signal.aborted) navigate(`${projectPath(org, stack.project_id)}/runs/${r.subject_id}?env=${stack.environment_id}`);
        } catch (e) { if (!controller.signal.aborted) setError(e instanceof Error ? e.message : 'Request failed.'); }
        finally { if (!controller.signal.aborted) setBusy(false); }
    }
    return <section className="panel"><h2>Start a request</h2><p>Check source validates preparation. Request plan runs the native engine and retains review evidence. The server resolves the current source and checks permissions and execution restrictions.</p><div className="actions"><Button label="Check source" isDisabled={!stack.can_plan} onClick={() => { setError(''); setIntent('source_check'); }}/><Button label="Request plan" variant="primary" isDisabled={!stack.can_native_plan} onClick={() => { setError(''); setIntent('native_plan'); }}/></div>{stack.check_reason && <p className="muted">Check source: {stack.check_reason}</p>}{stack.plan_reason && <p className="muted">Request plan: {stack.plan_reason}</p>}<dialog ref={dialog} aria-labelledby="planning-title" onCancel={e => { if (busy) e.preventDefault(); else setIntent(null); }} onClose={() => setIntent(null)}><form onSubmit={submit}><h2 id="planning-title">{intent === 'native_plan' ? 'Request native plan' : 'Check source'}</h2><p>{stack.name} · {stack.environment_name}</p><p className="muted">The platform resolves the current branch or PR head. A source check cannot become a native plan.</p><SourceFields/><label>Priority<select aria-label="Priority" name="priority" defaultValue="medium"><option value="low">Low</option><option value="medium">Medium</option><option value="high">High</option></select></label>{error && <p className="error" role="alert">{error}</p>}<div className="actions"><Button label={busy ? 'Requesting…' : 'Confirm request'} type="submit" isDisabled={busy}/><button type="button" disabled={busy} onClick={() => setIntent(null)}>Cancel</button></div></form></dialog></section>;
}
function SourceFields() {
    const [kind, setKind] = useState('branch');
    return <><label>Source<select aria-label="Source" name="kind" value={kind} onChange={e => setKind(e.target.value)}><option value="branch">Tracked branch</option><option value="pr">Pull request</option></select></label>{kind === 'pr' && <label>PR number<input name="pr" type="number" min={1} max={9007199254740991} step={1} required/></label>}</>;
}
