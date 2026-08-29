import { describe, expect, it } from 'vitest';
import { accumulate, EMPTY_PROGRESS, LEVELS, newlyUnlocked, standing, type Progress } from './gamification';
import type { ItemView, JobView, Snapshot } from './types';

function item(over: Partial<ItemView>): ItemView {
  return { id: 'i1', name: 'f.bin', status: 'done', size: 100, downloaded: 100, speed: 0, elapsed: 1, ...over };
}

function snapshot(items: ItemView[], over: Partial<Snapshot> = {}): Snapshot {
  const job: JobView = {
    id: 'j1', source: 'https://example.test/a', title: 'a', host: 'example.test',
    status: 'running', createdAt: '', items,
    total: items.length, done: 0, failed: 0, canceled: 0, active: 0,
    size: 0, downloaded: 0, speed: 0, sizeKnown: true,
  };
  return {
    jobs: [job], concurrency: 1, maxConcurrency: 4, streams: 1, maxStreams: 8,
    active: 0, queued: 0, speed: 0, paused: false, speedLimit: 0,
    downloadDir: '/tmp', diskFree: 0, diskTotal: 0, hostCount: 10, ...over,
  };
}

describe('accumulate', () => {
  it('counts a finished file once, however many snapshots repeat it', () => {
    const snap = snapshot([item({})]);
    const first = accumulate(EMPTY_PROGRESS, snap);
    expect(first.filesCompleted).toBe(1);
    expect(first.bytesDownloaded).toBe(100);

    // The server pushes the whole state every tick; replaying the same
    // snapshot must change nothing.
    const second = accumulate(first, snap);
    expect(second.filesCompleted).toBe(1);
    expect(second.bytesDownloaded).toBe(100);
  });

  it('ignores files that are not finished yet', () => {
    const p = accumulate(EMPTY_PROGRESS, snapshot([item({ status: 'running', downloaded: 50 })]));
    expect(p.filesCompleted).toBe(0);
    expect(p.bytesDownloaded).toBe(0);
  });

  it('credits the reported size when a skipped file moved no bytes', () => {
    const p = accumulate(EMPTY_PROGRESS, snapshot([item({ downloaded: 0, size: 77 })]));
    expect(p.bytesDownloaded).toBe(77);
  });

  it('forgets cleared jobs so the counted set cannot grow forever', () => {
    const first = accumulate(EMPTY_PROGRESS, snapshot([item({ id: 'gone' })]));
    expect(first.countedItems).toContain('gone');

    // The job was cleared: its id is no longer live, so it is dropped —
    // and re-adding it later counts as a fresh download.
    const second = accumulate(first, snapshot([item({ id: 'other' })]));
    expect(second.countedItems).not.toContain('gone');
    expect(second.filesCompleted).toBe(2);
  });

  it('tracks peaks rather than latest values', () => {
    const fast = accumulate(EMPTY_PROGRESS, snapshot([], { speed: 9000, active: 6 }));
    const slow = accumulate(fast, snapshot([], { speed: 100, active: 1 }));
    expect(slow.peakSpeed).toBe(9000);
    expect(slow.maxParallel).toBe(6);
  });
});

describe('standing', () => {
  it('starts at the first rank with everything still to do', () => {
    const s = standing(0);
    expect(s.level.index).toBe(1);
    expect(s.next?.index).toBe(2);
    expect(s.fraction).toBe(0);
  });

  it('sits exactly on a boundary at the rank it just reached', () => {
    const second = LEVELS[1]!;
    const s = standing(second.at);
    expect(s.level.index).toBe(2);
    expect(s.fraction).toBe(0);
  });

  it('caps out at the final rank', () => {
    const last = LEVELS[LEVELS.length - 1]!;
    const s = standing(last.at * 2);
    expect(s.level.index).toBe(last.index);
    expect(s.next).toBeNull();
    expect(s.fraction).toBe(1);
    expect(s.toNext).toBe(0);
  });
});

describe('newlyUnlocked', () => {
  it('reports an achievement once and never again', () => {
    const p: Progress = { ...EMPTY_PROGRESS, filesCompleted: 1 };
    const fresh = newlyUnlocked(p, 10);
    expect(fresh.map((a) => a.id)).toContain('first-blood');

    const recorded: Progress = { ...p, unlocked: fresh.map((a) => a.id) };
    expect(newlyUnlocked(recorded, 10)).toHaveLength(0);
  });

  it('never grants the every-host badge while no hosts exist to visit', () => {
    // hostCount 0 would otherwise satisfy "used >= supported" immediately.
    const p: Progress = { ...EMPTY_PROGRESS, hostsUsed: [] };
    expect(newlyUnlocked(p, 0).map((a) => a.id)).not.toContain('globetrotter');
  });
});
