// SPDX-License-Identifier: MIT
// @vitest-environment jsdom
import { act } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { addUrls, ApiError } from '../api';
import type { AddResponse } from '../types';

const { readText } = vi.hoisted(() => {
  const readText = vi.fn<() => Promise<string>>();
  Object.defineProperty(navigator, 'clipboard', { value: { readText }, configurable: true });
  return { readText };
});
vi.mock('../api', async (original) => ({ ...await original<typeof import('../api')>(), addUrls: vi.fn() }));
import { AddForm } from './AddForm';

let root: Root;
let host: HTMLDivElement;
const notice = vi.fn();

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((yes) => { resolve = yes; });
  return { promise, resolve };
}

beforeEach(async () => {
  vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true);
  vi.mocked(addUrls).mockReset();
  readText.mockReset();
  notice.mockReset();
  host = document.createElement('div');
  document.body.append(host);
  root = createRoot(host);
  await act(async () => { root.render(<AddForm onNotice={notice} />); });
});

afterEach(async () => {
  await act(async () => { root.unmount(); });
  host.remove();
  vi.unstubAllGlobals();
});

function field(): HTMLTextAreaElement { return host.querySelector('textarea')!; }

async function type(text: string): Promise<void> {
  await act(async () => {
    Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value')!.set!.call(field(), text);
    field().dispatchEvent(new Event('input', { bubbles: true }));
  });
}

function submit(): void {
  host.querySelector('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true }));
}

it('keeps rejected links after partial success', async () => {
  vi.mocked(addUrls).mockResolvedValue({
    accepted: [{ id: 'job', url: 'https://example.test/first' }],
    rejected: [{ url: 'ftp://example.test/second', error: 'unsupported scheme' }],
  });
  await type('https://example.test/first\nftp://example.test/second');
  await act(async () => { submit(); });
  expect(field().value).toBe('ftp://example.test/second');
  expect(notice).toHaveBeenCalledWith(expect.stringContaining('unsupported scheme'), 'error');
});

it('preserves edits made during a request and prevents duplicate submissions', async () => {
  const pending = deferred<AddResponse>();
  vi.mocked(addUrls).mockReturnValue(pending.promise);
  await type('https://example.test/first');
  await act(async () => { submit(); submit(); });
  expect(addUrls).toHaveBeenCalledTimes(1);
  await type('https://example.test/next');
  await act(async () => { pending.resolve({ accepted: [{ id: 'job', url: 'https://example.test/first' }], rejected: [] }); });
  expect(field().value).toBe('https://example.test/next');
});

it('keeps the whole draft when the server rejects the request', async () => {
  vi.mocked(addUrls).mockRejectedValue(new ApiError('unsupported scheme', 400));
  await type('ftp://example.test/first');
  await act(async () => { submit(); });
  expect(field().value).toBe('ftp://example.test/first');
  expect(notice).toHaveBeenCalledWith('unsupported scheme', 'error');
});

it('locks submission while clipboard permission is pending and preserves the draft', async () => {
  const clipboard = deferred<string>();
  readText.mockReturnValue(clipboard.promise);
  vi.mocked(addUrls).mockResolvedValue({ accepted: [{ id: 'job', url: 'https://example.test/copied' }], rejected: [] });
  await type('https://example.test/draft');
  const button = [...host.querySelectorAll('button')].find((b) => b.textContent?.includes('Clipboard'))!;
  await act(async () => { button.click(); submit(); });
  expect(readText).toHaveBeenCalledTimes(1);
  expect(addUrls).not.toHaveBeenCalled();
  await act(async () => { clipboard.resolve('https://example.test/copied'); });
  expect(addUrls).toHaveBeenCalledExactlyOnceWith('https://example.test/copied', '');
  expect(field().value).toBe('https://example.test/draft');
});
