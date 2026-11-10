import { useEffect, useRef, useState } from 'react';
import { api, APIError, type Schema } from '../lib/api';
import { usePollingAvailability } from '../lib/polling';
export const LOG_WINDOW = 524288, LOG_SEGMENTS = 500;
type Segment = Schema['Segment'];
export function appendWindow(previous: Segment[], incoming: Segment[]) {
    const all = [...previous, ...incoming];
    let size = all.reduce((n, s) => n + ('text' in s ? s.text.length : 0), 0), removed = false;
    while (all.length > LOG_SEGMENTS || size > LOG_WINDOW) { const removedSegment=all.shift()!; size -= 'text' in removedSegment ? removedSegment.text.length : 0; removed = true; }
    return { segments: all, removed };
}
export function LogView({ org, run, attempt, onDenied }: { org: string; run: string; attempt: string; onDenied: () => void }) {
    const [following, setFollowing] = useState(true), [segments, setSegments] = useState<Segment[]>([]), [limited, setLimited] = useState(false), [error, setError] = useState(''), [complete, setComplete] = useState(false), [more, setMore] = useState(false), [reset, setReset] = useState(''), [reload, setReload] = useState(0);
    const cursor = useRef(''), consoleWindow = useRef<HTMLDivElement>(null);
    const availability = usePollingAvailability();
    useEffect(() => {
        if (!following || !availability.active || availability.controller.signal.aborted) return;
        const controller = new AbortController(); let timer: ReturnType<typeof setTimeout>;
        const suspend = () => { controller.abort(); clearTimeout(timer); };
        availability.controller.signal.addEventListener('abort', suspend);
        async function poll() {
            if (controller.signal.aborted) return;
            try {
                const q = new URLSearchParams({ attempt_id: attempt, limit_bytes: '65536', ...(cursor.current ? { cursor: cursor.current } : {}) });
                const b = await api(`/organizations/${org}/runs/${run}/logs?${q}`, 'LogBatch', { signal: controller.signal });
                if (controller.signal.aborted) return;
                cursor.current = b.cursor;
                setError(''); setComplete(b.archive_complete); setMore(b.has_more);
                if (b.reset) { setReset(b.reset_reason ?? 'preview_reset'); setLimited(false); }
                setSegments(previous => { const next = appendWindow(b.reset ? [] : previous, b.segments); if (next.removed) setLimited(true); return next.segments; });
                // Continue archives in bounded batches; never a synchronous drain.
                timer = setTimeout(poll, b.has_more ? 150 : 2000);
            } catch (e) {
                if (controller.signal.aborted) return;
                setError(e instanceof Error ? e.message : 'Logs are unavailable.');
                if (e instanceof APIError && [401, 403, 404].includes(e.status)) {
                    cursor.current = ''; setSegments([]); setComplete(false); setMore(false); setLimited(false); setReset('');
                    onDenied();
                }
            }
        }
        void poll(); return () => { suspend(); availability.controller.signal.removeEventListener('abort', suspend); };
    }, [org, run, attempt, following, reload, onDenied, availability]);
    useEffect(() => { if (following && consoleWindow.current) consoleWindow.current.scrollTop = consoleWindow.current.scrollHeight; }, [segments, following]);
    // S6845 scroll-container exception: keep retained output keyboard-scrollable.
    return <section className="panel log-panel"><div className="row"><div><h2>Redacted console</h2><small>{complete ? 'Durable archive complete' : 'Durable archive incomplete or not yet finalized'}{more ? ' · continuing archive' : ''}</small></div><button aria-pressed={!following} onClick={() => setFollowing(v => !v)}>{following ? 'Pause' : 'Follow'}</button></div><p className="muted">One redacted stream per attempt. Hidden text cannot be recovered here.</p>{limited && <p className="notice">Display window limited to the latest 512 Ki characters / 500 segments. Earlier text was removed from browser memory; this does not mean durable evidence is missing. <button onClick={() => { cursor.current = ''; setSegments([]); setLimited(false); setFollowing(true); setReload(v => v + 1); }}>Read archive from start</button></p>}{reset && <p className="notice">Preview reset ({reset.replaceAll('_', ' ')}). Continuing from retained archive; preview continuity is unknown.</p>}{error && <div className="error"><p role="alert">{error}</p><button onClick={() => { setFollowing(true); setReload(v => v + 1); }}>Retry logs</button></div>}<div ref={consoleWindow} className="console" tabIndex={0} role="region" aria-label="Console output">{!segments.length && !error && <p>{following ? 'Waiting for retained logs or live preview…' : 'Log following paused.'}</p>}{segments.map((s, i) => 'gap' in s ? <p key={`${i}/${s.sequence}/${s.byte_start}`} className="log-gap">Gap: {s.gap.reason.replaceAll('_', ' ')} · {s.gap.unknown_extent ? 'unknown extent' : `${s.byte_start}–${s.byte_end} redacted bytes`}</p> : <pre key={`${i}/${s.sequence}/${s.byte_start}`}>{s.text}</pre>)}</div></section>;
}
