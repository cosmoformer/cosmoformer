import { test as base, expect, type BrowserContext, type BrowserContextOptions } from '@playwright/test';
export async function monitorCSP(context: BrowserContext, violations: string[]) {
    await context.exposeBinding('__reportCosmoformerCSP', (_source, violation: string) => { violations.push(violation); });
    await context.addInitScript(() => {
        document.addEventListener('securitypolicyviolation', event => {
            (window as unknown as { __reportCosmoformerCSP: (value: string) => void }).__reportCosmoformerCSP(`${event.effectiveDirective}: ${event.blockedURI}`);
        });
    });
}
// The same journeys exercise the packaged build. Collect violations throughout
// reloads/navigation rather than inspecting only the final document.
export const test = base.extend<{
    packagedCSP: void;
    cspViolations: string[];
    newContext: (options?: BrowserContextOptions) => Promise<BrowserContext>;
}>({
    cspViolations: async ({}, use) => {
        const violations: string[] = [];
        await use(violations);
        expect(violations, 'CSP violations through packaged Nginx').toEqual([]);
    },
    packagedCSP: [async ({ context, cspViolations }, use) => {
        if (process.env.COSMOFORMER_PACKAGED_BROWSER === '1') await monitorCSP(context, cspViolations);
        await use();
    }, { auto: true }],
    newContext: async ({ browser, cspViolations }, use) => {
        const contexts: BrowserContext[] = [];
        try {
            await use(async options => {
                const context = await browser.newContext(options);
                contexts.push(context);
                if (process.env.COSMOFORMER_PACKAGED_BROWSER === '1') await monitorCSP(context, cspViolations);
                return context;
            });
        } finally { await Promise.all(contexts.map(context => context.close())); }
    },
});
export { expect };
export type { Page } from '@playwright/test';
