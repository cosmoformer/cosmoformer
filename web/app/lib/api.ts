import type { components } from '../.generated/api';
import * as validators from '../.generated/validators.js';
export type Schema = components['schemas'];
let csrf: string | null = null;
export function setCSRF(value: string | null) { csrf = value; }
export class APIError extends Error {
    constructor(public status: number, public code: string, message: string) { super(message); }
}
const known = new Set(['INVALID_REQUEST', 'SIGN_IN_REQUIRED', 'FORBIDDEN', 'NOT_FOUND', 'CONFLICT', 'INVALID_INPUT', 'TRY_AGAIN', 'UNAVAILABLE', 'RECONFIRM_SECRET_WRITE', 'PLAN_NOT_READY', 'PLAN_EXPIRED', 'PLAN_INVALIDATED', 'PLAN_IDENTITY_CHANGED', 'RECOVERY_REQUIRED', 'INPUTS_CHANGED', 'STATE_OR_RECOVERY_CHANGED', 'PROFILE_UNSUPPORTED', 'EVIDENCE_UNAVAILABLE', 'SOURCE_CHANGED', 'SOURCE_VERIFICATION_STALE', 'SOURCE_UNAVAILABLE', 'CLOCK_UNRELIABLE', 'MAINTENANCE', 'INDEPENDENT_STOP_PROOF_REQUIRED', 'RECOVERY_EVIDENCE_CHANGED']);
export async function api<K extends keyof Schema>(path: string, schema: K, options: {
    method?: string;
    body?: unknown;
    signal?: AbortSignal;
} = {}): Promise<Schema[K]> {
    const method = options.method ?? 'GET';
    const mutation = method !== 'GET';
    const headers: Record<string, string> = { 'Accept': 'application/json' };
    if (mutation) {
        headers['Content-Type'] = 'application/json';
        headers['X-Cosmoformer-Request'] = 'browser';
        headers['Idempotency-Key'] = crypto.randomUUID();
        if (csrf)
            headers['X-CSRF-Token'] = csrf;
    }
    const response = await fetch('/api/v1' + path, { method, headers, credentials: 'same-origin', cache: 'no-store', body: mutation ? JSON.stringify(options.body ?? {}) : undefined, signal: options.signal });
    if (response.status === 401 && !options.signal?.aborted) {
        setCSRF(null);
        window.dispatchEvent(new Event('cosmoformer:expired'));
    }
    let value: unknown;
    try {
        value = await response.json();
    }
    catch {
        throw new APIError(response.status, 'UNAVAILABLE', 'The service returned an unexpected response.');
    }
    if (!response.ok) {
        const p = (value !== null && typeof value === 'object' ? value : {}) as {
            code?: string;
            message?: string;
        };
        const code = typeof p.code === 'string' && known.has(p.code) ? p.code : 'UNAVAILABLE';
        throw new APIError(response.status, code, typeof p.code === 'string' && known.has(p.code) && typeof p.message === 'string' ? p.message : 'The request could not be completed.');
    }
    const validate = (validators as Record<string, (v: unknown) => boolean>)[schema];
    if (!validate || !validate(value))
        throw new APIError(503, 'UNAVAILABLE', 'The service returned an unexpected response. Refresh and try again.');
    return value as Schema[K];
}
