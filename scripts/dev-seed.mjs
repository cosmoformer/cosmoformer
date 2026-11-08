import {randomBytes} from 'node:crypto';
import path from 'node:path';
import {dev, docker, child, privateFile} from './workflow-lib.mjs';
import {apiSocket, localRequest, validateSocket} from './local-api.mjs';

// Uses the same authentication, authorization and validation as the browser.
export class LocalClient {
    constructor(socketPath = apiSocket) {
        validateSocket(socketPath);
        this.socketPath = socketPath; this.cookie = ''; this.csrf = '';
    }
    async call(method, route, body) {
        const write = method !== 'GET';
        // GETs decide what is currently missing. Each new mutation needs a new
        // receipt, so revocation or renaming never replays an old creation.
        const key = randomBytes(32).toString('base64url');
        const response = await localRequest(this.socketPath, method, `/api/v1${route}`, write ? JSON.stringify(body ?? {}) : undefined,
            {Cookie: this.cookie, ...(write ? {'Content-Type': 'application/json', Origin: 'https://localhost:5173', 'X-Cosmoformer-Request': 'browser', 'X-CSRF-Token': this.csrf, 'Idempotency-Key': key} : {})});
        if (response.status !== 200) throw Error(`Local API ${method} ${route}: HTTP ${response.status}. Check sign-in, current permissions and local setup.`);
        const cookie = response.headers['set-cookie']?.find(item => item.startsWith('__Host-cosmoformer_session='));
        if (cookie) this.cookie = cookie.split(';')[0];
        return JSON.parse(response.text);
    }
    async login(username, password) {
        await this.call('POST', '/session/login', {username, password});
        const session = await this.call('GET', '/session'); this.csrf = session.csrf_token; return session;
    }
    async logout() { if (this.cookie && this.csrf) await this.call('POST', '/session/logout', {}); this.cookie = ''; this.csrf = ''; }
}

function named(items, name) {
    const matches = items.filter(item => item.name === name);
    if (matches.length > 1) throw Error(`Multiple sample records named ${name}; resolve them before seeding.`);
    return matches[0];
}
export async function seed(client, actor) {
    const ensure = async (items, name, route) => named(items, name) ?? await client.call('POST', route, {name});
    const org = await ensure((await client.call('GET', '/organizations')).organizations, 'Local demo', '/organizations');
    const base = `/organizations/${org.id}`;
    const project = await ensure((await client.call('GET', `${base}/projects`)).projects, 'Sandbox', `${base}/projects`);
    // Refresh the project list because create responses contain only an ID.
    const projects = (await client.call('GET', `${base}/projects`)).projects;
    const current = projects.find(item => item.id === project.id);
    const environment = await ensure(current.environments, 'Development', `${base}/projects/${project.id}/environments`);
    let access = await client.call('GET', `${base}/access`);
    const team = await ensure(access.teams, 'Local developers', `${base}/teams`);
    access = await client.call('GET', `${base}/access`);
    if (!access.teams.find(item => item.id === team.id).members.includes(actor.id)) await client.call('POST', `${base}/teams/${team.id}/members`, {account_id: actor.id});
    if (!access.grants.some(grant => grant.team_id === team.id && grant.project_id === project.id && grant.environment_id === environment.id && grant.role === 'planner')) {
        await client.call('POST', `${base}/grants`, {team_id: team.id, project_id: project.id, environment_id: environment.id, role: 'planner'});
    }
    return {organization: org.id, project: project.id, environment: environment.id, team: team.id};
}

if (import.meta.main) {
    let client;
    try {
        client = new LocalClient();
        const container = privateFile(path.join(dev, 'container')).trim();
        const label = await child(docker, ['inspect', '--format', '{{index .Config.Labels "cosmoformer.local"}}', container], {quiet: true}).done;
        if (label.code || label.output.trim() !== 'dev') throw Error('Expected the recorded local development database. Run make dev-setup.');
        const dsn = new URL(privateFile(path.join(dev, 'app.dsn')));
        if (dsn.hostname !== '127.0.0.1' || dsn.port !== '54329' || dsn.pathname !== '/cosmoformer') throw Error('Seeding requires the standard isolated local development database.');
        const file = process.env.SEED_PASSWORD_FILE ?? path.join(dev, 'seed-password');
        let password;
        try { password = privateFile(file).replace(/\r?\n$/, ''); } catch { throw Error('Save your chosen local admin password in an owner-only file and pass SEED_PASSWORD_FILE=/absolute/path. No default password is supplied.'); }
        const session = await client.login(process.env.SEED_USERNAME ?? 'admin', password);
        if (!session.actor.installation_admin) throw Error('Seeding requires the local installation administrator.');
        await seed(client, session.actor);
        console.log('Local demo is ready: Sandbox / Development, Local developers team, and environment Planner access for the signed-in account.');
    } catch (error) { console.error(error.message); process.exitCode = 1; }
    finally { await client?.logout().catch(() => { console.error('Local seed session logout failed; its normal session expiry still applies.'); process.exitCode = 1; }); }
}
