import { useCallback, useEffect, useRef, useState } from 'react';
import { api, type Schema } from '../lib/api';

const outcomes = ['eligible', 'protected', 'unresolved', 'pending', 'failed', 'completed', 'attention'];
export default function Retention({ org }: { org: string }) {
    const [settings, setSettings] = useState<Schema['OperationalSettings'] | null>(null);
    const [items, setItems] = useState<Schema['CleanupItem'][]>([]), [next, setNext] = useState(''), [filter, setFilter] = useState('');
    const [error, setError] = useState(''), [notice, setNotice] = useState(''), [busy, setBusy] = useState(false), [limit, setLimit] = useState(100);
    const read = useRef<AbortController | null>(null), write = useRef<AbortController | null>(null);
    const load = useCallback(async (cursor = '') => {
        read.current?.abort(); const controller = new AbortController(); read.current = controller;
        setError('');
        try {
            const [current, preview] = await Promise.all([
                api('/admin/settings', 'OperationalSettings', { signal: controller.signal }),
                api(`/admin/cleanup-preview?${new URLSearchParams({ org_id: org, ...(filter ? { outcome: filter } : {}), ...(cursor ? { cursor } : {}) })}`, 'CleanupPage', { signal: controller.signal }),
            ]);
            if (!controller.signal.aborted) { setSettings(current); setItems(preview.items); setNext(preview.next_cursor); setLimit(n => Math.min(n, current.cleanup.batch)); }
        } catch (e) { if (!controller.signal.aborted) { setSettings(null); setItems([]); setNext(''); setError(e instanceof Error ? e.message : 'Cleanup preview is unavailable.'); } }
    }, [org, filter]);
    useEffect(() => { void load(); return () => { read.current?.abort(); write.current?.abort(); }; }, [load]);
    async function execute() {
        if (busy) return;
        const controller = new AbortController(); write.current = controller;
        setBusy(true); setError(''); setNotice('');
        try {
            const result = await api('/admin/cleanup', 'CleanupAcceptedBody', { method: 'POST', signal: controller.signal, body: { org_id: org, limit } });
            if (!controller.signal.aborted) { setNotice(`${result.claim_ids.length} cleanup claims selected. Review pending, failed and completed outcomes below.`); await load(); }
        } catch (e) { if (!controller.signal.aborted) setError(e instanceof Error ? e.message : 'Cleanup could not finish. Refresh to inspect durable outcomes before another action.'); }
        finally { if (!controller.signal.aborted) setBusy(false); }
    }
    return <>
        <section className="panel"><h2>Artifact retention</h2><p>Test backup protection and restoration before enabling automatic cleanup. Settings are configured by the host operator and are read-only here.</p>
            {settings && <><dl><dt>Automatic cleanup</dt><dd>{settings.cleanup.enabled ? 'Enabled' : 'Disabled'}</dd><dt>Cleanup interval</dt><dd>{settings.cleanup.interval_seconds} seconds</dd><dt>Terminal retention</dt><dd>{settings.cleanup.retention_days} days</dd><dt>Maximum batch</dt><dd>{settings.cleanup.batch} artifacts</dd><dt>Maintenance</dt><dd>{settings.maintenance || settings.offline ? 'Active · cleanup paused' : 'Inactive'}</dd><dt>Artifact storage</dt><dd>{settings.storage_configured ? 'Configured' : 'Unavailable'}</dd></dl><details><summary>Execution limits</summary><p>Plan: {settings.plan_seconds} seconds · Apply: {settings.apply_seconds} seconds · Source check: {settings.preflight_seconds} seconds · Plan lifetime: {settings.plan_ttl_hours} hours</p></details></>}
        </section>
        <section className="panel"><h2>Cleanup preview</h2><p>This preview is for the selected organization. Usable plans, live attempts, recovery and declared backups protect evidence. Uploads without an exact version remain unresolved. Metadata and audit history are retained.</p>
            <label>Outcome<select aria-label="Outcome" value={filter} disabled={busy} onChange={e => setFilter(e.target.value)}><option value="">All outcomes</option>{outcomes.map(o => <option key={o} value={o}>{o}</option>)}</select></label>
            <button disabled={busy} onClick={() => void load()}>Refresh preview</button>
            {error && <p role="alert" className="error">{error}</p>}{notice && <p role="status">{notice}</p>}
            {!items.length ? <p>No artifacts match this preview.</p> : <table><thead><tr><th>Artifact</th><th>Kind</th><th>Outcome</th></tr></thead><tbody>{items.map(item => <tr key={item.artifact_id}><td><code>{item.artifact_id}</code></td><td>{item.kind}</td><td>{item.outcome}{item.reason && <small>{item.reason.replaceAll('_', ' ').toLowerCase()}</small>}</td></tr>)}</tbody></table>}
            {next && <button disabled={busy} onClick={() => void load(next)}>Next page</button>}
            <form onSubmit={e => { e.preventDefault(); void execute(); }}><label>Batch size<input aria-label="Batch size" type="number" min={1} max={settings?.cleanup.batch ?? 100} required value={limit} disabled={busy} onChange={e => setLimit(Number(e.target.value))}/></label><p>Delete eligible exact versions and retry due claims, up to this bound. This action rechecks protection and may leave pending claims if storage is unavailable. Failed claims keep their exact identity.</p><button type="submit" disabled={busy || !settings || settings.maintenance || settings.offline || !settings.storage_configured}>{busy ? 'Cleaning…' : 'Run bounded cleanup'}</button></form>
        </section>
    </>;
}
