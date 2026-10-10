// SPDX-License-Identifier: MIT
/** Lifecycle state of a job or a single file. Mirrors download.Status in Go. */
export type Status =
  | 'resolving'
  | 'queued'
  | 'running'
  | 'done'
  | 'failed'
  | 'canceled';

/** One file inside a job. */
export interface ItemView {
  id: string;
  name: string;
  dir?: string;
  status: Status;
  /** Total length in bytes, or -1 when the host has not reported one. */
  size: number;
  downloaded: number;
  /** Bytes per second, smoothed server-side. Zero unless running. */
  speed: number;
  error?: string;
  /**
   * What the transfer is waiting for right now — a busy host being given
   * time, say. Transient, and cleared when the item finishes, so unlike an
   * error it describes the present rather than the outcome.
   */
  note?: string;
  path?: string;
  /** Connections currently fetching this file; absent or 1 when unsplit. */
  streams?: number;
  /** True when the file was already in the destination, so nothing moved. */
  skipped?: boolean;
  /**
   * Parts joined so far, and how many there are. A file arriving as a
   * playlist has no known byte total until the last part lands, so these are
   * the only honest progress it has.
   */
  segmentsDone?: number;
  segmentsTotal?: number;
}

/** One submitted URL and everything behind it. */
export interface JobView {
  /**
   * When set, `items` holds only the rows that changed since the last frame
   * and is to be merged into what is already held rather than replacing it.
   * See mergeSnapshot, which is the only thing that should ever read it.
   */
  patch?: boolean;
  id: string;
  source: string;
  title: string;
  host: string;
  status: Status;
  error?: string;
  createdAt: string;
  items: ItemView[];
  total: number;
  done: number;
  failed: number;
  canceled: number;
  active: number;
  size: number;
  downloaded: number;
  speed: number;
  /** False while any item's length is still unknown. */
  sizeKnown: boolean;
  /** Restored from the last run and waiting for a Resume or a retry. */
  held?: boolean;
}

/** Whole-application state, pushed over server-sent events. */
export interface Snapshot {
  jobs: JobView[];
  concurrency: number;
  maxConcurrency: number;
  /** Ceiling on connections one slow file may be split across. */
  streams: number;
  maxStreams: number;
  /** Proxy routing for free hosts and WAF challenge recovery on any host. */
  proxies: boolean;
  active: number;
  queued: number;
  speed: number;
  /** True while the whole queue is held. */
  paused: boolean;
  /** Ceiling on total throughput in bytes per second; 0 is unlimited. */
  speedLimit: number;
  downloadDir: string;
  /**
   * What may still be written at downloadDir, and the size of the filesystem
   * holding it. Both zero when the destination could not be measured — a
   * full disk reports zero free too, so only the total tells them apart.
   */
  diskFree: number;
  diskTotal: number;
  /**
   * Room that must be left before another transfer starts. Below it the
   * queue waits rather than filling the disk, and saying so is the only
   * thing that separates a held queue from a slow one.
   */
  diskMinFree: number;
  hostCount: number;
  /** Jobs restored from the last run and waiting for a Resume. */
  held: number;
  /** The server's build, e.g. "v1.2.62". */
  version?: string;
}

/** Result of submitting URLs. */
export interface AddResponse {
  accepted: { id: string; url: string }[];
  rejected: { url: string; error: string }[];
}

/** Connection state of the event stream. */
export type ConnectionState = 'connecting' | 'live' | 'offline';

export interface ProxyRow {
  id: string;
  url: string;
  source: 'direct' | 'manual' | 'discovered';
  status: 'available' | 'untested' | 'active' | 'busy' | 'cooling' | 'finishing';
  /** Estimated useful bytes/second; the mean of the selector's score. */
  score: number;
  throughput: number;
  currentSpeed: number;
  successRate: number;
  requests: number;
  wafRequests?: number;
  wafSuccess?: number;
  active: number;
  cooldownUntil?: string;
  lastSuccess?: string;
}

export interface ProxyPage {
  rows: ProxyRow[];
  total: number;
  offset: number;
  limit: number;
  summary: { total: number; available: number; active: number; cooling: number; untested: number };
  sources: { url: string; count: number; fetched?: string; refreshing: boolean; failed: boolean }[];
}
