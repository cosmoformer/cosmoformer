import { createContext, useCallback, useContext, useEffect, useRef, useState, type ReactNode } from 'react';
import { api, setCSRF, type Schema, APIError } from './api';

type Session = Schema['SessionOutputBody'];
const Context = createContext<{
    session: Session | null;
    loading: boolean;
    error: string;
    refresh: () => Promise<void>;
}>({ session: null, loading: true, error: '', refresh: async () => {} });

export function SessionProvider({ children }: { children: ReactNode }) {
    const [session, setSession] = useState<Session | null>(null);
    const [loading, setLoading] = useState(true);
    const [error, setError] = useState('');
    const lastActivity = useRef(0), generation = useRef(0);

    // Expiry and newer refreshes invalidate older responses, including CSRF state.
    const expire = useCallback(() => {
        generation.current++;
        setSession(null);
        setCSRF(null);
        setLoading(false);
        setError('');
    }, []);
    const refresh = useCallback(async () => {
        const current = ++generation.current;
        try {
            const s = await api('/session', 'SessionOutputBody');
            if (current !== generation.current) return;
            setCSRF(s.csrf_token);
            setSession(s);
            setError('');
        } catch (e) {
            if (current !== generation.current) return;
            setSession(null);
            setCSRF(null);
            setError(e instanceof APIError && e.status === 401 ? '' : e instanceof Error ? e.message : 'Unable to connect.');
        } finally {
            if (current === generation.current) setLoading(false);
        }
    }, []);
    useEffect(() => {
        void refresh();
        window.addEventListener('cosmoformer:expired', expire);
        return () => {
            generation.current++;
            window.removeEventListener('cosmoformer:expired', expire);
        };
    }, [refresh, expire]);
    useEffect(() => {
        if (!session) return;
        const activity = (event: Event) => {
            if (!event.isTrusted || Date.now() - lastActivity.current < 60000) return;
            lastActivity.current = Date.now();
            const current = generation.current;
            void api('/session/activity', 'OKBody', { method: 'POST' })
                .then(() => { if (current === generation.current) return refresh(); })
                .catch(() => {});
        };
        window.addEventListener('pointerdown', activity);
        window.addEventListener('keydown', activity);
        const expiry = Date.parse(session.expires_at);
        const timer = setTimeout(expire, Math.max(0, expiry - Date.now()));
        return () => {
            window.removeEventListener('pointerdown', activity);
            window.removeEventListener('keydown', activity);
            clearTimeout(timer);
        };
    }, [session, refresh, expire]);
    return <Context.Provider value={{ session, loading, error, refresh }}>{children}</Context.Provider>;
}
export function useSession() { return useContext(Context); }
