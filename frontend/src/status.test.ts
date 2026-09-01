import { describe, expect, it } from 'vitest';
import {
  countByFilter,
  isActive,
  isRetryable,
  isTerminal,
  matchesFilter,
  matchesQuery,
} from './status';
import type { JobView, Status } from './types';

const ALL: Status[] = ['resolving', 'queued', 'running', 'done', 'failed', 'canceled'];

function job(status: Status, extra: Partial<JobView> = {}): JobView {
  return {
    id: status,
    source: 'https://example.test/' + status,
    title: 'Album ' + status,
    host: 'example',
    status,
    createdAt: '2026-01-01T00:00:00Z',
    items: [],
    total: 0,
    done: 0,
    failed: 0,
    canceled: 0,
    active: 0,
    size: 0,
    downloaded: 0,
    speed: 0,
    sizeKnown: true,
    ...extra,
  };
}

describe('status predicates', () => {
  it('partition every status into active or terminal, never both', () => {
    for (const status of ALL) {
      expect(isActive(status) !== isTerminal(status), status).toBe(true);
    }
  });

  it('only allow a retry of something that stopped short', () => {
    expect(ALL.filter(isRetryable)).toEqual(['failed', 'canceled']);
  });
});

describe('matchesFilter', () => {
  it('shows everything under "all"', () => {
    for (const status of ALL) expect(matchesFilter(job(status), 'all')).toBe(true);
  });

  it('files each job under exactly one of the other groups', () => {
    for (const status of ALL) {
      const groups = (['active', 'done', 'failed'] as const).filter((f) =>
        matchesFilter(job(status), f),
      );
      expect(groups, status).toHaveLength(1);
    }
  });
});

describe('countByFilter', () => {
  it('counts in one pass what the filters would show', () => {
    const jobs = ALL.map((status) => job(status));
    expect(countByFilter(jobs)).toEqual({ all: 6, active: 3, done: 1, failed: 2 });
  });

  it('is all zeroes for no jobs', () => {
    expect(countByFilter([])).toEqual({ all: 0, active: 0, done: 0, failed: 0 });
  });
});

describe('matchesQuery', () => {
  const j = job('done', {
    title: 'Summer Mixtape',
    source: 'https://files.example.test/d/AbCd',
    host: 'example',
    items: [
      {
        id: 'i1',
        name: 'First Song.mp3',
        status: 'done',
        size: 1,
        downloaded: 1,
        speed: 0,
        elapsed: 1,
      },
    ],
  });

  it('matches everything with an empty or blank query', () => {
    expect(matchesQuery(j, '')).toBe(true);
    expect(matchesQuery(j, '   ')).toBe(true);
  });

  it('matches the title, source, host and file names, ignoring case', () => {
    expect(matchesQuery(j, 'MIXTAPE')).toBe(true);
    expect(matchesQuery(j, 'files.example.test')).toBe(true);
    expect(matchesQuery(j, 'Example')).toBe(true);
    expect(matchesQuery(j, 'first song')).toBe(true);
  });

  it('rejects what appears nowhere', () => {
    expect(matchesQuery(j, 'winter')).toBe(false);
  });
});
