// SPDX-License-Identifier: MIT
import type { LogEntry, LogPage } from './types';

/** Drop expired rows even when a byte limit retains fewer than the row cap. */
export function mergeLogs(previous: LogPage | null, page: LogPage): LogPage {
  if (!previous || page.reset || previous.session !== page.session) return page;
  if (previous.next === page.next && previous.oldest === page.oldest && page.entries.length === 0) return previous;
  const entries = new Map(previous.entries
    .filter((entry) => entry.id >= page.oldest && entry.id <= page.next)
    .map((entry) => [entry.id, entry]));
  for (const entry of page.entries) entries.set(entry.id, entry);
  return { ...page, entries: [...entries.values()].sort((a, b) => a.id - b.id).slice(-page.limit) };
}

export function logSeverity(level: string): number {
  const match = /^(DEBUG|INFO|WARN|ERROR)([+-]\d+)?$/.exec(level);
  if (!match) return 0;
  const base: Record<string, number> = { DEBUG: -4, INFO: 0, WARN: 4, ERROR: 8 };
  return base[match[1]!]! + Number(match[2] ?? 0);
}

export function matchesLog(entry: LogEntry, query: string, minimum: number): boolean {
  return logSeverity(entry.level) >= minimum &&
    `${entry.time} ${entry.level} ${entry.text}`.toLowerCase().includes(query.trim().toLowerCase());
}

export function formatLog(entry: LogEntry): string {
  return `${entry.time ? `time=${entry.time} ` : ''}level=${entry.level} ${entry.text}${entry.truncated ? ' … [truncated]' : ''}`;
}
