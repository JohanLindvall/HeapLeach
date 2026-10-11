// SPDX-License-Identifier: MIT
import { expect, it } from 'vitest';
import { formatLog, matchesLog, mergeLogs } from './logs';
import type { LogEntry, LogPage } from './types';

const entry = (id: number): LogEntry => ({ id, time: '2026-01-01T12:00:00Z', level: 'INFO', text: `msg=example job=job-${id}` });
const initial: LogPage = { session: 'first', entries: [entry(1), entry(2), entry(3)], oldest: 1, next: 3, limit: 3, reset: true };

it('merges deltas without duplicate or expired rows and caps the retained window', () => {
  const next = mergeLogs(initial, { ...initial, oldest: 3, next: 5, entries: [entry(3), entry(4), entry(5)], reset: false });
  expect(next.entries.map((row) => row.id)).toEqual([3, 4, 5]);
  const capped = mergeLogs(next, { ...next, next: 6, entries: [entry(6)], reset: false });
  expect(capped.entries.map((row) => row.id)).toEqual([4, 5, 6]);
  expect(initial.entries.map((row) => row.id)).toEqual([1, 2, 3]);
});

it('replaces history after a restart or retention gap and leaves unchanged polls stable', () => {
  expect(mergeLogs(initial, { ...initial, entries: [], reset: false })).toBe(initial);
  const restarted = { ...initial, session: 'second', next: 1, entries: [entry(1)] };
  expect(mergeLogs(initial, restarted)).toBe(restarted);
  const rotated = { ...initial, oldest: 8, next: 8, entries: [entry(8)] };
  expect(mergeLogs(initial, rotated)).toBe(rotated);
});

it('searches inherited fields, filters severity and marks truncated exports', () => {
  const message = { ...entry(1), level: 'WARN', truncated: true };
  expect(matchesLog(message, '  JOB-1  ', 4)).toBe(true);
  expect(matchesLog(message, 'job-1', 8)).toBe(false);
  expect(matchesLog({ ...message, level: 'INFO+2' }, '', 4)).toBe(false);
  expect(matchesLog({ ...message, level: 'ERROR+1' }, '', 8)).toBe(true);
  expect(formatLog(message)).toBe('time=2026-01-01T12:00:00Z level=WARN msg=example job=job-1 … [truncated]');
});
