import { useEffect, useRef, useState } from 'react';
import { api, APIError, type Schema } from './api';

function pollingAvailable() {
    return (typeof document === 'undefined' || document.visibilityState !== 'hidden') && (typeof navigator === 'undefined' || navigator.onLine !== false);
}

// Abort immediately on suspension, including responses that settle before React
// runs effect cleanup. A new controller also fences a rapid hide/show transition.
export function usePollingAvailability() {
    const [availability, setAvailability] = useState(() => ({ active: pollingAvailable(), controller: new AbortController() }));
    const current = useRef(availability);
    useEffect(() => {
        const update = () => {
            const active = pollingAvailable();
            if (active === current.current.active) return;
            current.current.controller.abort();
            const next = { active, controller: new AbortController() };
            current.current = next;
            setAvailability(next);
        };
        document.addEventListener('visibilitychange', update);
        window.addEventListener('online', update);
        window.addEventListener('offline', update);
        update();
        return () => {
            document.removeEventListener('visibilitychange', update);
            window.removeEventListener('online', update);
            window.removeEventListener('offline', update);
        };
    }, []);
    return availability;
}

// One bounded request at a time. Changing scope immediately hides the old value;
// abort and identity checks also fence servers/mocks that ignore cancellation.
export function useRead<K extends keyof Schema>(path: string, schema: K, interval = 0) {
    const [state, setState] = useState<{ path: string; data?: Schema[K]; error: string; denied?: boolean }>({ path, error: '' });
    const [revision, reload] = useState(0);
    const availability = usePollingAvailability();
    useEffect(() => {
        if (!availability.active || availability.controller.signal.aborted) return;
        const controller = new AbortController();
        let timer: ReturnType<typeof setTimeout>;
        const suspend = () => { controller.abort(); clearTimeout(timer); };
        availability.controller.signal.addEventListener('abort', suspend);
        setState(previous => ({ path, data: previous.path === path ? previous.data : undefined, error: '' }));
        async function read() {
            if (controller.signal.aborted) return;
            try {
                const data = await api(path, schema, { signal: controller.signal });
                if (!controller.signal.aborted) setState({ path, data, error: '' });
            } catch (e) {
                if (!controller.signal.aborted) setState({ path, error: e instanceof Error ? e.message : 'Unable to load this page.', denied: e instanceof APIError && [403,404].includes(e.status) });
            } finally {
                if (!controller.signal.aborted && interval) timer = setTimeout(read, interval);
            }
        }
        void read();
        return () => { suspend(); availability.controller.signal.removeEventListener('abort', suspend); };
    }, [path, schema, interval, revision, availability]);
    return { denied: state.path === path && state.denied, data: state.path === path ? state.data : undefined, error: state.path === path ? state.error : '', reload: () => reload(v => v + 1) };
}
