// SPDX-License-Identifier: MIT
// @vitest-environment jsdom
import { act } from 'react';
import { beforeEach, expect, it } from 'vitest';
import { host, mountEachTest, root } from '../testkit';
import { formatSpeed } from '../format';
import type { JobLimit, JobView } from '../types';
import { JobCard } from './JobCard';
import type { QueueLimits } from './JobLimits';

const job: JobView = {
  id: 'job-1', source: 'https://site.example.test/album', title: 'An Album', host: 'site', status: 'queued',
  items: [], total: 0, done: 0, failed: 0, canceled: 0, active: 0,
  size: 0, downloaded: 0, speed: 0, sizeKnown: true,
};
const queue: QueueLimits = { files: 10, streams: 4, speed: 0 };
const noop = () => {};

let sent: [string, JobLimit][];

mountEachTest();

beforeEach(() => {
  sent = [];
});

async function render(view: JobView) {
  await act(async () => {
    root.render(
      <JobCard job={view} onCancel={noop} onRetry={noop} onRemove={noop} onCancelItem={noop} onRetryItem={noop}
        onLimit={(id, limit) => { sent.push([id, limit]); }} queue={queue} />,
    );
  });
}

function tags(): string[] {
  return [...host.querySelectorAll('.tag')].map((tag) => tag.textContent ?? '');
}

it('says which limits a job holds without opening them', async () => {
  await render({ ...job, fileLimit: 0, streamLimit: 1, speedLimit: 1e6 });
  // Held with nothing running, paused is the job's state, in its first tags.
  expect(tags().slice(0, 3)).toContain('paused');
  expect(tags()).not.toContain('queued');
  expect(tags()).toEqual(expect.arrayContaining(['1 stream', `≤ ${formatSpeed(1e6)}`]));
  await render({ ...job, status: 'running', active: 1, fileLimit: 0 });
  expect(tags()).toEqual(expect.arrayContaining(['running', 'paused']));
  await render({ ...job, status: 'done', fileLimit: 0 });
  expect(tags()).not.toContain('paused');
  await render({ ...job, fileLimit: 2, streamLimit: 3 });
  expect(tags()).toEqual(expect.arrayContaining(['2 at a time', '3 streams']));
  await render(job);
  expect(tags().some((tag) => /at a time|stream|paused|≤/.test(tag))).toBe(false);
});

it('opens the job’s limits and sends a change for that job', async () => {
  await render({ ...job, streamLimit: 2 });
  const toggle = host.querySelector<HTMLButtonElement>('button[aria-controls="limits-job-1"]')!;
  expect(toggle.getAttribute('aria-expanded')).toBe('false');
  await act(async () => { toggle.click(); });
  expect(toggle.getAttribute('aria-expanded')).toBe('true');
  const files = host.querySelector<HTMLInputElement>('#limits-job-1-fileLimit')!;
  await act(async () => {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!.call(files, '1');
    files.dispatchEvent(new Event('input', { bubbles: true }));
  });
  await act(async () => { files.focus(); files.blur(); });
  expect(sent).toEqual([['job-1', { fileLimit: 1, streamLimit: 2 }]]);
});

it('offers a retry for a finished job whose listing a failure cut short', async () => {
  const retry = () => host.querySelector('button[title="Retry job"]');
  await render({ ...job, status: 'done' });
  expect(retry()).toBeNull();
  await render({ ...job, status: 'done', incomplete: true });
  expect(retry()).not.toBeNull();
  // Not while it is still running: the retry is for what is left out.
  await render({ ...job, status: 'running', active: 1, incomplete: true });
  expect(retry()).toBeNull();
});
