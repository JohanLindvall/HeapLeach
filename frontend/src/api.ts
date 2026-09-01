import type { AddResponse, Snapshot } from './types';

/** Error carrying the server's message for a failed API call. */
export class ApiError extends Error {
  readonly status: number;

  constructor(message: string, status: number) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const response = await fetch(path, {
    ...init,
    headers: init?.body ? { 'Content-Type': 'application/json' } : undefined,
  });

  const text = await response.text();
  let body: unknown = null;
  if (text) {
    try {
      body = JSON.parse(text) as unknown;
    } catch {
      body = null;
    }
  }

  if (!response.ok) {
    const message =
      body && typeof body === 'object' && 'error' in body
        ? String((body as { error: unknown }).error)
        : `request failed (${response.status})`;
    throw new ApiError(message, response.status);
  }
  return body as T;
}

/** Queue one or more URLs. Accepts newline- or space-separated input. */
export function addUrls(urls: string, password: string): Promise<AddResponse> {
  return request<AddResponse>('/api/downloads', {
    method: 'POST',
    body: JSON.stringify({ urls, password }),
  });
}

/** Fetch the current state; used as a fallback when SSE is unavailable. */
export function fetchState(): Promise<Snapshot> {
  return request<Snapshot>('/api/state');
}

/**
 * The runtime settings, each optional: a request carries only what changed,
 * and the server applies the fields it finds.
 */
export interface Settings {
  /** Parallel transfers. */
  concurrency: number;
  /** Connections one slow file may be split across. */
  streams: number;
  /** Hold or release the whole queue. */
  paused: boolean;
  /** Ceiling on total throughput in bytes per second; 0 lifts the cap. */
  speedLimit: number;
  /**
   * Where finished files are written. Transfers already running keep the
   * destination they started with — their path was settled when the transfer
   * began, and a part file that moved mid-flight could not be resumed.
   * Everything still queued goes to the new place.
   */
  downloadDir: string;
}

/** Change one or more settings; the answer is the state as it now stands. */
export function updateSettings(patch: Partial<Settings>): Promise<Snapshot> {
  return request<Snapshot>('/api/settings', {
    method: 'POST',
    body: JSON.stringify(patch),
  });
}

/** Forget every finished job. Files on disk are left alone. */
export function clearFinished(): Promise<{ removed: number }> {
  return request<{ removed: number }>('/api/clear', { method: 'POST' });
}

export function cancelJob(jobId: string): Promise<void> {
  return request<void>(`/api/jobs/${encodeURIComponent(jobId)}/cancel`, { method: 'POST' });
}

export function retryJob(jobId: string): Promise<void> {
  return request<void>(`/api/jobs/${encodeURIComponent(jobId)}/retry`, { method: 'POST' });
}

export function removeJob(jobId: string): Promise<void> {
  return request<void>(`/api/jobs/${encodeURIComponent(jobId)}`, { method: 'DELETE' });
}

export function cancelItem(jobId: string, itemId: string): Promise<void> {
  return request<void>(
    `/api/jobs/${encodeURIComponent(jobId)}/items/${encodeURIComponent(itemId)}/cancel`,
    { method: 'POST' },
  );
}

export function retryItem(jobId: string, itemId: string): Promise<void> {
  return request<void>(
    `/api/jobs/${encodeURIComponent(jobId)}/items/${encodeURIComponent(itemId)}/retry`,
    { method: 'POST' },
  );
}
