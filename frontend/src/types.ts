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
  url?: string;
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
  /** Seconds since the transfer started. */
  elapsed: number;
}

/** One submitted URL and everything behind it. */
export interface JobView {
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
}

/** Whole-application state, pushed over server-sent events. */
export interface Snapshot {
  jobs: JobView[];
  concurrency: number;
  maxConcurrency: number;
  /** Ceiling on connections one slow file may be split across. */
  streams: number;
  maxStreams: number;
  active: number;
  queued: number;
  speed: number;
  /** True while the whole queue is held. */
  paused: boolean;
  /** Ceiling on total throughput in bytes per second; 0 is unlimited. */
  speedLimit: number;
  downloadDir: string;
  hostCount: number;
}

/** Result of submitting URLs. */
export interface AddResponse {
  accepted: { id: string; url: string }[];
  rejected: { url: string; error: string }[];
}

/** Connection state of the event stream. */
export type ConnectionState = 'connecting' | 'live' | 'offline';
