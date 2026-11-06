import { useState, type FormEvent } from 'react';
import { Link, useParams } from 'react-router';
import { Button } from '@astryxdesign/core/Button';
import { api } from '../lib/api';
export default function Account() {
    const { purpose } = useParams();
    const [error, setError] = useState(''), [busy, setBusy] = useState(false), [done, setDone] = useState(false);
    if (purpose !== 'setup' && purpose !== 'reset')
        return <main className="standalone"><h1>Page unavailable</h1><Link to="/login">Sign in</Link></main>;
    async function submit(event: FormEvent<HTMLFormElement>) { event.preventDefault(); const f = new FormData(event.currentTarget); if (f.get('password') !== f.get('confirm')) {
        setError('The passwords don’t match.');
        return;
    } setBusy(true); setError(''); try {
        await api('/account/' + purpose, 'OKBody', { method: 'POST', body: { code: f.get('code'), password: f.get('password') } });
        setDone(true);
    }
    catch (e) {
        setError(e instanceof Error ? e.message : 'Unable to complete setup.');
    }
    finally {
        setBusy(false);
    } }
    return <main className="setup-page"><Link className="brand" to="/login"><img src="/brand/logo-horizontal-color.svg" alt="Cosmoformer" width={1954} height={427}/></Link><section className="form-card panel">{done ? <><span className="eyebrow">YOU’RE ALL SET</span><h1>Password saved</h1><p>Your account permissions are unchanged. Sign in to continue.</p><Link className="button-link" to="/login">Go to sign in →</Link></> : <><span className="eyebrow">{purpose === 'setup' ? 'GET STARTED' : 'ACCOUNT RECOVERY'}</span><h1>{purpose === 'setup' ? 'Set up your account' : 'Reset your password'}</h1><p className="muted">Enter the one-use code your administrator shared privately.</p><form onSubmit={submit}><label>{purpose === 'setup' ? 'Setup code' : 'Reset code'}<input name="code" autoComplete="off" required minLength={43} maxLength={43} spellCheck={false}/></label><label>New password<input name="password" type="password" autoComplete="new-password" aria-describedby="password-help" required minLength={15} maxLength={256}/></label><small id="password-help">15–128 characters. Spaces and Unicode are welcome.</small><label>Confirm password<input name="confirm" type="password" autoComplete="new-password" required/></label>{error && <p role="alert" className="error">{error}</p>}<Button label={busy ? 'Saving…' : 'Save password'} type="submit" isDisabled={busy}/></form><Link to="/login">Back to sign in</Link></>}</section></main>;
}
