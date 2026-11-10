import { useEffect, useRef, useState, type FormEvent } from 'react';
import { api, APIError, type Schema } from '../lib/api';
import { useRead } from '../lib/polling';
import { ReadState, time } from './workspace';

const reasons: Record<string, string> = {
    APPROVAL_AUTHORITY_REQUIRED: 'Current approval permission is required. Administration, planning, deployment and raw evidence permissions do not grant it.',
    PLAN_NOT_READY: 'This plan has no finalized review evidence yet.',
    PLAN_EXPIRED: 'This plan has expired. Request a fresh plan and new approvals.',
    PLAN_INVALIDATED: 'This plan was invalidated. Retained Changes and past votes remain historical.',
    INPUTS_CHANGED: 'Reviewed inputs or approval policy changed. Request a fresh plan.',
    PROFILE_UNSUPPORTED: 'The required runtime or analysis profile is unavailable.',
    EVIDENCE_UNAVAILABLE: 'Exact retained plan evidence is unavailable.',
    STATE_OR_RECOVERY_CHANGED: 'State or recovery context changed. A fresh normal plan is required.',
    RECOVERY_REQUIRED: 'Recovery evidence cannot receive normal plan approval.',
    MAINTENANCE: 'Approvals are blocked during maintenance.',
    CLOCK_UNRELIABLE: 'Trusted server time is unavailable. Approvals are blocked.',
};
export function Approvals({ org, run, onDenied }: { org: string; run: Schema['RunPublic']; onDenied: () => void }) {
    const plan = run.plan!;
    const path = `/organizations/${org}/plans/${plan.id}/approvals`;
    const read = useRead(path, 'ApprovalSummary', 5000);
    const [confirm, setConfirm] = useState(false), [busy, setBusy] = useState(false), [error, setError] = useState(''), [message, setMessage] = useState('');
    const dialog = useRef<HTMLDialogElement>(null), request = useRef<AbortController | null>(null);
    useEffect(() => () => request.current?.abort(), []);
    useEffect(() => { if (read.denied) onDenied(); }, [read.denied, onDenied]);
    const summary = read.data;
    const matches = summary?.plan_id === plan.id && summary.revision === plan.revision && summary.manifest_digest === plan.manifest_digest && summary.source_commit === run.commit;
    const canApprove = matches && !!summary?.can_approve && !summary.own_counted;
    useEffect(() => { if (!canApprove) setConfirm(false); }, [canApprove]);
    useEffect(() => { if (confirm) dialog.current?.showModal(); else dialog.current?.close(); }, [confirm]);
    async function vote(positive: boolean) {
        if (busy || positive && !canApprove || !positive && !summary?.can_withdraw) return;
        const controller = new AbortController(); request.current = controller;
        setBusy(true); setError(''); setMessage('');
        try {
            await api(path + (positive ? '' : '/me'), 'VoteOutputBody', { method: positive ? 'POST' : 'DELETE', signal: controller.signal, body: { expected_plan_revision: plan.revision, ...(positive ? { manifest_digest: plan.manifest_digest } : {}) } });
            if (!controller.signal.aborted) { setConfirm(false); setMessage(positive ? 'Approval recorded for this exact plan.' : 'Your approval was withdrawn.'); read.reload(); }
        } catch (e) {
            if (!controller.signal.aborted) {
                // Close confirmation on all errors. Re-login or a changed grant
                // always requires another explicit action; no mutation replay.
                setConfirm(false);
                if (e instanceof APIError && [403, 404].includes(e.status)) onDenied();
                else { setError(e instanceof Error ? e.message : 'Approval request failed.'); read.reload(); }
            }
        } finally { if (!controller.signal.aborted) setBusy(false); }
    }
    function submit(e: FormEvent) { e.preventDefault(); void vote(true); }
    return <section className="panel" aria-label="Plan approvals"><h2>Plan approvals</h2>
        {!summary ? <ReadState error={read.error} retry={read.reload}/> : !matches ? <p className="error" role="alert">Plan identity changed. Refresh this run before approving.</p> : <>
            <p className="approval-count"><strong>{summary.counted_approvals} of {summary.minimum}</strong> distinct approvals currently counted</p>
            <p>{summary.own_counted ? 'Your approval currently counts.' : summary.own_approved ? 'Your past vote is retained but does not currently count.' : 'You have no current approval on this plan.'}</p>
            <ul>{summary.blocked_reasons.map(code => <li key={code}>{reasons[code] ?? 'Current plan eligibility needs attention. Refresh this run.'}</li>)}</ul>
            <p className="muted">{summary.source_observed_at ? `Last recorded source observation: ${time(summary.source_observed_at)}. ` : ''}Approve performs a new Bitbucket check. Counts reflect current account, membership and approval permission. Apply is not available.</p>
            <div className="actions"><button disabled={!canApprove || busy} onClick={() => { setError(''); setConfirm(true); }}>Approve</button><button disabled={!summary.can_withdraw || busy} onClick={() => void vote(false)}>Withdraw my approval</button></div>
        </>}
        {error && <p className="error" role="alert">{error}</p>}{message && <p role="status">{message}</p>}
        <dialog ref={dialog} aria-labelledby="approval-title" onCancel={e => { if (busy) e.preventDefault(); else setConfirm(false); }} onClose={() => setConfirm(false)}>{confirm && <form onSubmit={submit}><h2 id="approval-title">Approve this exact plan</h2>
            <dl className="approval-context"><div><dt>Stack</dt><dd><span>{run.stack_name}</span><small><code>{run.stack_id}</code></small></dd></div><div><dt>Environment</dt><dd><span>{run.environment_name}</span><small><code>{run.environment_id}</code></small></dd></div><div><dt>Commit</dt><dd><code>{run.commit}</code></dd></div><div><dt>Plan / revision</dt><dd><code>{plan.id}</code> · {plan.revision}</dd></div><div><dt>Manifest</dt><dd><code>{plan.manifest_digest}</code></dd></div><div><dt>Ready / expires</dt><dd>{time(plan.ready_at)}<br/>{time(plan.expires_at)}</dd></div><div><dt>Required approvals</dt><dd>{summary?.minimum}</dd></div></dl>
            <p>This vote binds the reviewed commit and retained plan evidence. The server verifies source, current inputs and your approval permission again.</p>
            <div className="actions"><button type="submit" disabled={busy || !canApprove}>{busy ? 'Checking…' : 'Confirm approval'}</button><button type="button" disabled={busy} onClick={() => setConfirm(false)}>Cancel</button></div>
        </form>}</dialog>
    </section>;
}
