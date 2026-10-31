// SPDX-License-Identifier: MIT
// @vitest-environment jsdom
import { act } from 'react';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { host, mountEachTest, root } from '../testkit';
import { ApiError, fetchLogs } from '../api';
import type { LogEntry, LogPage } from '../types';
import { LogPanel } from './LogPanel';

vi.mock('../api', async (original) => ({ ...await original<typeof import('../api')>(), fetchLogs: vi.fn() }));

const entry = (id: number, text: string, level = 'INFO'): LogEntry => ({ id, time: '2026-01-01T12:00:00.123Z', text, level });
const initial: LogPage = { session: 'first', oldest: 1, next: 1, limit: 1000, reset: true, entries: [entry(1, 'Service ready')] };

mountEachTest();

beforeEach(() => {
  vi.useFakeTimers();
  Object.defineProperty(document, 'hidden', { configurable: true, value: false });
  vi.mocked(fetchLogs).mockReset().mockResolvedValue(initial);
});

afterEach(async () => {
  vi.useRealTimers();
});

const button = (text: string): HTMLButtonElement => [...host.querySelectorAll('button')].find((el) => el.textContent?.includes(text))!;
const messages = (): string => host.querySelector('[role="log"]')!.textContent!;

it('loads history, appends new messages, searches fields and treats logged markup as text', async () => {
  await act(async () => root.render(<LogPanel />));
  expect(messages()).toContain('Service ready');
  expect(host.querySelector('time')!.textContent).toMatch(/\d{2}:\d{2}:\d{2}\.123/);
  const warning = entry(2, 'job=example <img src=x onerror=alert(1)>', 'WARN');
  vi.mocked(fetchLogs).mockResolvedValue({ ...initial, entries: [warning], next: 2, reset: false });
  await act(async () => vi.advanceTimersByTimeAsync(1000));
  expect(fetchLogs).toHaveBeenLastCalledWith('first', 1, expect.any(AbortSignal));
  expect(messages()).toContain('Service ready');
  expect(messages()).toContain(warning.text);
  expect(host.querySelector('img')).toBeNull();
  const level = host.querySelector<HTMLSelectElement>('[aria-label="Log level"]')!;
  await act(async () => { level.value = '4'; level.dispatchEvent(new Event('change', { bubbles: true })); });
  expect(messages()).not.toContain('Service ready');
  expect(messages()).toContain(warning.text);
  const search = host.querySelector<HTMLInputElement>('input[type="search"]')!;
  await act(async () => {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!.call(search, 'missing');
    search.dispatchEvent(new Event('input', { bubbles: true }));
  });
  expect(messages()).toContain('No messages match');
  expect(button('Download logs').disabled).toBe(true);
});

it('freezes updates while paused and replaces stale history after a restart', async () => {
  await act(async () => root.render(<LogPanel />));
  await act(async () => button('Pause updates').click());
  await act(async () => vi.advanceTimersByTimeAsync(5000));
  expect(fetchLogs).toHaveBeenCalledTimes(1);
  expect(host.textContent).toContain('Updates paused');
  vi.mocked(fetchLogs).mockRejectedValue(new ApiError('Service restarting.', 503));
  await act(async () => button('Resume updates').click());
  expect(host.querySelector('[role="alert"]')!.textContent).toContain('Service restarting.');
  expect(messages()).toContain('Service ready');
  vi.mocked(fetchLogs).mockResolvedValue({ ...initial, session: 'second', entries: [entry(1, 'Restarted service')] });
  await act(async () => vi.advanceTimersByTimeAsync(1000));
  expect(messages()).toContain('Restarted service');
  expect(messages()).not.toContain('Service ready');
  expect(host.querySelector('[role="alert"]')).toBeNull();
});

it('aborts on pause and ignores the late result of the canceled request', async () => {
  let finish!: (page: LogPage) => void;
  vi.mocked(fetchLogs).mockImplementationOnce(() => new Promise((resolve) => { finish = resolve; }));
  await act(async () => root.render(<LogPanel />));
  const signal = vi.mocked(fetchLogs).mock.calls[0]![2]!;
  await act(async () => button('Pause updates').click());
  expect(signal.aborted).toBe(true);
  vi.mocked(fetchLogs).mockResolvedValue({ ...initial, entries: [entry(1, 'Fresh response')] });
  await act(async () => button('Resume updates').click());
  await act(async () => finish(initial));
  expect(messages()).toContain('Fresh response');
  expect(messages()).not.toContain('Service ready');
});

it('does not poll hidden tabs and resumes when visible', async () => {
  Object.defineProperty(document, 'hidden', { configurable: true, value: true });
  await act(async () => root.render(<LogPanel />));
  await act(async () => vi.advanceTimersByTimeAsync(3000));
  expect(fetchLogs).not.toHaveBeenCalled();
  Object.defineProperty(document, 'hidden', { configurable: true, value: false });
  await act(async () => document.dispatchEvent(new Event('visibilitychange')));
  expect(fetchLogs).toHaveBeenCalledTimes(1);
  expect(messages()).toContain('Service ready');
});
