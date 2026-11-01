// SPDX-License-Identifier: MIT
// Shared by the tests; nothing in the application imports it.
import { act } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { afterEach, beforeEach, vi } from 'vitest';

/** The element the component under test renders into, and its React root. */
export let host: HTMLDivElement;
export let root: Root;

/**
 * Gives each test of the file a fresh host and root, in React's act
 * environment, and unmounts them after it, undoing every stubbed global.
 * Called before the file's own beforeEach, which can then render.
 */
export function mountEachTest(): void {
  beforeEach(() => {
    vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true);
    host = document.createElement('div');
    document.body.append(host);
    root = createRoot(host);
  });
  afterEach(async () => {
    await act(async () => { root.unmount(); });
    host.remove();
    vi.unstubAllGlobals();
  });
}

/** A promise a test settles itself, to hold a request open. */
export function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}
