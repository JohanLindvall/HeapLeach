import { describe, expect, it } from 'vitest';
import { mergeSnapshot } from './snapshotMerge';
import type { ItemView, JobView, Snapshot } from './types';

function item(id: string, downloaded = 0): ItemView {
  return { id, name: `${id}.mp4`, status: 'queued', size: 100, downloaded, speed: 0 };
}

function snapshot(jobs: JobView[]): Snapshot {
  return {
    jobs,
    concurrency: 4,
    maxConcurrency: 32,
    streams: 8,
    maxStreams: 16,
    active: 0,
    queued: 0,
    paused: false,
    speedLimit: 0,
    speed: 0,
    downloadDir: '/tmp',
    diskFree: 0,
    diskTotal: 0,
    diskMinFree: 0,
    hostCount: 1,
  } as Snapshot;
}

function job(items: ItemView[], patch = false): JobView {
  return {
    id: 'j',
    title: 'Job',
    source: 'https://example.test/a',
    host: 'example',
    status: 'running',
    createdAt: '',
    items,
    total: items.length,
    done: 0,
    failed: 0,
    canceled: 0,
    active: 0,
    size: 0,
    downloaded: 0,
    speed: 0,
    sizeKnown: true,
    ...(patch ? { patch: true } : {}),
  } as JobView;
}

describe('mergeSnapshot', () => {
  it('takes a whole frame as it stands', () => {
    const frame = snapshot([job([item('a'), item('b')])]);
    expect(mergeSnapshot(null, frame)).toBe(frame);
  });

  // The point of the whole exercise: a frame says only what moved, and the
  // rest is filled in from what is already held.
  it('fills a patch back in from what is held', () => {
    const held = mergeSnapshot(null, snapshot([job([item('a'), item('b'), item('c')])]));
    const merged = mergeSnapshot(held, snapshot([job([item('b', 4096)], true)]));

    expect(merged.jobs[0].items.map((i) => i.id)).toEqual(['a', 'b', 'c']);
    expect(merged.jobs[0].items[1].downloaded).toBe(4096);
    // Order is the job's own, and a patch does not restate it: a known row
    // is replaced where it stands rather than moved to the end.
    expect(merged.jobs[0].items[0].id).toBe('a');
    expect(merged.jobs[0].items[2].id).toBe('c');
  });

  it('keeps everything when a patch carries nothing', () => {
    const held = mergeSnapshot(null, snapshot([job([item('a'), item('b')])]));
    const merged = mergeSnapshot(held, snapshot([job([], true)]));
    expect(merged.jobs[0].items.map((i) => i.id)).toEqual(['a', 'b']);
  });

  // React may replay a state updater, and a reconnect may repeat a frame.
  it('is idempotent, so a replayed frame changes nothing', () => {
    const held = mergeSnapshot(null, snapshot([job([item('a'), item('b')])]));
    const frame = snapshot([job([item('b', 512)], true)]);
    const once = mergeSnapshot(held, frame);
    const twice = mergeSnapshot(once, frame);
    expect(twice.jobs[0].items).toEqual(once.jobs[0].items);
  });

  it('nothing downstream is left holding a patch flag', () => {
    const held = mergeSnapshot(null, snapshot([job([item('a')])]));
    const merged = mergeSnapshot(held, snapshot([job([item('a', 1)], true)]));
    expect(merged.jobs[0].patch).toBeUndefined();
  });

  // A job that arrives whole replaces what was held, which is how a removal
  // reaches the browser at all.
  it('a whole job replaces what was held for it', () => {
    const held = mergeSnapshot(null, snapshot([job([item('a'), item('b'), item('c')])]));
    const merged = mergeSnapshot(held, snapshot([job([item('a')])]));
    expect(merged.jobs[0].items.map((i) => i.id)).toEqual(['a']);
  });
});
