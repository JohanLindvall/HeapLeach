// SPDX-License-Identifier: MIT
// @vitest-environment jsdom
import { act } from 'react';
import { createRoot, type Root } from 'react-dom/client';
import { afterEach, beforeEach, expect, it, vi } from 'vitest';
import type { GroupLimit, Snapshot } from '../types';
import { GroupLimits } from './GroupLimits';

const base: Snapshot = {
  jobs: [], concurrency: 10, maxConcurrency: 256, streams: 4, maxStreams: 16,
  proxies: true, active: 0, queued: 0, speed: 0, paused: false, speedLimit: 0,
  downloadDir: '/tmp/downloads', diskFree: 0, diskTotal: 0, diskMinFree: 0, hostCount: 1, held: 0,
  groups: ['keep2share', 'site.example.test'], groupLimits: { keep2share: { files: 2 } },
};

let root: Root;
let host: HTMLDivElement;
let sent: Record<string, GroupLimit>[];

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

async function render(snapshot = base) {
  await act(async () => { root.render(<GroupLimits snapshot={snapshot} onChange={(groups) => { sent.push(groups); }} />); });
}

function input(label: string): HTMLInputElement {
  return host.querySelector<HTMLInputElement>(`input[aria-label="${label}"]`)!;
}

async function type(field: HTMLInputElement, text: string) {
  await act(async () => {
    const setter = Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, 'value')!.set!;
    setter.call(field, text);
    field.dispatchEvent(new Event('input', { bubbles: true }));
  });
  await act(async () => { field.focus(); field.blur(); });
}

it('shows each group with its own limits, the queue’s as placeholders', async () => {
  await render();
  expect(input('keep2share: files at once').value).toBe('2');
  expect(input('site.example.test: files at once').value).toBe('');
  expect(input('site.example.test: files at once').placeholder).toBe('10');
  expect(input('site.example.test: streams per file').placeholder).toBe('4');
});

it('sends the whole set when a group is held at zero files', async () => {
  await render();
  await type(input('site.example.test: files at once'), '0');
  expect(sent).toEqual([{ keep2share: { files: 2 }, 'site.example.test': { files: 0 } }]);
});

it('keeps a limit beneath the queue’s own', async () => {
  await render();
  await type(input('keep2share: streams per file'), '9');
  expect(sent).toEqual([{ keep2share: { files: 2, streams: 4 } }]);
});

it('clears a field left empty, and a group with none left', async () => {
  await render();
  await type(input('keep2share: files at once'), '');
  expect(sent).toEqual([{}]);
});

it('clears a group outright', async () => {
  await render();
  const clear = host.querySelector<HTMLButtonElement>('button[aria-label="Clear the limits for keep2share"]')!;
  await act(async () => { clear.click(); });
  expect(sent).toEqual([{}]);
});

it('says nothing when the queue has no groups', async () => {
  await render({ ...base, groups: [], groupLimits: {} });
  expect(host.textContent).toBe('');
});
