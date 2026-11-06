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
    vi.stubGlobal('fetch', vi.fn(() => Promise.reject(new Error('Unexpected test request'))));
});
afterEach(() => {
    cleanup();
    vi.useRealTimers();
    vi.unstubAllGlobals();
    setCSRF(null);
});
