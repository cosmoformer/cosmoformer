import * as domMatchers from '@testing-library/jest-dom/matchers';
import type { TestingLibraryMatchers } from '@testing-library/jest-dom/matchers';
import { cleanup } from '@testing-library/react';
import { afterEach, beforeEach, expect, vi } from 'vitest';
import { setCSRF } from '../../app/lib/api';

// The packaged jest-dom adapter still declares Vitest's pre-v5 Assertion type.
// Register its standalone matchers through Vitest's supported extension interface.
declare module 'vitest' {
    interface Matchers<R extends void | Promise<void> = void | Promise<void>, T = unknown> extends TestingLibraryMatchers<T, R> {}
}
expect.extend(domMatchers);

beforeEach(() => {
    setCSRF(null);
    if (typeof HTMLDialogElement !== 'undefined') Object.defineProperty(HTMLDialogElement.prototype, 'showModal', { configurable: true, value: function (this: HTMLDialogElement) { this.setAttribute('open', ''); } });
    if (typeof HTMLDialogElement !== 'undefined') Object.defineProperty(HTMLDialogElement.prototype, 'close', { configurable: true, value: function (this: HTMLDialogElement) { this.removeAttribute('open'); } });
    vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new Error('Unexpected test request'))));
});
afterEach(() => {
    cleanup();
    vi.useRealTimers();
    vi.unstubAllGlobals();
    setCSRF(null);
});
