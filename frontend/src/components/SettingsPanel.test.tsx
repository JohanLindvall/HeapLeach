// SPDX-License-Identifier: MIT
// @vitest-environment jsdom
import { act } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { ApiError, fetchProxies, fetchSettings, updateSettings } from '../api';
import type { ProxyPage, Snapshot } from '../types';
import { SettingsPanel } from './SettingsPanel';

vi.mock('../api', async (original) => ({
  ...await original<typeof import('../api')>(),
  fetchProxies: vi.fn(), fetchSettings: vi.fn(), updateSettings: vi.fn(),
}));

const snapshot: Snapshot = {
  jobs: [], concurrency: 4, maxConcurrency: 32, streams: 2, maxStreams: 16,
  proxies: false, active: 0, queued: 0, speed: 0, paused: false, speedLimit: 0,
  downloadDir: '/tmp/downloads', diskFree: 0, diskTotal: 0, diskMinFree: 0, hostCount: 1, held: 0,
};

const inventory: ProxyPage = {
  rows: [{ id: 'route', url: 'http://proxy.example.test:8080', source: 'discovered', status: 'active',
    score: 1_000_000, throughput: 2_000_000, currentSpeed: 3_000_000, successRate: 0.9, requests: 17, active: 1,
    lastSuccess: '2026-01-01T12:00:00Z' }],
  total: 51, offset: 0, limit: 50,
  summary: { total: 51, available: 49, active: 1, cooling: 1, untested: 45 }, sources: [],
};

let root: Root;
let host: HTMLDivElement;

beforeEach(() => {
  vi.useFakeTimers();
  vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true);
  Object.defineProperty(document, 'hidden', { configurable: true, value: false });
  vi.mocked(fetchProxies).mockReset().mockResolvedValue(inventory);
  vi.mocked(fetchSettings).mockReset().mockResolvedValue({
    concurrency: 4, streams: 2, paused: false, speedLimit: 0, downloadDir: '/tmp/downloads',
    proxies: false, proxyEndpoints: ['direct'], proxyFeeds: ['https://feed.example.test/list'],
  });
  vi.mocked(updateSettings).mockReset().mockResolvedValue(snapshot);
  host = document.createElement('div');
  document.body.append(host);
  root = createRoot(host);
});

afterEach(async () => {
  await act(async () => { root.unmount(); });
  host.remove();
  vi.useRealTimers();
  vi.unstubAllGlobals();
});

async function render(value = snapshot) {
  await act(async () => { root.render(<SettingsPanel snapshot={value} />); });
}

function button(text: string): HTMLButtonElement {
  return [...host.querySelectorAll('button')].find((b) => b.textContent?.includes(text))!;
}

async function type(selector: string, value: string) {
  const field = host.querySelector<HTMLInputElement | HTMLTextAreaElement>(selector)!;
  await act(async () => {
    const prototype = field instanceof HTMLTextAreaElement ? HTMLTextAreaElement.prototype : HTMLInputElement.prototype;
    Object.getOwnPropertyDescriptor(prototype, 'value')!.set!.call(field, value);
    field.dispatchEvent(new Event('input', { bubbles: true }));
  });
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  let reject!: (reason: unknown) => void;
  const promise = new Promise<T>((yes, no) => { resolve = yes; reject = no; });
  return { promise, resolve, reject };
}

it('shows measured and live throughput, scoring and success, and pages on the server', async () => {
  await render();
  expect(host.textContent).toContain('1.0 MB/s');
  expect(host.textContent).toContain('2.0 MB/s');
  expect(host.textContent).toContain('3.0 MB/s now');
  expect(host.textContent).toContain('90%');
  expect(host.querySelector('tbody')!.textContent).toContain('17');
  expect(host.textContent).toContain('K2S · FileBoom · WAF recovery');
  expect(button('Previous').disabled).toBe(true);
  vi.mocked(fetchProxies).mockResolvedValue({ ...inventory, offset: 50 });
  await act(async () => { button('Next').click(); });
  expect(fetchProxies).toHaveBeenLastCalledWith(expect.objectContaining({ offset: 50 }), expect.any(AbortSignal));
  expect(button('Next').disabled).toBe(true);
  expect(host.textContent).toContain('51–51 of 51');
  const service = host.querySelector<HTMLSelectElement>('[aria-label="Proxy service"]')!;
  await act(async () => { service.value = 'fileboom'; service.dispatchEvent(new Event('change', { bubbles: true })); });
  expect(fetchProxies).toHaveBeenLastCalledWith(expect.objectContaining({ site: 'fileboom', offset: 0 }), expect.any(AbortSignal));
  await act(async () => { service.value = 'cloudflare'; service.dispatchEvent(new Event('change', { bubbles: true })); });
  expect(fetchProxies).toHaveBeenLastCalledWith(expect.objectContaining({ site: 'cloudflare', offset: 0 }), expect.any(AbortSignal));
  await type('input[type="search"]', 'other');
  await act(async () => { await vi.advanceTimersByTimeAsync(250); });
  expect(fetchProxies).toHaveBeenLastCalledWith(expect.objectContaining({ offset: 0, search: 'other' }), expect.any(AbortSignal));
});

it('keeps source drafts when a live save fails', async () => {
  await render();
  await act(async () => { button('Edit proxy sources').click(); });
  await type('#proxy-endpoints', ' direct\nhttp://new.example.test:8080\n');
  await type('#proxy-feeds', '');
  vi.mocked(updateSettings).mockRejectedValue(new ApiError('Proxy database is unavailable.', 400));
  await act(async () => { host.querySelector('form')!.dispatchEvent(new Event('submit', { bubbles: true, cancelable: true })); });
  expect(updateSettings).toHaveBeenCalledExactlyOnceWith({ proxyEndpoints: ['direct', 'http://new.example.test:8080'], proxyFeeds: [] });
  expect(host.querySelector<HTMLTextAreaElement>('#proxy-endpoints')!.value).toContain('new.example.test');
  expect(host.querySelector('[role="alert"]')!.textContent).toBe('Proxy database is unavailable.');
  expect(button('Save sources').disabled).toBe(false);
  vi.mocked(updateSettings).mockResolvedValue(snapshot);
  await act(async () => { button('Save sources').click(); });
  expect(host.querySelector('#proxy-source-editor')).toBeNull();
});

it('sends concurrency and proxy changes without replacing unrelated settings', async () => {
  await render();
  await type('#concurrency', '6');
  await act(async () => { host.querySelector('#concurrency')!.dispatchEvent(new KeyboardEvent('keyup', { key: 'ArrowRight', bubbles: true })); });
  expect(updateSettings).toHaveBeenLastCalledWith({ concurrency: 6 });
  await act(async () => { host.querySelector<HTMLInputElement>('[role="switch"]')!.click(); });
  expect(updateSettings).toHaveBeenLastCalledWith({ proxies: true });
  await render({ ...snapshot, concurrency: 6, proxies: true });
  expect(host.querySelector<HTMLInputElement>('[role="switch"]')!.checked).toBe(true);
  expect(host.querySelector<HTMLInputElement>('#concurrency')!.value).toBe('6');
});

it('prevents duplicate proxy toggles and reports a failed enable without showing it as enabled', async () => {
  await render();
  const pending = deferred<Snapshot>();
  vi.mocked(updateSettings).mockReturnValue(pending.promise);
  const toggle = host.querySelector<HTMLInputElement>('[role="switch"]')!;
  await act(async () => { toggle.click(); toggle.click(); });
  expect(toggle.disabled).toBe(true);
  expect(updateSettings).toHaveBeenCalledTimes(1);
  await act(async () => { pending.reject(new ApiError('Cannot open proxy database.', 400)); });
  expect(toggle.checked).toBe(false);
  expect(toggle.disabled).toBe(false);
  expect(host.querySelector('[role="alert"]')!.textContent).toBe('Cannot open proxy database.');
});

it('ignores obsolete list responses and polls only while the panel is visible', async () => {
  const old = deferred<ProxyPage>();
  vi.mocked(fetchProxies).mockReturnValueOnce(old.promise);
  await render();
  const sort = host.querySelector<HTMLSelectElement>('[aria-label="Sort proxies"]')!;
  await act(async () => { sort.value = 'throughput'; sort.dispatchEvent(new Event('change', { bubbles: true })); });
  await act(async () => { old.resolve({ ...inventory, rows: [], total: 0 }); });
  expect(host.querySelector('tbody')!.textContent).toContain('proxy.example.test');
  expect(vi.mocked(fetchProxies).mock.calls[0]![1]!.aborted).toBe(true);
  await act(async () => { await vi.advanceTimersByTimeAsync(5000); });
  expect(fetchProxies).toHaveBeenCalledTimes(3);
  Object.defineProperty(document, 'hidden', { configurable: true, value: true });
  await act(async () => { await vi.advanceTimersByTimeAsync(5000); });
  expect(fetchProxies).toHaveBeenCalledTimes(3);
  Object.defineProperty(document, 'hidden', { configurable: true, value: false });
  await act(async () => { document.dispatchEvent(new Event('visibilitychange')); });
  expect(fetchProxies).toHaveBeenCalledTimes(4);
  await act(async () => { root.render(null); });
  expect(vi.mocked(fetchProxies).mock.calls.at(-1)![1]!.aborted).toBe(true);
  await act(async () => { await vi.advanceTimersByTimeAsync(10000); });
  expect(fetchProxies).toHaveBeenCalledTimes(4);
});
