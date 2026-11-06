import { useState, type FormEvent } from 'react';
import { Navigate, Link, useNavigate } from 'react-router';
import { Button } from '@astryxdesign/core/Button';
import { api } from '../lib/api';
import { useSession } from '../lib/session';
export default function Login() {
    const { session, loading, refresh } = useSession(), navigate = useNavigate();
    const [error, setError] = useState(''), [busy, setBusy] = useState(false);
    if (session)
        return <Navigate to="/organizations" replace/>;
    async function submit(event: FormEvent<HTMLFormElement>) { event.preventDefault(); const form = new FormData(event.currentTarget); setBusy(true); setError(''); try {
        await api('/session/login', 'LoginOutputBody', { method: 'POST', body: { username: form.get('username'), password: form.get('password') } });
        await refresh();
        navigate('/organizations', { replace: true });
    }
    catch (e) {
        setError(e instanceof Error ? e.message : 'Unable to sign in.');
    }
    finally {
        setBusy(false);
    } }
    return <main className="auth-page"><section className="auth-story"><Link className="brand" to="/"><img src="/brand/logo-horizontal-reversed.svg" alt="Cosmoformer" width={1954} height={427}/></Link><div><span className="eyebrow">YOUR INFRASTRUCTURE. YOUR SPACE.</span><h1>A clearer view.<br />A confident next step.</h1><p>One workspace for your team’s infrastructure.</p></div><small>Open source. Yours to run.</small></section><section className="auth-form"><div className="form-card"><span className="eyebrow">WELCOME BACK</span><h2>Sign in to your workspace</h2><p className="muted">Use your Cosmoformer account to continue.</p><form onSubmit={submit}><label>Username<input name="username" autoComplete="username" required maxLength={64}/></label><label>Password<input name="password" type="password" autoComplete="current-password" required maxLength={1024}/></label>{error && <p role="alert" className="error">{error}</p>}<Button variant="primary" className="auth-submit" label={busy ? 'Signing in…' : 'Sign in'} type="submit" isDisabled={busy || loading}/></form><p className="form-note">Joining for the first time? <Link to="/account/setup">Set up your account</Link></p><p className="form-note"><Link to="/account/reset">Use a password reset code</Link></p></div></section></main>;
}
