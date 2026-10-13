// SPDX-License-Identifier: MIT
// @vitest-environment jsdom
import { act } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import { formatSpeed } from '../format';
import type { JobLimit } from '../types';
import { JobLimits, type QueueLimits } from './JobLimits';

const queue: QueueLimits = { files: 10, streams: 4, speed: 0 };

let root: Root;
let host: HTMLDivElement;
let sent: JobLimit[];

beforeEach(() => {
  vi.stubGlobal('IS_REACT_ACT_ENVIRONMENT', true);
  sent = [];
  host = document.createElement('div');
  document.body.append(host);
  root = createRoot(host);
});

afterEach(async () => {
  await act(async () => { root.unmount(); });
  host.remove();
  vi.unstubAllGlobals();
});

async function render(limit: JobLimit = { fileLimit: 2 }, limits = queue) {
  await act(async () => {
    root.render(<JobLimits id="limits-job" limit={limit} queue={limits} onChange={(next) => { sent.push(next); }} />);
  });
}

function field<T extends HTMLElement = HTMLInputElement>(name: string): T {
  return host.querySelector<T>(`#limits-job-${name}`)!;
}

async function type(input: HTMLInputElement, text: string) {
  await act(async () => {
    Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!.call(input, text);
    input.dispatchEvent(new Event('input', { bubbles: true }));
  });
  await act(async () => { input.focus(); input.blur(); });
}

async function choose(select: HTMLSelectElement, value: number) {
  await act(async () => {
    Object.getOwnPropertyDescriptor(HTMLSelectElement.prototype, 'value')!.set!.call(select, String(value));
    select.dispatchEvent(new Event('change', { bubbles: true }));
  });
}

it('shows the job’s own limits, the queue’s as placeholders', async () => {
  await render();
  expect(field('fileLimit').value).toBe('2');
  expect(field('streamLimit').value).toBe('');
  expect(field('fileLimit').placeholder).toBe('10');
  expect(field('streamLimit').placeholder).toBe('4');
});

it('sends the whole limit when the job is held at zero files', async () => {
  await render({ streamLimit: 3 });
  await type(field('fileLimit'), '0');
  expect(sent).toEqual([{ fileLimit: 0, streamLimit: 3 }]);
});

it('keeps a limit beneath the queue’s own', async () => {
  await render();
  await type(field('streamLimit'), '9');
  expect(sent).toEqual([{ fileLimit: 2, streamLimit: 4 }]);
});

it('clears a field left empty, and sends nothing for what did not change', async () => {
  await render({ fileLimit: 2, speedLimit: 1e6 });
  await type(field('streamLimit'), '');
  await type(field('fileLimit'), '');
  expect(sent).toEqual([{ speedLimit: 1e6 }]);
});

it('offers only speeds beneath the queue’s, and sends the one chosen', async () => {
  await render({}, { ...queue, speed: 2e6 });
  const select = field<HTMLSelectElement>('speedLimit');
  expect([...select.options].map((option) => Number(option.value))).toEqual([0, 256 * 1024, 512 * 1024, 1e6]);
  expect(select.options[0]!.textContent).toBe(`Queue’s (${formatSpeed(2e6)})`);
  await choose(select, 1e6);
  expect(sent).toEqual([{ speedLimit: 1e6 }]);
});

it('keeps showing a speed that is not one it offers', async () => {
  await render({ speedLimit: 3e6 }, { ...queue, speed: 2e6 });
  expect(field<HTMLSelectElement>('speedLimit').value).toBe('3000000');
});

it('clears the job’s limits outright, and offers no Clear without any', async () => {
  await render({ fileLimit: 0, speedLimit: 1e6 });
  const clear = [...host.querySelectorAll('button')].find((button) => button.textContent === 'Clear')!;
  await act(async () => { clear.click(); });
  expect(sent).toEqual([{}]);
  await render({});
  expect(host.querySelectorAll('button')).toHaveLength(0);
});
